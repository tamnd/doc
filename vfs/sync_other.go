//go:build !darwin

package vfs

import "os"

// fullSync flushes f to stable storage. On platforms other than macOS, M0 maps
// both sync modes to fsync via os.File.Sync. The SyncData/fdatasync refinement
// on Linux is a later optimization (spec 2061 doc 05 §2.5); fsync is always a
// correct, if stronger, barrier.
func fullSync(f *os.File, mode SyncMode) error {
	return f.Sync()
}
