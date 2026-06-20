package pager

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/vfs"
)

// updateBody refetches an existing page and rewrites its body, returning a dirty
// (uncommitted) frame still pinned by the caller.
func updateBody(t *testing.T, p *Pager, id uint64, b byte) {
	t.Helper()
	f, err := p.Fetch(id, true)
	if err != nil {
		t.Fatalf("fetch %d: %v", id, err)
	}
	lo, hi := bodyRange(len(f.Buf))
	for i := lo; i < hi; i++ {
		f.Buf[i] = b
	}
	p.MarkDirty(f)
	p.Unpin(f)
}

// TestFsyncFailureSurfacesAsCommitError is the fsyncgate lesson: a failed fsync
// must surface as a commit error, never a silent success (spec 2061 doc 05 §11).
func TestFsyncFailureSurfacesAsCommitError(t *testing.T) {
	ff := vfs.NewFaultFS(vfs.NewMemFS())
	p := mustOpen(t, ff, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p.Close()

	allocFilled(t, p, 0x10)
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultSync, AfterSyncs: 0, Once: true})
	err := p.Commit()
	ff.Disarm()
	if !errors.Is(err, vfs.ErrInjected) {
		t.Fatalf("commit err = %v, want the injected fsync failure", err)
	}
	if ff.Injected() != 1 {
		t.Fatalf("expected exactly one injected fault, got %d", ff.Injected())
	}
}

// TestWriteErrorSurfacesAsCommitError checks a clean WAL write error (ENOSPC/EIO)
// propagates out of Commit rather than being swallowed.
func TestWriteErrorSurfacesAsCommitError(t *testing.T) {
	ff := vfs.NewFaultFS(vfs.NewMemFS())
	p := mustOpen(t, ff, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p.Close()

	allocFilled(t, p, 0x20)
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultWrite, AfterWrites: 0, Once: true})
	err := p.Commit()
	ff.Disarm()
	if !errors.Is(err, vfs.ErrInjected) {
		t.Fatalf("commit err = %v, want the injected write error", err)
	}
}

// TestTornWALCommitDiscardedByRecovery arms a torn write on the second WAL write
// of a one-frame commit (its payload). Recovery's chained checksum fails at the
// torn frame, so the torn commit is discarded and the prior commit survives
// (spec 2061 doc 05 §12, §14).
func TestTornWALCommitDiscardedByRecovery(t *testing.T) {
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	p := mustOpen(t, ff, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})

	// Commit A: durable.
	id := allocFilled(t, p, 0xA0)
	if err := p.Commit(); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	// Commit B: rewrite the same page (one frame, no header change), then tear
	// its payload write.
	updateBody(t, p, id, 0xB0)
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultTear, AfterWrites: 1, TearAt: 64, Once: true})
	_ = p.Commit() // tears; returns the injected error
	ff.Disarm()

	mainBytes := mem.Snapshot(dbPath)
	walBytes := mem.Snapshot(dbPath + "-wal")

	fs2 := loadFS(mainBytes, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	// The torn commit B is gone; the page still holds commit A's body.
	readBody(t, p2, id, 0xA0)
}

// TestDroppedWALWriteLosesOnlyThatCommit models the drive's volatile cache
// evaporating: a WriteAt is reported successful but its bytes never land. The
// affected commit is not recoverable; earlier commits are.
func TestDroppedWALWriteLosesOnlyThatCommit(t *testing.T) {
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	p := mustOpen(t, ff, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})

	id := allocFilled(t, p, 0x11)
	if err := p.Commit(); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	updateBody(t, p, id, 0x22)
	// Drop the payload write of commit B.
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultDrop, AfterWrites: 1, Once: true})
	_ = p.Commit()
	ff.Disarm()

	fs2 := loadFS(mem.Snapshot(dbPath), mem.Snapshot(dbPath+"-wal"))
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	readBody(t, p2, id, 0x11)
}

// TestFormatVersionGating reuses the format layer's version gate through the
// pager open path: a file whose major version is bumped fails to open.
func TestFormatVersionGating(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	allocFilled(t, p, 0x01)
	p.Commit()
	p.Close()

	// Corrupt the format_major to a future version and recompute the header
	// checksum so it is the version, not the checksum, that rejects the open.
	raw := fs.Snapshot(dbPath)
	hdr, err := format.DecodeHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	hdr.FormatMajor = format.FormatMajorCurrent + 1
	enc := hdr.Encode()
	copy(raw, enc)
	fs2 := loadFS(raw, fs.Snapshot(dbPath+"-wal"))
	if _, err := Open(fs2, dbPath, Options{}); !errors.Is(err, format.ErrUnsupportedMajor) {
		t.Fatalf("open err = %v, want ErrUnsupportedMajor", err)
	}
}
