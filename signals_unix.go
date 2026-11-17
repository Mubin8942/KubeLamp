//go:build linux || darwin

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// Signals ignored when KubeLamp starts (started in the background, under nohup, by some
// process managers) would stay ignored in every program it runs, so Ctrl+C couldn't stop
// a command in a terminal. Catching them instead keeps KubeLamp's behaviour (it still
// doesn't stop on them) while programs it starts get the normal default again.
func init() {
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP} {
		if signal.Ignored(s) {
			signal.Notify(make(chan os.Signal, 1), s)
		}
	}
}
