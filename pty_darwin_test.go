package serial

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

func unlockpt(master *os.File) error {
	if err := ioctl(master, syscall.TIOCPTYGRANT, 0); err != nil {
		return err
	}
	return ioctl(master, syscall.TIOCPTYUNLK, 0)
}

func ptsname(master *os.File) (string, error) {
	var name [128]byte
	if err := ioctl(master, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return "", err
	}
	if i := bytes.IndexByte(name[:], 0); i >= 0 {
		return string(name[:i]), nil
	}
	return string(name[:]), nil
}
