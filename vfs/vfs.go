// Package vfs is the virtual file-system seam for doc. All durable I/O passes
// through the FS and File interfaces so that the storage engine can be driven
// against a real file system (osfs), an in-memory file system (memfs, for fast
// deterministic tests), or a fault-injecting decorator (FaultFS, for crash and
// torn-write testing). The interface mirrors the kv VFS so that doc inherits the
// same durability substrate (spec 2061 doc 05 §2.5).
package vfs

import "errors"

// SyncMode selects the durability barrier requested of Sync.
type SyncMode int

const (
	// SyncFull flushes data and metadata to stable storage. On macOS it maps to
	// fcntl(F_FULLFSYNC), which actually flushes the drive cache; on Linux it
	// maps to fsync. This is the level required for true crash durability.
	SyncFull SyncMode = iota
	// SyncData flushes file data (and the metadata needed to read it back) but
	// not all metadata. On Linux it maps to fdatasync; on macOS, where plain
	// fsync does not flush the drive cache, it maps to F_FULLFSYNC as well.
	SyncData
)

// OpenFlags controls how a path is opened.
type OpenFlags int

const (
	// OpenRead opens an existing file for reading and writing.
	OpenRead OpenFlags = 0
	// OpenCreate creates the file if it does not exist.
	OpenCreate OpenFlags = 1 << iota
	// OpenReadOnly opens the file without write access.
	OpenReadOnly
	// OpenExclusive fails if the file already exists (with OpenCreate).
	OpenExclusive
)

// ErrNotImplemented reports an optional VFS capability a backend does not
// provide (e.g. ShmMap on memfs).
var ErrNotImplemented = errors.New("vfs: operation not implemented by this backend")

// FS is a minimal virtual file system: enough to back the pager, the WAL, and
// the shared-memory index. Implementations must be safe for concurrent use
// across distinct files; concurrent access to a single File is the caller's
// responsibility (the pager serializes writes).
type FS interface {
	// Open opens or creates path according to flags.
	Open(path string, flags OpenFlags) (File, error)
	// Delete removes path. If syncDir is true the containing directory is synced
	// so the deletion is durable.
	Delete(path string, syncDir bool) error
	// Exists reports whether path exists.
	Exists(path string) (bool, error)
	// ShmMap maps a 32 KiB region of the shared-memory file for path, creating it
	// when create is true. Backends that do not support shared memory return
	// ErrNotImplemented.
	ShmMap(path string, region int, create bool) ([]byte, error)
}

// File is an open file supporting positional I/O and explicit durability.
type File interface {
	// ReadAt reads len(p) bytes at offset off. It follows io.ReaderAt semantics:
	// a short read returns an error (io.EOF at end of file).
	ReadAt(p []byte, off int64) (int, error)
	// WriteAt writes p at offset off, extending the file if needed.
	WriteAt(p []byte, off int64) (int, error)
	// Sync flushes prior writes to stable storage at the requested level.
	Sync(mode SyncMode) error
	// Truncate sets the file size to size bytes.
	Truncate(size int64) error
	// Size returns the current file size in bytes.
	Size() (int64, error)
	// Close closes the file.
	Close() error
}
