//go:build darwin

package vfs

import (
	"os"
	"syscall"
)

// fullSync flushes f to stable storage. On macOS, plain fsync does not flush the
// drive's write cache, so both SyncFull and SyncData issue fcntl(F_FULLFSYNC),
// which does — the same conclusion SQLite reaches (spec 2061 doc 05 §2.5). If
// F_FULLFSYNC is unsupported by the underlying file system, it falls back to
// fsync rather than failing.
func fullSync(f *os.File, mode SyncMode) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_FULLFSYNC), 0)
	if errno != 0 {
		return f.Sync()
	}
	return nil
}
