//go:build unix

package guest

import "syscall"

// hostAlive probes the host pid with signal 0: it delivers nothing and only
// reports whether the process exists. EPERM still means alive — the process
// is there, we just may not signal it. Only ESRCH, no such process, is the
// host-death verdict.
func hostAlive(pid int) bool {
	return syscall.Kill(pid, 0) != syscall.ESRCH
}
