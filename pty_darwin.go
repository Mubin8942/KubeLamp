//go:build darwin

package main

import (
	"bytes"
	"os"
	"unsafe"
)

const (
	tiocptygrant = 0x20007454
	tiocptyunlk  = 0x20007452
	tiocptygname = 0x40807453
)

func ptsName(master *os.File) (string, error) {
	if err := ioctl(master.Fd(), tiocptygrant, 0); err != nil {
		return "", err
	}
	if err := ioctl(master.Fd(), tiocptyunlk, 0); err != nil {
		return "", err
	}
	buf := make([]byte, 128)
	if err := ioctl(master.Fd(), tiocptygname, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return "", err
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}
