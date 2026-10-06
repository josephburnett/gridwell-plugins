//go:build unix

package fsfile

import "syscall"

// writeAccess asks the kernel whether this process may write path, which
// answers for the owner, the group, root and a read-only mount alike.
func writeAccess(path string) error {
	const wOK = 2 // POSIX W_OK
	return syscall.Access(path, wOK)
}
