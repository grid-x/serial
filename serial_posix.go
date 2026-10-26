//go:build darwin || linux || freebsd || openbsd || netbsd
// +build darwin linux freebsd openbsd netbsd

package serial

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// port implements Port interface.
//
// Reads and Writes go through f, i.e. Go's runtime poller (epoll/kqueue) instead of select(2),
// which only supports fds below FD_SETSIZE (1024): for a higher fd the kernel writes past the
// fixed-size fd_set on the stack, corrupting it. f also makes Close wake blocked Reads/Writes and
// keeps the fd open until they have returned, so its number cannot be reused under them.
type port struct {
	f *os.File
	// fd is f's descriptor, for termios and ioctl calls. f.Fd() is not used because it puts the
	// fd into blocking mode, which disables the poller.
	fd         int
	oldTermios *syscall.Termios

	timeout time.Duration

	// closeOnce makes a second Close wait until the first has closed the fd
	// (os.File.Close would return at once with an error).
	closeOnce sync.Once
}

const (
	rs485Enabled      = 1 << 0
	rs485RTSOnSend    = 1 << 1
	rs485RTSAfterSend = 1 << 2
	rs485RXDuringTX   = 1 << 4
	rs485Tiocs        = 0x542f
)

// rs485_ioctl_opts is used to configure RS485 options in the driver
type rs485_ioctl_opts struct {
	flags                 uint32
	delay_rts_before_send uint32
	delay_rts_after_send  uint32
	padding               [5]uint32
}

// Open connects to the given serial port.
func open(c *Config) (Port, error) {
	p := &port{}
	termios, err := newTermios(c)
	if err != nil {
		return nil, err
	}
	// See man termios(3).
	// O_NOCTTY: no controlling terminal.
	// O_NDELAY: no data carrier detect. It is O_NONBLOCK too, which the runtime poller needs.
	p.fd, err = syscall.Open(c.Address, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NDELAY|syscall.O_CLOEXEC, 0666)
	if err != nil {
		return nil, err
	}
	p.f = os.NewFile(uintptr(p.fd), c.Address)
	p.timeout = c.Timeout
	// Deadlines work only on fds the poller accepted; without it Reads could not time out or be
	// woken by Close. ptys are accepted; whether any serial driver is rejected is not verified.
	if err = p.f.SetDeadline(time.Time{}); err != nil {
		p.f.Close()
		return nil, fmt.Errorf("serial: %s is not supported by the runtime poller: %v", c.Address, err)
	}
	// Backup current termios to restore on closing.
	p.backupTermios()
	if err = p.setTermios(termios); err != nil {
		// No need to restore termios
		p.oldTermios = nil
		p.Close()
		return nil, err
	}
	if err = enableRS485(p.fd, &c.RS485); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Close closes the port. Blocked Reads and Writes return os.ErrClosed.
func (p *port) Close() (err error) {
	p.closeOnce.Do(func() {
		// The termios is restored only after f.Close has woken blocked Reads, so none of them can
		// see the old settings (with VMIN 0 a read without data returns 0, reported as a hangup).
		// f.Close closes p.fd, so it is restored through a duplicate.
		fd := -1
		if p.oldTermios != nil {
			if fd = dupCloexec(p.fd); fd < 0 {
				// No fd is free for the duplicate (e.g. EMFILE), so restore it on p.fd now; a
				// blocked Read may then report a hangup before f.Close wakes it.
				p.restoreTermios(p.fd)
			}
		}
		err = p.f.Close()
		if fd >= 0 {
			p.restoreTermios(fd)
			syscall.Close(fd)
		}
	})
	return
}

// dupCloexec returns a close-on-exec duplicate of fd, or -1 if that fails.
func dupCloexec(fd int) int {
	// F_DUPFD_CLOEXEC sets close-on-exec atomically, so a concurrent fork cannot inherit the dup.
	dup, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return -1
	}
	return int(dup)
}

// Read reads from serial port. Port must be opened before calling this method.
// It blocks until at least one byte is available or p.timeout expires (ErrTimeout), then returns
// what has arrived, which may be less than len(b): use io.ReadFull to read a whole frame.
func (p *port) Read(b []byte) (n int, err error) {
	if p.timeout > 0 {
		// Fails only on a closed port, which Read reports.
		p.f.SetReadDeadline(time.Now().Add(p.timeout))
	}
	n, err = p.f.Read(b)
	return n, p.ioErr(err)
}

// Write writes b to the serial port. With a timeout it waits for room in the output queue until
// all of b is written or the timeout expires (ErrTimeout). Without a timeout it makes a single
// attempt and never blocks: a full output queue fails with EAGAIN, a partial write with
// io.ErrShortWrite.
func (p *port) Write(b []byte) (n int, err error) {
	if p.timeout > 0 {
		// Fails only on a closed port, which Write reports.
		p.f.SetWriteDeadline(time.Now().Add(p.timeout))
		n, err = p.f.Write(b)
		return n, p.ioErr(err)
	}
	c, err := p.f.SyscallConn()
	if err != nil {
		return 0, err
	}
	// RawConn.Write keeps the fd open while the func runs; returning true skips the poller wait.
	if cerr := c.Write(func(fd uintptr) bool {
		for {
			n, err = syscall.Write(int(fd), b)
			if err != syscall.EINTR {
				return true
			}
		}
	}); cerr != nil {
		// without a deadline its only error is the port being closed
		return 0, os.ErrClosed
	}
	if n < 0 {
		n = 0
	}
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	return n, p.ioErr(err)
}

// ioErr converts a read/write error to the errors Read and Write return: ErrTimeout for an
// expired deadline, os.ErrClosed for a closed port, a hangupError for a device that went away,
// and otherwise the bare syscall.Errno (as before the port used os.File). These compare with ==,
// except a hangupError, which only matches with errors.Is. On a pty whose master was closed,
// reads return EOF and writes fail with EIO on linux and darwin; a removed real device may fail
// reads and writes with EIO, ENXIO ("device not configured") or ENODEV depending on the OS and
// driver (not verified on real hardware).
//
// A one-off EIO from a device that is still there is reported as a hangup too, so the caller
// reconnects; whether a real driver returns one is not verified. Probing the fd cannot tell them
// apart: tcgetattr still succeeds after a pty hangup on darwin. Callers that need to tell them
// apart can check the errno with errors.Is.
func (p *port) ioErr(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		err = pe.Err
	}
	switch err {
	case os.ErrDeadlineExceeded:
		return ErrTimeout
	case io.EOF, syscall.EIO, syscall.ENXIO, syscall.ENODEV:
		return &hangupError{name: p.f.Name(), err: err}
	}
	return err
}

// hangupError reports a device that went away. errors.Is matches it with io.ErrUnexpectedEOF,
// so callers that reconnect on EOF (like modbus) also reconnect on it, and with its cause:
// io.EOF for a read of 0 bytes, or the errno (e.g. syscall.EIO). Comparing it with == matches
// neither.
type hangupError struct {
	name string
	err  error
}

func (e *hangupError) Error() string {
	return fmt.Sprintf("serial: %s: %v (device hung up)", e.name, e.err)
}

func (e *hangupError) Is(target error) bool { return target == io.ErrUnexpectedEOF }

func (e *hangupError) Unwrap() error { return e.err }

func (p *port) setTermios(termios *syscall.Termios) (err error) {
	if err = tcsetattr(p.fd, termios); err != nil {
		err = fmt.Errorf("serial: could not set setting: %v", err)
	}
	return
}

// backupTermios saves current termios setting.
// Make sure that device file has been opened before calling this function.
func (p *port) backupTermios() {
	oldTermios := &syscall.Termios{}
	if err := tcgetattr(p.fd, oldTermios); err != nil {
		// Warning only.
		log.Printf("serial: could not get setting: %v\n", err)
		return
	}
	// Will be reloaded when closing.
	p.oldTermios = oldTermios
}

// restoreTermios restores backed up termios setting on fd, an open fd of the device.
func (p *port) restoreTermios(fd int) {
	if p.oldTermios == nil {
		return
	}
	if err := tcsetattr(fd, p.oldTermios); err != nil {
		// Warning only.
		log.Printf("serial: could not restore setting: %v\n", err)
		return
	}
	p.oldTermios = nil
}

// Helpers for termios

func newTermios(c *Config) (termios *syscall.Termios, err error) {
	termios = &syscall.Termios{}
	flag := termios.Cflag
	// Baud rate
	if c.BaudRate == 0 {
		// 19200 is the required default.
		flag = syscall.B19200
	} else {
		var ok bool
		flag, ok = baudRates[c.BaudRate]
		if !ok {
			err = fmt.Errorf("serial: unsupported baud rate %v", c.BaudRate)
			return
		}
	}
	termios.Cflag |= flag
	// Input baud.
	cfSetIspeed(termios, flag)
	// Output baud.
	cfSetOspeed(termios, flag)
	// Character size.
	if c.DataBits == 0 {
		flag = syscall.CS8
	} else {
		var ok bool
		flag, ok = charSizes[c.DataBits]
		if !ok {
			err = fmt.Errorf("serial: unsupported character size %v", c.DataBits)
			return
		}
	}
	termios.Cflag |= flag
	// Stop bits
	switch c.StopBits {
	case 0, 1:
		// Default is one stop bit.
		// noop
	case 2:
		// CSTOPB: Set two stop bits.
		termios.Cflag |= syscall.CSTOPB
	default:
		err = fmt.Errorf("serial: unsupported stop bits %v", c.StopBits)
		return
	}
	switch c.Parity {
	case "N":
		// noop
	case "O":
		// PARODD: Parity is odd.
		termios.Cflag |= syscall.PARODD
		fallthrough
	case "", "E":
		// As mentioned in the modbus spec, the default parity mode must be Even parity
		// PARENB: Enable parity generation on output.
		termios.Cflag |= syscall.PARENB
		// INPCK: Enable input parity checking.
		termios.Iflag |= syscall.INPCK
	default:
		err = fmt.Errorf("serial: unsupported parity %v", c.Parity)
		return
	}
	// Control modes.
	// CREAD: Enable receiver.
	// CLOCAL: Ignore control lines.
	termios.Cflag |= syscall.CREAD | syscall.CLOCAL
	// Special characters.
	// VMIN: Minimum number of characters for noncanonical read.
	// VTIME: Time in deciseconds for noncanonical read.
	// With VMIN 0 a read(2) without data returns 0 (EOF) even on the O_NDELAY fd; with VMIN 1
	// it fails with EAGAIN, so the runtime poller waits for data. Timeouts are p.timeout.
	termios.Cc[syscall.VMIN] = 1
	termios.Cc[syscall.VTIME] = 0
	return
}

// enableRS485 enables RS485 functionality of driver via an ioctl if the config says so
func enableRS485(fd int, config *RS485Config) error {
	if !config.Enabled {
		return nil
	}
	rs485 := rs485_ioctl_opts{
		rs485Enabled,
		uint32(config.DelayRtsBeforeSend / time.Millisecond),
		uint32(config.DelayRtsAfterSend / time.Millisecond),
		[5]uint32{0, 0, 0, 0, 0},
	}

	if config.RtsHighDuringSend {
		rs485.flags |= rs485RTSOnSend
	}
	if config.RtsHighAfterSend {
		rs485.flags |= rs485RTSAfterSend
	}
	if config.RxDuringTx {
		rs485.flags |= rs485RXDuringTX
	}

	r, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(rs485Tiocs),
		uintptr(unsafe.Pointer(&rs485)))
	if errno != 0 {
		return os.NewSyscallError("SYS_IOCTL (RS485)", errno)
	}
	if r != 0 {
		return errors.New("serial: unknown error from SYS_IOCTL (RS485)")
	}
	return nil
}
