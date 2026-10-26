//go:build linux || darwin
// +build linux darwin

package serial

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// openPTY opens a pseudo terminal pair and returns the master and the slave device path.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty support: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unlockpt(master); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	slave, err := ptsname(master)
	if err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	return master, slave
}

// ioctl runs an ioctl on f via SyscallConn. f.Fd() is not used because it puts f into blocking
// mode, which would make deadlines on the pty master stop working.
func ioctl(f *os.File, req, arg uintptr) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// openPort opens the slave side of a new pty as a serial port. With minFD > 0 the process fd
// table is filled first, so the port lands at fd >= minFD.
func openPort(t *testing.T, c Config, minFD int) (*port, *os.File) {
	t.Helper()
	master, slave := openPTY(t)
	if minFD > 0 {
		fillFDs(t, minFD)
	}
	c.Address = slave
	p, err := Open(&c)
	if err != nil {
		t.Fatalf("open %s: %v", slave, err)
	}
	t.Cleanup(func() { p.Close() })
	pp := p.(*port)
	if pp.fd < minFD {
		t.Fatalf("serial port got fd %d, want >= %d", pp.fd, minFD)
	}
	return pp, master
}

// fillFDs opens /dev/null until the next fd the process gets is at least min, so the serial port
// opened afterwards lands above FD_SETSIZE like it does in a process that holds many sockets.
// RLIMIT_NOFILE is raised if needed and restored on cleanup.
func fillFDs(t *testing.T, min int) {
	t.Helper()
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	if want := uint64(min + 64); rl.Cur < want {
		if rl.Max < want {
			skipOrFailInCI(t, "RLIMIT_NOFILE hard limit %d is below %d, raise it (ulimit -Hn) to test fds >= FD_SETSIZE", rl.Max, want)
		}
		orig := rl
		rl.Cur = want
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
			skipOrFailInCI(t, "setrlimit: %v", err)
		}
		// registered before the fds below, so it runs after they are closed
		t.Cleanup(func() {
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
				t.Errorf("restore RLIMIT_NOFILE: %v", err)
			}
		})
	}
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		t.Cleanup(func() { f.Close() })
		if int(f.Fd()) >= min {
			return
		}
	}
}

// skipOrFailInCI skips the test, or fails it if the CI environment variable is set, so a CI
// runner with a low fd limit does not silently skip the only tests for fds >= FD_SETSIZE.
func skipOrFailInCI(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// fdCases runs f for a port below and above FD_SETSIZE (1024). With select(2), a fd >= 1024 made
// the kernel write past the fixed-size fd_set on the stack and watch the wrong fd.
var fdCases = []struct {
	name  string
	minFD int
}{
	{"low fd", 0},
	{"high fd", 1100},
}

func TestReadTimeout(t *testing.T) {
	for _, tc := range fdCases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := openPort(t, Config{Timeout: 200 * time.Millisecond}, tc.minFD)

			var buf [16]byte
			start := time.Now()
			if _, err := p.Read(buf[:]); !errors.Is(err, ErrTimeout) {
				t.Fatalf("got %v, want ErrTimeout", err)
			}
			if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
				t.Fatalf("returned after %v, want ~200ms timeout", elapsed)
			}
		})
	}
}

func TestReadData(t *testing.T) {
	for _, tc := range fdCases {
		t.Run(tc.name, func(t *testing.T) {
			p, master := openPort(t, Config{Timeout: 2 * time.Second}, tc.minFD)

			msg := "test serial"
			if _, err := master.Write([]byte(msg)); err != nil {
				t.Fatalf("write to pty master: %v", err)
			}
			var buf [16]byte
			n, err := p.Read(buf[:])
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(buf[:n]) != msg {
				t.Fatalf("read %q, want %q", buf[:n], msg)
			}
		})
	}
}

// TestReadBlocks checks that Read without a timeout waits for data instead of returning early.
func TestReadBlocks(t *testing.T) {
	for _, tc := range fdCases {
		t.Run(tc.name, func(t *testing.T) {
			p, master := openPort(t, Config{}, tc.minFD)

			const delay = 200 * time.Millisecond
			go func() {
				time.Sleep(delay)
				master.Write([]byte("x"))
			}()
			var buf [16]byte
			start := time.Now()
			n, err := p.Read(buf[:])
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if elapsed := time.Since(start); elapsed < delay-10*time.Millisecond {
				t.Fatalf("returned after %v, before the data was written", elapsed)
			}
			if string(buf[:n]) != "x" {
				t.Fatalf("read %q, want %q", buf[:n], "x")
			}
		})
	}
}

// TestReadHangup checks that a hung-up line makes Read fail at once instead of waiting for the
// timeout.
func TestReadHangup(t *testing.T) {
	for _, tc := range fdCases {
		t.Run(tc.name, func(t *testing.T) {
			p, master := openPort(t, Config{Timeout: 2 * time.Second}, tc.minFD)
			master.Close()

			var buf [16]byte
			start := time.Now()
			_, err := p.Read(buf[:])
			// modbus reconnects on io.EOF/io.ErrUnexpectedEOF, so a hangup must surface as one of them
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("returned after %v, want an immediate error", elapsed)
			}
		})
	}
}

// TestClosed checks that Read and Write on a closed port fail at once instead of blocking.
func TestClosed(t *testing.T) {
	for _, timeout := range []time.Duration{0, 2 * time.Second} {
		p, _ := openPort(t, Config{Timeout: timeout}, 0)
		p.Close()

		done := make(chan error, 1)
		go func() {
			var buf [16]byte
			_, err := p.Read(buf[:])
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrClosed) {
				t.Fatalf("timeout %v: got %v, want os.ErrClosed", timeout, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout %v: Read on a closed port blocked", timeout)
		}
		if _, err := p.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("timeout %v: Write got %v, want os.ErrClosed", timeout, err)
		}
	}
}

// TestCloseDuringRead checks that Close wakes a blocked Read.
func TestCloseDuringRead(t *testing.T) {
	for _, tc := range fdCases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := openPort(t, Config{}, tc.minFD)

			done := make(chan error, 1)
			go func() {
				var buf [16]byte
				_, err := p.Read(buf[:])
				done <- err
			}()
			time.Sleep(100 * time.Millisecond) // let Read block
			closed := make(chan error, 1)
			go func() { closed <- p.Close() }()

			select {
			case err := <-done:
				if !errors.Is(err, os.ErrClosed) {
					t.Fatalf("Read got %v, want os.ErrClosed", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not wake the blocked Read")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close blocked")
			}
		})
	}
}

// TestCloseTwice checks that a second Close succeeds instead of failing like os.File.Close.
func TestCloseTwice(t *testing.T) {
	p, _ := openPort(t, Config{}, 0)
	for i := 0; i < 2; i++ {
		if err := p.Close(); err != nil {
			t.Fatalf("Close %d: %v", i+1, err)
		}
	}
}

// TestReadEmpty checks that a zero-length Read returns at once instead of waiting for data and
// reporting a hangup.
func TestReadEmpty(t *testing.T) {
	p, _ := openPort(t, Config{Timeout: 2 * time.Second}, 0)

	start := time.Now()
	if n, err := p.Read(nil); n != 0 || err != nil {
		t.Fatalf("got %d, %v, want 0, nil", n, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %v, want at once", elapsed)
	}
}

// TestErrorsComparable checks that Read and Write return the plain errors, so callers comparing
// with == (as older code does) keep working; only hangups need errors.Is.
func TestErrorsComparable(t *testing.T) {
	p, master := openPort(t, Config{Timeout: 100 * time.Millisecond}, 0)
	var buf [16]byte
	if _, err := p.Read(buf[:]); err != ErrTimeout {
		t.Errorf("Read timeout: got %#v, want ErrTimeout", err)
	}
	master.Close()
	if _, err := p.Read(buf[:]); !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, io.EOF) {
		t.Errorf("Read hangup: got %#v, want io.ErrUnexpectedEOF wrapping io.EOF", err)
	}
	if _, err := p.Write([]byte("x")); !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, syscall.EIO) {
		t.Errorf("Write hangup: got %#v, want io.ErrUnexpectedEOF wrapping EIO", err)
	}
	p.Close()
	if _, err := p.Read(buf[:]); err != os.ErrClosed {
		t.Errorf("Read closed: got %#v, want os.ErrClosed", err)
	}
	if _, err := p.Write([]byte("x")); err != os.ErrClosed {
		t.Errorf("Write closed: got %#v, want os.ErrClosed", err)
	}
}

func TestIOErr(t *testing.T) {
	p, _ := openPort(t, Config{}, 0)
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENXIO, syscall.ENODEV} {
		err := p.ioErr(&os.PathError{Op: "read", Path: "/dev/ttyS0", Err: errno})
		if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, errno) {
			t.Errorf("ioErr(%v) = %v, want io.ErrUnexpectedEOF wrapping %v", errno, err, errno)
		}
	}
	if err := p.ioErr(&os.PathError{Op: "read", Path: "/dev/ttyS0", Err: syscall.EAGAIN}); err != syscall.EAGAIN {
		t.Errorf("ioErr(EAGAIN) = %v, want the bare EAGAIN", err)
	}
	if err := p.ioErr(&os.PathError{Op: "read", Path: "/dev/ttyS0", Err: os.ErrDeadlineExceeded}); err != ErrTimeout {
		t.Errorf("ioErr(deadline exceeded) = %v, want ErrTimeout", err)
	}
	if err := p.ioErr(nil); err != nil {
		t.Errorf("ioErr(nil) = %v, want nil", err)
	}
}

// TestWriteFull fills the pty output queue (nobody reads the master): without a timeout Write
// must return at once like the old single non-blocking write(2), with a timeout it must give up
// with ErrTimeout after it.
func TestWriteFull(t *testing.T) {
	big := make([]byte, 1<<20)

	p, _ := openPort(t, Config{}, 0)
	start := time.Now()
	n, err := p.Write(big)
	if err != io.ErrShortWrite && err != syscall.EAGAIN {
		t.Errorf("no timeout: got %d, %v, want io.ErrShortWrite or EAGAIN", n, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("no timeout: returned after %v, want at once", elapsed)
	}

	p, _ = openPort(t, Config{Timeout: 200 * time.Millisecond}, 0)
	start = time.Now()
	if n, err := p.Write(big); err != ErrTimeout {
		t.Errorf("timeout: got %d, %v, want ErrTimeout", n, err)
	}
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond || elapsed > time.Second {
		t.Errorf("timeout: returned after %v, want ~200ms", elapsed)
	}
}

// TestWrite writes with a non-default line config and checks the bytes reach the other side.
func TestWrite(t *testing.T) {
	p, master := openPort(t, Config{BaudRate: 57600, DataBits: 7, Parity: "N", StopBits: 2}, 0)

	msg := "test serial"
	n, err := p.Write([]byte(msg))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("wrote %d bytes, want %d", n, len(msg))
	}
	if err := master.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline on pty master: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := master.Read(buf); err != nil {
		t.Fatalf("read from pty master: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("pty master read %q, want %q", buf, msg)
	}
}

// TestCloseRestoresWithoutFreeFD checks that Close restores the termios even when no fd is free
// for the duplicate it restores through.
func TestCloseRestoresWithoutFreeFD(t *testing.T) {
	_, slave := openPTY(t)
	// a pty resets its termios when its last fd is closed, so one is held open to check it
	hold, err := syscall.Open(slave, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NDELAY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", slave, err)
	}
	defer syscall.Close(hold)
	var orig syscall.Termios
	if err := tcgetattr(hold, &orig); err != nil {
		t.Fatalf("tcgetattr: %v", err)
	}
	p, err := Open(&Config{Address: slave})
	if err != nil {
		t.Fatalf("open %s: %v", slave, err)
	}
	defer p.Close()

	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	// with the limit at the lowest free fd, the duplicate fails with EMFILE
	free, err := syscall.Dup(hold)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	syscall.Close(free)
	low := rl
	low.Cur = uint64(free)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Skipf("setrlimit: %v", err)
	}
	err = p.Close()
	if rerr := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); rerr != nil {
		t.Fatalf("restore RLIMIT_NOFILE: %v", rerr)
	}
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	var got syscall.Termios
	if err := tcgetattr(hold, &got); err != nil {
		t.Fatalf("tcgetattr: %v", err)
	}
	if got != orig {
		t.Errorf("termios not restored: got %+v, want %+v", got, orig)
	}
}
