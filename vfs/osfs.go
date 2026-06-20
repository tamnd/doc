package vfs

import (
	"os"
	"path/filepath"
)

// OSFS is the production FS backed by the operating system's file system. It
// uses buffered pread/pwrite via os.File.ReadAt/WriteAt and maps Sync to the
// platform's strongest cache-flushing primitive (see sync_darwin.go /
// sync_other.go). The zero value is ready to use.
type OSFS struct{}

// NewOSFS returns an OSFS.
func NewOSFS() *OSFS { return &OSFS{} }

// Open opens or creates path. OpenReadOnly maps to O_RDONLY; otherwise the file
// is opened O_RDWR, with O_CREATE and O_EXCL added per flags.
func (OSFS) Open(path string, flags OpenFlags) (File, error) {
	var mode int
	if flags&OpenReadOnly != 0 {
		mode = os.O_RDONLY
	} else {
		mode = os.O_RDWR
	}
	if flags&OpenCreate != 0 {
		mode |= os.O_CREATE
	}
	if flags&OpenExclusive != 0 {
		mode |= os.O_EXCL
	}
	f, err := os.OpenFile(path, mode, 0o644)
	if err != nil {
		return nil, err
	}
	return &osFile{f: f}, nil
}

// Delete removes path and, when syncDir is set, fsyncs the parent directory so
// the unlink is durable.
func (OSFS) Delete(path string, syncDir bool) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	if syncDir {
		d, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		return d.Sync()
	}
	return nil
}

// Exists reports whether path exists.
func (OSFS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// ShmMap is not yet implemented by the os backend; the WAL index uses a
// heap-backed region in M0–M5. It returns ErrNotImplemented.
func (OSFS) ShmMap(path string, region int, create bool) ([]byte, error) {
	return nil, ErrNotImplemented
}

type osFile struct {
	f *os.File
}

func (o *osFile) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *osFile) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o *osFile) Truncate(size int64) error                { return o.f.Truncate(size) }
func (o *osFile) Close() error                             { return o.f.Close() }

func (o *osFile) Size() (int64, error) {
	st, err := o.f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Sync flushes the file. The mode-to-syscall mapping is platform-specific; see
// fullSync, which is defined per-GOOS.
func (o *osFile) Sync(mode SyncMode) error { return fullSync(o.f, mode) }
