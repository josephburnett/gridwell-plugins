//go:build !linux

package heycli

import "os/exec"

// dieWithParent has no kernel door off Linux. An orphaned feed still ends at
// its next line: its stdout has no reader, and the write fails.
func dieWithParent(*exec.Cmd) {}
