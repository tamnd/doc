package heap

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

// loadFS rebuilds a fresh in-memory filesystem from snapshots of the main and WAL
// files, modeling a crash where only the bytes that reached the snapshot survive.
func loadFS(mainBytes, walBytes []byte) *vfs.MemFS {
	fs := vfs.NewMemFS()
	if mainBytes != nil {
		f, _ := fs.Open(dbPath, vfs.OpenCreate)
		f.WriteAt(mainBytes, 0)
		f.Close()
	}
	if walBytes != nil {
		f, _ := fs.Open(dbPath+"-wal", vfs.OpenCreate)
		f.WriteAt(walBytes, 0)
		f.Close()
	}
	return fs
}

// recoverAfterCrash snapshots the live files and reopens a pager+heap over copies,
// as a clean process restart against whatever durably landed would.
func recoverAfterCrash(t *testing.T, mem *vfs.MemFS) (*pager.Pager, *Heap) {
	t.Helper()
	fs2 := loadFS(mem.Snapshot(dbPath), mem.Snapshot(dbPath+"-wal"))
	return openHeap(t, fs2, pager.Options{Sync: pager.SyncFull})
}

// TestCommittedInsertsSurviveCrash inserts and commits a batch, then recovers from
// a snapshot taken at that point. Every committed record must be present and
// readable, satisfying the M1 durability criterion at the record-store layer.
func TestCommittedInsertsSurviveCrash(t *testing.T) {
	mem := vfs.NewMemFS()
	_, h := openHeap(t, mem, pager.Options{Sync: pager.SyncFull, CheckpointFrames: 1 << 30})

	const n = 1000
	rids := make([]storage.RID, n)
	want := make([]bson.Raw, n)
	for i := 0; i < n; i++ {
		want[i] = makeDoc(40+(i%120), byte(i))
		rids[i] = insertCommit(t, h, want[i])
	}

	// Crash: do not Close (no clean checkpoint); recover from the durable bytes.
	p2, h2 := recoverAfterCrash(t, mem)
	defer p2.Close()
	if got := h2.FreeSpaceStats().LiveRecords; got != n {
		t.Fatalf("recovered live records = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		got, err := h2.Lookup(h2.BeginReadOnly(), rids[i])
		if err != nil {
			t.Fatalf("recovered lookup %d: %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("recovered doc %d mismatch", i)
		}
	}
}

// TestCrashAtFsyncBoundaryRecoversConsistentPrefix arms an fsync failure on the
// commit after a durable prefix. Honest fsync semantics: the failed barrier does
// not erase bytes that already reached the WAL, so recovery yields the committed
// prefix and possibly the in-flight record too. The invariant under test is that
// recovery produces a consistent prefix with no torn or partial records: the
// durable prefix is wholly intact and any extra recovered record is itself whole
// (spec 2061 doc 19 §22 M1 exit).
func TestCrashAtFsyncBoundaryRecoversConsistentPrefix(t *testing.T) {
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	_, h := openHeap(t, ff, pager.Options{Sync: pager.SyncFull, CheckpointFrames: 1 << 30})

	// Commit a durable prefix one record at a time.
	const committed = 200
	rids := make([]storage.RID, committed)
	want := make([]bson.Raw, committed)
	for i := 0; i < committed; i++ {
		want[i] = makeDoc(64+(i%80), byte(i))
		rids[i] = insertCommit(t, h, want[i])
	}

	// The next commit's fsync fails: the application learns its durability is in
	// doubt via the error, but the WAL bytes may already be present.
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultSync, AfterSyncs: 0, Once: true})
	tx := h.Begin()
	if _, err := h.Insert(tx, makeDoc(99, 0xFF)); err != nil {
		t.Fatalf("insert before failed commit: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, vfs.ErrInjected) {
		t.Fatalf("commit err = %v, want the injected fsync failure", err)
	}
	ff.Disarm()

	p2, h2 := recoverAfterCrash(t, mem)
	defer p2.Close()
	got := h2.FreeSpaceStats().LiveRecords
	if got != committed && got != committed+1 {
		t.Fatalf("recovered live records = %d, want %d or %d (consistent prefix)", got, committed, committed+1)
	}
	// The whole durable prefix must be intact and readable regardless.
	for i := 0; i < committed; i++ {
		rec, err := h2.Lookup(h2.BeginReadOnly(), rids[i])
		if err != nil {
			t.Fatalf("recovered lookup %d: %v", i, err)
		}
		if !bytes.Equal(rec, want[i]) {
			t.Fatalf("recovered doc %d mismatch", i)
		}
	}
}

// TestTornWALInsertDiscardedByRecovery tears the WAL payload write of a heap
// commit. Recovery's chained checksum rejects the torn frame, so the prior
// committed state survives and the torn insert vanishes.
func TestTornWALInsertDiscardedByRecovery(t *testing.T) {
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	_, h := openHeap(t, ff, pager.Options{Sync: pager.SyncFull, CheckpointFrames: 1 << 30})

	keep := insertCommit(t, h, makeDoc(128, 0xA0))

	// Tear the second WAL write of the next commit (its page payload).
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultTear, AfterWrites: 1, TearAt: 64, Once: true})
	tx := h.Begin()
	if _, err := h.Insert(tx, makeDoc(128, 0xB0)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = tx.Commit() // tears
	ff.Disarm()

	p2, h2 := recoverAfterCrash(t, mem)
	defer p2.Close()
	if got := h2.FreeSpaceStats().LiveRecords; got != 1 {
		t.Fatalf("recovered live records = %d, want 1 (torn insert discarded)", got)
	}
	mustLookup(t, h2, keep, makeDoc(128, 0xA0))
}
