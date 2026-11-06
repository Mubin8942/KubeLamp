//go:build !darwin

package main

func inAppBundle() bool                     { return false }
func fixPath()                              {}
func detachFromLauncher(args []string) bool { return false }
