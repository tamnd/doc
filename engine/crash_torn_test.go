package engine

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/vfs"
)

// This file covers the torn-write injection of spec 2061 doc 19 §17.3. A torn
// write is a partial page write: the storage controller persists the first part
// of a write and loses the rest at power loss. The WAL's chained frame checksum
// must reject a torn frame, and a torn main-file page must be repaired from the
// WAL full-page image, so recovery always lands on either the clean pre-commit
// state or the fully-applied commit, never a corrupt in-between.

// tornBaseImage is one clean durable image plus the count of documents in it.
type tornBaseImage struct {
	main []byte
	wal  []byte
	docs int
}

// buildTornBase inserts seed documents, committing each cleanly, then returns the
// durable bytes. Every torn scenario restores from this same base so the injected
// commit is identical across offsets.
func buildTornBase(t *testing.T, path, db, coll string, seed int) tornBaseImage {
	t.Helper()
	mem := vfs.NewMemFS()
	e, err := Open(mem, path, crashOptions())
	if err != nil {
		t.Fatalf("base open: %v", err)
	}
	c, err := e.CreateCollection(db, coll)
	if err != nil {
		t.Fatalf("base create: %v", err)
	}
	for i := 1; i <= seed; i++ {
		if _, err := c.InsertOne(crashDoc(i)); err != nil {
			t.Fatalf("base insert %d: %v", i, err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("base close: %v", err)
	}
	return tornBaseImage{main: mem.Snapshot(path), wal: mem.Snapshot(path + "-wal"), docs: seed}
}

// TestTornWriteEveryOffset tears the next commit at a spread of write ordinals and
// byte offsets. For each scenario it restores the clean base, injects the torn
// write into the commit of one more document, then reopens and asserts recovery is
// atomic: the recovered set is either the base or the base plus the new document,
// the documents read back correctly, and the structural check passes.
func TestTornWriteEveryOffset(t *testing.T) {
	const db, coll, path = "shop", "orders", "torn.doc"
	base := buildTornBase(t, path, db, coll, 8)
	newID := base.docs + 1

	// Tear ordinals cover the first several writes of the commit; offsets cover
	// sector boundaries plus the small and large edges where a header or checksum
	// straddles the tear. -short trims the matrix.
	ordinals := []int{0, 1, 2, 3}
	offsets := []int{0, 1, 4, 64, 200, 512, 1024, 4096, 8191}
	if testing.Short() {
		ordinals = []int{0, 1}
		offsets = []int{0, 64, 512, 4096}
	}

	scenarios := 0
	torn := 0
	for _, ord := range ordinals {
		for _, off := range offsets {
			scenarios++
			if tornWriteScenario(t, base, path, db, coll, newID, ord, off) {
				torn++
			}
		}
	}
	t.Logf("ran %d torn-write scenarios (%d actually tore a write); recovery stayed atomic in all", scenarios, torn)
}

// tornWriteScenario runs one torn-write case and returns whether the injected tear
// actually fired (some ordinals fall past the end of the commit's write sequence).
func tornWriteScenario(t *testing.T, base tornBaseImage, path, db, coll string, newID, ordinal, tearAt int) bool {
	t.Helper()
	fs := loadCrashFS(crashImage{main: base.main, wal: base.wal}, path)
	ff := vfs.NewFaultFS(fs)
	e, err := Open(ff, path, crashOptions())
	if err != nil {
		t.Fatalf("ord %d off %d: open: %v", ordinal, tearAt, err)
	}
	c := e.GetCollection(db, coll)
	if c == nil {
		t.Fatalf("ord %d off %d: collection missing in base", ordinal, tearAt)
	}

	ff.Arm(vfs.FaultPlan{Mode: vfs.FaultTear, AfterWrites: ordinal, TearAt: tearAt, Once: true})
	// InsertOne auto-commits; the tear may surface as an error or be absorbed.
	_, _ = c.InsertOne(crashDoc(newID))
	ff.Disarm()
	fired := ff.Injected() > 0
	_ = e.Close()

	// Recover from the bytes that survived the torn write.
	rfs := loadCrashFS(crashImage{main: fs.Snapshot(path), wal: fs.Snapshot(path + "-wal")}, path)
	re, err := Open(rfs, path, crashOptions())
	if err != nil {
		t.Fatalf("ord %d off %d: reopen: %v", ordinal, tearAt, err)
	}
	defer re.Close()

	rc := re.GetCollection(db, coll)
	if rc == nil {
		t.Fatalf("ord %d off %d: collection lost after recovery", ordinal, tearAt)
	}
	docs, err := rc.Find(bson.NewBuilder().Build())
	if err != nil {
		t.Fatalf("ord %d off %d: find: %v", ordinal, tearAt, err)
	}
	present := map[int]string{}
	for _, d := range docs {
		id, _ := d.Lookup("_id")
		v, _ := d.Lookup("v")
		present[int(id.Int32())] = v.StringValue()
	}

	// Atomicity: the recovered set is the base (newID absent) or the base plus the
	// new document, and never a torn subset.
	switch len(present) {
	case base.docs, base.docs + 1:
	default:
		t.Fatalf("ord %d off %d: recovered %d docs, want %d or %d (torn write was not atomic)", ordinal, tearAt, len(present), base.docs, base.docs+1)
	}
	for seq := 1; seq <= base.docs; seq++ {
		if got := present[seq]; got != crashValue(seq) {
			t.Fatalf("ord %d off %d: base doc %d reads %q, want %q (torn write corrupted a prior commit)", ordinal, tearAt, seq, got, crashValue(seq))
		}
	}
	if v, ok := present[newID]; ok && v != crashValue(newID) {
		t.Fatalf("ord %d off %d: new doc reads %q, want %q", ordinal, tearAt, v, crashValue(newID))
	}
	if rep := re.Check(true); !rep.Valid {
		t.Fatalf("ord %d off %d: doc check failed after recovery: %+v", ordinal, tearAt, rep)
	}
	return fired
}
