package index

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

// loadFS rebuilds a filesystem from durable snapshots of the main and WAL files,
// modeling a crash where only persisted bytes survive.
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

func recoverAfterCrash(t *testing.T, mem *vfs.MemFS) (*pager.Pager, *BTree) {
	t.Helper()
	fs2 := loadFS(mem.Snapshot(dbPath), mem.Snapshot(dbPath+"-wal"))
	return openTree(t, fs2, pager.Options{Sync: pager.SyncFull})
}

// TestCommittedIndexEntriesSurviveCrash builds a multi-level tree, commits, then
// recovers from the durable bytes without a clean close. Every committed entry,
// and the persisted catalog root, must come back. This is the index half of the
// M1 durability criterion (spec 2061 doc 19 §22 M1 exit).
func TestCommittedIndexEntriesSurviveCrash(t *testing.T) {
	mem := vfs.NewMemFS()
	_, bt := openTree(t, mem, pager.Options{Sync: pager.SyncFull, CheckpointFrames: 1 << 30})

	const n = 1500
	tx := bt.Begin()
	for i := 0; i < n; i++ {
		if err := bt.Put(tx, EncodeObjectID(oid(uint32(i))), rid(uint32(i)+1, 0)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Crash: recover from the durable snapshot.
	p2, bt2 := recoverAfterCrash(t, mem)
	defer p2.Close()
	if got := bt2.Stats().Entries; got != n {
		t.Fatalf("recovered entries = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		got := mustGet(t, bt2, EncodeObjectID(oid(uint32(i))))
		if got != rid(uint32(i)+1, 0) {
			t.Fatalf("recovered get %d = %+v", i, got)
		}
	}
}

// TestTornIndexCommitDiscardedByRecovery tears the WAL payload write of an index
// commit. Recovery's chained checksum rejects the torn frame, so the committed
// prefix survives unchanged and the torn insert vanishes (no half-applied split).
func TestTornIndexCommitDiscardedByRecovery(t *testing.T) {
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	_, bt := openTree(t, ff, pager.Options{Sync: pager.SyncFull, CheckpointFrames: 1 << 30})

	keep := EncodeObjectID(oid(1))
	putCommit(t, bt, keep, rid(2, 0))

	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultTear, AfterWrites: 1, TearAt: 64, Once: true})
	tx := bt.Begin()
	if err := bt.Put(tx, EncodeObjectID(oid(2)), rid(3, 0)); err != nil {
		t.Fatalf("put: %v", err)
	}
	_ = tx.Commit() // tears
	ff.Disarm()

	p2, bt2 := recoverAfterCrash(t, mem)
	defer p2.Close()
	if got := bt2.Stats().Entries; got != 1 {
		t.Fatalf("recovered entries = %d, want 1 (torn commit discarded)", got)
	}
	if got := mustGet(t, bt2, keep); got != rid(2, 0) {
		t.Fatalf("kept entry = %+v, want page 2", got)
	}
	rtx := bt2.BeginReadOnly()
	if _, err := bt2.Get(rtx, EncodeObjectID(oid(2))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("torn entry got %v, want ErrNotFound", err)
	}
}
