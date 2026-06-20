package vfs

import (
	"io"
	"os"
	"sync"
)

// MemFS is an in-memory FS for fast, deterministic tests. Files live in a map
// from path to a byte buffer; there is no real durability, so Sync is a no-op.
// MemFS is safe for concurrent use across files and within a single file.
type MemFS struct {
	mu    sync.Mutex
	files map[string]*memData
}

// NewMemFS returns an empty in-memory file system.
func NewMemFS() *MemFS { return &MemFS{files: make(map[string]*memData)} }

// memData is the shared backing store for a path; multiple open handles alias it.
type memData struct {
	mu   sync.RWMutex
	data []byte
}

// Open opens or creates path. OpenExclusive with an existing file returns
// os.ErrExist; opening a missing file without OpenCreate returns os.ErrNotExist.
func (m *MemFS) Open(path string, flags OpenFlags) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.files[path]
	if ok && flags&OpenExclusive != 0 && flags&OpenCreate != 0 {
		return nil, os.ErrExist
	}
	if !ok {
		if flags&OpenCreate == 0 {
			return nil, os.ErrNotExist
		}
		d = &memData{}
		m.files[path] = d
	}
	return &memFile{d: d, readOnly: flags&OpenReadOnly != 0}, nil
}

// Delete removes path. syncDir is ignored (memory has no directory metadata).
func (m *MemFS) Delete(path string, syncDir bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[path]; !ok {
		return os.ErrNotExist
	}
	delete(m.files, path)
	return nil
}

// Exists reports whether path exists.
func (m *MemFS) Exists(path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[path]
	return ok, nil
}

// ShmMap returns ErrNotImplemented; memfs has no shared memory.
func (m *MemFS) ShmMap(path string, region int, create bool) ([]byte, error) {
	return nil, ErrNotImplemented
}

type memFile struct {
	d        *memData
	readOnly bool
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	f.d.mu.RLock()
	defer f.d.mu.RUnlock()
	if off >= int64(len(f.d.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.d.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	if f.readOnly {
		return 0, os.ErrPermission
	}
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	end := off + int64(len(p))
	if end > int64(len(f.d.data)) {
		grown := make([]byte, end)
		copy(grown, f.d.data)
		f.d.data = grown
	}
	copy(f.d.data[off:end], p)
	return len(p), nil
}

func (f *memFile) Sync(mode SyncMode) error { return nil }

func (f *memFile) Truncate(size int64) error {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	if size <= int64(len(f.d.data)) {
		f.d.data = f.d.data[:size]
		return nil
	}
	grown := make([]byte, size)
	copy(grown, f.d.data)
	f.d.data = grown
	return nil
}

func (f *memFile) Size() (int64, error) {
	f.d.mu.RLock()
	defer f.d.mu.RUnlock()
	return int64(len(f.d.data)), nil
}

func (f *memFile) Close() error { return nil }

// Snapshot returns a copy of the current bytes of path, or nil if absent. Tests
// use it to capture the durable image at a chosen instant (e.g. just before an
// injected crash) and compare it against the post-recovery image.
func (m *MemFS) Snapshot(path string) []byte {
	m.mu.Lock()
	d, ok := m.files[path]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]byte, len(d.data))
	copy(out, d.data)
	return out
}
