package serial

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

func unlockpt(master *os.File) error {
	var unlock int32
	return ioctl(master, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
}

func ptsname(master *os.File) (string, error) {
	var n uint32
	if err := ioctl(master, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
