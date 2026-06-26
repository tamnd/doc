package engine

import (
	"sync"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/vfs"
)

// This file covers write reordering, spec 2061 doc 19 §17.4. A storage controller
// without strict ordering can flush cached writes in a different order than they
// were issued. That cannot corrupt the database as long as one rule holds: a data
// page never reaches the main file before the WAL frame that covers it has been
// fsync'd. reorderFS does two things at once. It observes every write and asserts
// that rule directly, and it reorders adjacent main-file writes (the controller's
// behavior) so the recovery check runs against a genuinely reordered main file.

// reorderFS decorates a MemFS. It tracks whether the WAL has unsynced frames and
// fails the test if a main-file page is written while that is true. It also swaps
// adjacent main-file writes to different offsets, flushing the held write at the
// next write to a different page, at any Sync, and at Close.
type reorderFS struct {
	mem  *vfs.MemFS
	main string
	wal  string

	mu          sync.Mutex
	walUnsynced bool // WAL frames written since the last WAL Sync
	violations  []string
	reorders    int

	held    []byte // a delayed main-file write, flushed out of order
	heldOff int64
	hasHeld bool
}

func newReorderFS(main string) *reorderFS {
	return &reorderFS{mem: vfs.NewMemFS(), main: main, wal: main + "-wal"}
}

func (r *reorderFS) Open(path string, flags vfs.OpenFlags) (vfs.File, error) {
	f, err := r.mem.Open(path, flags)
	if err != nil {
		return nil, err
	}
	return &reorderFile{fs: r, inner: f, path: path}, nil
}

func (r *reorderFS) Delete(path string, syncDir bool) error { return r.mem.Delete(path, syncDir) }
func (r *reorderFS) Exists(path string) (bool, error)       { return r.mem.Exists(path) }
func (r *reorderFS) ShmMap(p string, region int, create bool) ([]byte, error) {
	return r.mem.ShmMap(p, region, create)
}

// flushHeldLocked writes any delayed main-file write through to the inner file.
func (r *reorderFS) flushHeldLocked(inner vfs.File) {
	if r.hasHeld {
		_, _ = inner.WriteAt(r.held, r.heldOff)
		r.hasHeld = false
		r.held = nil
	}
}

type reorderFile struct {
	fs    *reorderFS
	inner vfs.File
	path  string
}

func (f *reorderFile) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	// Serve a read that overlaps the delayed write from the held buffer so the
	// reordering stays invisible to the live engine, exactly as a controller cache
	// would.
	if f.path == f.fs.main && f.fs.hasHeld && off == f.fs.heldOff && len(p) <= len(f.fs.held) {
		n := copy(p, f.fs.held)
		f.fs.mu.Unlock()
		return n, nil
	}
	f.fs.mu.Unlock()
	return f.inner.ReadAt(p, off)
}

func (f *reorderFile) WriteAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	if f.path == f.fs.wal {
		f.fs.walUnsynced = true
		f.fs.mu.Unlock()
		return f.inner.WriteAt(p, off)
	}
	if f.path == f.fs.main {
		// The write-ahead rule: no data page may reach the main file while the WAL
		// holds unsynced frames.
		if f.fs.walUnsynced {
			f.fs.violations = append(f.fs.violations,
				"main page written at offset while WAL had unsynced frames")
		}
		// Reorder: hold this write and flush the previously held one first, but
		// never reorder two writes to the same offset (a controller preserves
		// per-sector order).
		buf := make([]byte, len(p))
		copy(buf, p)
		if f.fs.hasHeld && f.fs.heldOff != off {
			prev, prevOff := f.fs.held, f.fs.heldOff
			f.fs.held, f.fs.heldOff = buf, off
			f.fs.reorders++
			f.fs.mu.Unlock()
			// Flush this newer write first, then the older held one: reversed pair.
			_, _ = f.inner.WriteAt(buf, off)
			_, _ = f.inner.WriteAt(prev, prevOff)
			f.fs.mu.Lock()
			// The just-written buf is now durable; drop the held slot it occupied.
			if f.fs.hasHeld && f.fs.heldOff == off {
				f.fs.hasHeld = false
				f.fs.held = nil
			}
			f.fs.mu.Unlock()
			return len(p), nil
		}
		f.fs.flushHeldLocked(f.inner)
		f.fs.held, f.fs.heldOff, f.fs.hasHeld = buf, off, true
		f.fs.mu.Unlock()
		return len(p), nil
	}
	f.fs.mu.Unlock()
	return f.inner.WriteAt(p, off)
}

func (f *reorderFile) Sync(mode vfs.SyncMode) error {
	f.fs.mu.Lock()
	if f.path == f.fs.wal {
		f.fs.walUnsynced = false
	}
	if f.path == f.fs.main {
		f.fs.flushHeldLocked(f.inner)
	}
	f.fs.mu.Unlock()
	return f.inner.Sync(mode)
}

func (f *reorderFile) Truncate(size int64) error {
	f.fs.mu.Lock()
	f.fs.flushHeldLocked(f.inner)
	f.fs.mu.Unlock()
	return f.inner.Truncate(size)
}
func (f *reorderFile) Size() (int64, error) { return f.inner.Size() }
func (f *reorderFile) Close() error {
	f.fs.mu.Lock()
	f.fs.flushHeldLocked(f.inner)
	f.fs.mu.Unlock()
	return f.inner.Close()
}

// TestWriteReorderingPreservesDurability runs a commit-and-checkpoint workload over
// reorderFS, which both reorders main-file writes and asserts the WAL-before-data
// rule on every write. It then reopens the reordered file and checks the full
// committed state recovers and the structural check passes.
func TestWriteReorderingPreservesDurability(t *testing.T) {
	const db, coll, path = "shop", "orders", "reorder.doc"
	n := crashScale(t, 120)

	rfs := newReorderFS(path)
	e, err := Open(rfs, path, crashOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := e.CreateCollection(db, coll)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := c.InsertOne(crashDoc(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		// Checkpoint periodically so the workload writes data pages to the main
		// file under the reorderer, not only WAL frames.
		if i%20 == 0 {
			if err := e.Checkpoint(); err != nil {
				t.Fatalf("checkpoint at %d: %v", i, err)
			}
		}
	}
	main := rfs.mem.Snapshot(path)
	wal := rfs.mem.Snapshot(path + "-wal")
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(rfs.violations) > 0 {
		t.Fatalf("WAL write-ahead rule violated %d times: %v", len(rfs.violations), rfs.violations[0])
	}
	if rfs.reorders == 0 {
		t.Fatal("no main-file writes were reordered; the test did not exercise reordering")
	}

	re, err := Open(loadCrashFS(crashImage{main: main, wal: wal}, path), path, crashOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()
	rc := re.GetCollection(db, coll)
	if rc == nil {
		t.Fatal("collection lost after reordered workload")
	}
	got, err := rc.Find(bson.NewBuilder().Build())
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != n {
		t.Fatalf("recovered %d docs, want %d after reordered writes", len(got), n)
	}
	if rep := re.Check(true); !rep.Valid {
		t.Fatalf("doc check failed after reordered workload: %+v", rep)
	}
	t.Logf("reordered %d main-file writes with no write-ahead violation; %d docs recovered", rfs.reorders, n)
}
