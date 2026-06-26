package engine

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/vfs"
)

// This file covers the interrupted-checkpoint scenario of spec 2061 doc 19 §17.5.
// A checkpoint folds WAL frames into the main file and only then truncates the
// WAL. If a crash interrupts it after writing some of the frames, the main file is
// partially updated but the WAL is intact, so recovery replays the full WAL and
// the database reverts to the exact committed state from before the checkpoint
// began. The redo is idempotent, so the partially-written pages are simply
// rewritten.

// TestInterruptedCheckpoint accrues several commits in the WAL, then interrupts a
// checkpoint by failing the main-file write after a varying number of pages. After
// each interruption it reopens the database and asserts the full committed state
// is recovered and the structural check passes.
func TestInterruptedCheckpoint(t *testing.T) {
	const db, coll, path = "shop", "orders", "ckpt.doc"
	const docs = 30

	afterWrites := []int{0, 1, 2, 3, 5, 8}
	if testing.Short() {
		afterWrites = []int{0, 1, 3}
	}

	interrupted := 0
	for _, after := range afterWrites {
		if interruptCheckpointScenario(t, path, db, coll, docs, after) {
			interrupted++
		}
	}
	if interrupted == 0 {
		t.Fatal("no checkpoint was actually interrupted; the fault never fired")
	}
	t.Logf("recovered the full committed state across %d checkpoint interruptions (%d fired a fault)", len(afterWrites), interrupted)
}

// interruptCheckpointScenario builds a fresh WAL with docs commits, interrupts the
// checkpoint after `after` main-file writes, reopens, and verifies recovery. It
// returns whether the fault actually fired.
func interruptCheckpointScenario(t *testing.T, path, db, coll string, docs, after int) bool {
	t.Helper()
	mem := vfs.NewMemFS()
	ff := vfs.NewFaultFS(mem)
	e, err := Open(ff, path, crashOptions())
	if err != nil {
		t.Fatalf("after %d: open: %v", after, err)
	}
	c, err := e.CreateCollection(db, coll)
	if err != nil {
		t.Fatalf("after %d: create: %v", after, err)
	}
	for i := 1; i <= docs; i++ {
		if _, err := c.InsertOne(crashDoc(i)); err != nil {
			t.Fatalf("after %d: insert %d: %v", after, i, err)
		}
	}

	// Interrupt the checkpoint partway through folding frames into the main file.
	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultWrite, AfterWrites: after, Once: true})
	_ = e.Checkpoint()
	ff.Disarm()
	fired := ff.Injected() > 0

	main := mem.Snapshot(path)
	wal := mem.Snapshot(path + "-wal")
	_ = e.Close()

	re, err := Open(loadCrashFS(crashImage{main: main, wal: wal}, path), path, crashOptions())
	if err != nil {
		t.Fatalf("after %d: reopen: %v", after, err)
	}
	defer re.Close()

	rc := re.GetCollection(db, coll)
	if rc == nil {
		t.Fatalf("after %d: collection lost after interrupted checkpoint", after)
	}
	got, err := rc.Find(bson.NewBuilder().Build())
	if err != nil {
		t.Fatalf("after %d: find: %v", after, err)
	}
	if len(got) != docs {
		t.Fatalf("after %d: recovered %d docs, want %d; the interrupted checkpoint did not revert to the pre-checkpoint committed state", after, len(got), docs)
	}
	for _, d := range got {
		id, _ := d.Lookup("_id")
		v, _ := d.Lookup("v")
		if seq := int(id.Int32()); v.StringValue() != crashValue(seq) {
			t.Fatalf("after %d: doc %d reads %q, want %q", after, seq, v.StringValue(), crashValue(seq))
		}
	}
	if rep := re.Check(true); !rep.Valid {
		t.Fatalf("after %d: doc check failed after recovery: %+v", after, rep)
	}
	return fired
}
