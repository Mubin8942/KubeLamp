//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func openBrowser(u string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}
