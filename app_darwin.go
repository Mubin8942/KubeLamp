//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func inAppBundle() bool {
	exe, err := os.Executable()
	return err == nil && strings.Contains(exe, ".app/Contents/MacOS/")
}

func logFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "KubeLamp.log")
}

// fixPath: apps started from Finder get a bare PATH (/usr/bin:/bin:…), so kubeconfig
// exec plugins like `aws`, `kubelogin` or `gke-gcloud-auth-plugin` aren't found.
// Borrow the PATH from the user's login shell, plus the usual Homebrew locations.
func fixPath() {
	var parts []string
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-ilc", `printf '\n__PATH__%s' "$PATH"`)
	cmd.Stdin = nil
	if out, err := cmd.Output(); err == nil {
		if i := strings.LastIndex(string(out), "__PATH__"); i >= 0 {
			parts = append(parts, strings.Split(strings.TrimSpace(string(out)[i+8:]), ":")...)
		}
	}
	home, _ := os.UserHomeDir()
	parts = append(parts, strings.Split(os.Getenv("PATH"), ":")...)
	parts = append(parts, "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin",
		filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	os.Setenv("PATH", strings.Join(out, ":"))
}

// detachFromLauncher: when opened from Finder, run the server as a background process
// and let the launcher exit. Double-clicking the app again then just reopens the browser.
func detachFromLauncher(args []string) bool {
	if !inAppBundle() || os.Getenv("KUBELAMP_CHILD") != "" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(logFilePath()), 0o755)
	lf, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "KUBELAMP_CHILD=1")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return false
	}
	_ = cmd.Process.Release()
	return true
}
