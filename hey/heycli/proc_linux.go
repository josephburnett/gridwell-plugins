package heycli

import (
	"os/exec"
	"syscall"
)

// dieWithParent has the kernel end the child when this process dies.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
