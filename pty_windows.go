//go:build windows

package main

import (
	"errors"
	"os/exec"
)

// The Windows console (ConPTY) isn't wired up yet; pod exec still works.
func startPTY(cmd *exec.Cmd) (Terminal, error) {
	return nil, errors.New("the local shell is available on macOS and Linux; on Windows use your terminal (kubectl) — pod exec works here")
}

func killTree(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
