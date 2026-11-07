package main

import (
	"os/exec"
	"testing"
)

// Runs the JavaScript tests for the terminal highlighter (web/js/termhl.js) when Node is installed.
func TestTerminalHighlighterJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "tools/jstest/termhl.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
