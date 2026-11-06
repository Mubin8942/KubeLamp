//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

type unixPTY struct{ *os.File }

type winsize struct{ Row, Col, X, Y uint16 }

func (p unixPTY) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	ws := winsize{Row: uint16(rows), Col: uint16(cols)}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, p.Fd(), uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// startPTY runs cmd attached to a new pseudo-terminal and returns the master side.
func startPTY(cmd *exec.Cmd) (Terminal, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	name, err := ptsName(master)
	if err != nil {
		master.Close()
		return nil, err
	}
	slave, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	p := unixPTY{master}
	p.Resize(120, 30)
	return p, nil
}

func killTree(cmd *exec.Cmd) {
	// the shell leads its own session/process group (Setsid)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	_ = cmd.Process.Kill()
}
