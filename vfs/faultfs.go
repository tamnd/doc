package vfs

import (
	"errors"
	"sync"
)

// FaultMode selects the kind of fault FaultFS injects (spec 2061 doc 05 §17,
// §19; doc 19 §M0 names Write, Sync, Tear, and Drop). The modes model the
// failure classes a real storage stack exhibits under power loss and media
// error.
type FaultMode int

const (
	// FaultNone passes every operation through unchanged.
	FaultNone FaultMode = iota
	// FaultWrite fails a qualifying WriteAt with Err, writing nothing — a clean
	// I/O error (e.g. ENOSPC, EIO) surfaced as a value.
	FaultWrite
	// FaultSync fails a qualifying Sync with Err — the fsync-failure case
	// ("fsyncgate"), where data the caller believed durable is not.
	FaultSync
	// FaultTear writes only the first TearAt bytes of a qualifying WriteAt, then
	// returns Err — a torn write left by power loss mid-write.
	FaultTear
	// FaultDrop reports a qualifying WriteAt as fully successful but discards the
	// bytes — a lost write, the drive's volatile cache evaporating at power loss.
	FaultDrop
)

// ErrInjected is the default error FaultFS returns when a plan does not name its
// own Err.
var ErrInjected = errors.New("vfs: injected fault")

// FaultPlan configures one armed fault. The fault triggers on the qualifying
// operation whose ordinal exceeds the After threshold: AfterWrites for write-side
// faults, AfterSyncs for sync faults. With Once set, the fault fires a single
// time and then disarms; otherwise it fires on every subsequent qualifying op.
type FaultPlan struct {
	Mode        FaultMode
	Err         error // defaults to ErrInjected when nil
	AfterWrites int   // allow this many WriteAts before a write-side fault fires
	AfterSyncs  int   // allow this many Syncs before a sync fault fires
	TearAt      int   // bytes actually written for FaultTear
	Once        bool  // fire a single time, then disarm
}

func (p FaultPlan) err() error {
	if p.Err != nil {
		return p.Err
	}
	return ErrInjected
}

// FaultFS decorates an underlying FS, injecting faults according to an armed
// plan. It is the harness behind the crash/recovery and torn-write test suites.
// A test arms a plan, exercises the engine, and asserts that recovery produces a
// consistent state. FaultFS is safe for concurrent use.
type FaultFS struct {
	inner FS

	mu         sync.Mutex
	plan       FaultPlan
	armed      bool
	writeCount int
	syncCount  int
	injected   int // number of faults actually injected, for assertions
}

// NewFaultFS wraps inner.
func NewFaultFS(inner FS) *FaultFS { return &FaultFS{inner: inner} }

// Arm installs plan and resets the operation counters. Counting restarts so the
// After thresholds are relative to the moment of arming.
func (f *FaultFS) Arm(plan FaultPlan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = plan
	f.armed = plan.Mode != FaultNone
	f.writeCount = 0
	f.syncCount = 0
}

// Disarm clears any armed plan; subsequent operations pass through.
func (f *FaultFS) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = false
	f.plan = FaultPlan{}
}

// Injected returns the number of faults injected since the last Arm. Tests assert
// that a fault actually fired (or did not).
func (f *FaultFS) Injected() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected
}

// nextWriteFault decides what to do for the current WriteAt, returning the fault
// mode to apply (FaultNone to pass through) and the TearAt byte count.
func (f *FaultFS) nextWriteFault() (FaultMode, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCount++
	if !f.armed {
		return FaultNone, 0, nil
	}
	switch f.plan.Mode {
	case FaultWrite, FaultTear, FaultDrop:
		if f.writeCount > f.plan.AfterWrites {
			if f.plan.Once {
				f.armed = false
			}
			f.injected++
			return f.plan.Mode, f.plan.TearAt, f.plan.err()
		}
	}
	return FaultNone, 0, nil
}

func (f *FaultFS) nextSyncFault() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCount++
	if !f.armed || f.plan.Mode != FaultSync {
		return nil
	}
	if f.syncCount > f.plan.AfterSyncs {
		if f.plan.Once {
			f.armed = false
		}
		f.injected++
		return f.plan.err()
	}
	return nil
}

// Open wraps the underlying file in a fault-aware handle.
func (f *FaultFS) Open(path string, flags OpenFlags) (File, error) {
	inner, err := f.inner.Open(path, flags)
	if err != nil {
		return nil, err
	}
	return &faultFile{fs: f, inner: inner}, nil
}

func (f *FaultFS) Delete(path string, syncDir bool) error { return f.inner.Delete(path, syncDir) }
func (f *FaultFS) Exists(path string) (bool, error)       { return f.inner.Exists(path) }
func (f *FaultFS) ShmMap(path string, region int, create bool) ([]byte, error) {
	return f.inner.ShmMap(path, region, create)
}

type faultFile struct {
	fs    *FaultFS
	inner File
}

func (ff *faultFile) ReadAt(p []byte, off int64) (int, error) { return ff.inner.ReadAt(p, off) }

func (ff *faultFile) WriteAt(p []byte, off int64) (int, error) {
	mode, tearAt, ferr := ff.fs.nextWriteFault()
	switch mode {
	case FaultNone:
		return ff.inner.WriteAt(p, off)
	case FaultWrite:
		return 0, ferr
	case FaultDrop:
		// Report success but write nothing: a lost write.
		return len(p), nil
	case FaultTear:
		n := tearAt
		if n > len(p) {
			n = len(p)
		}
		if n < 0 {
			n = 0
		}
		if n > 0 {
			_, _ = ff.inner.WriteAt(p[:n], off)
		}
		return n, ferr
	default:
		return ff.inner.WriteAt(p, off)
	}
}

func (ff *faultFile) Sync(mode SyncMode) error {
	if err := ff.fs.nextSyncFault(); err != nil {
		return err
	}
	return ff.inner.Sync(mode)
}

func (ff *faultFile) Truncate(size int64) error { return ff.inner.Truncate(size) }
func (ff *faultFile) Size() (int64, error)      { return ff.inner.Size() }
func (ff *faultFile) Close() error              { return ff.inner.Close() }
