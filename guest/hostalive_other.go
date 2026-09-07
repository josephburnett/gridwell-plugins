//go:build !unix

package guest

import "os"

// hostAlive on Windows and anything else without signals. os.FindProcess is
// a real question there — it opens a handle to the pid and fails when no
// such process exists — unlike on unix, where it always succeeds. Any other
// failure (a permission refusal) means the process is there, so only a
// not-found answer is the host-death verdict.
func hostAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if p != nil {
		_ = p.Release()
	}
	return true
}
