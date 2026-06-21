package doc

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCheckpointFoldsWAL confirms an online checkpoint folds the WAL into the main
// file without closing the database, leaving the data intact and the file clean.
func TestCheckpointFoldsWAL(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("d").Collection("c")
	for i := 0; i < 200; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "v": i}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	walBefore := db.eng.PagerStats().WALSizePages
	if walBefore == 0 {
		t.Fatal("expected a non-empty WAL before checkpoint")
	}
	if err := db.Checkpoint(ctx, ""); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if walAfter := db.eng.PagerStats().WALSizePages; walAfter != 0 {
		t.Fatalf("WAL size after checkpoint = %d pages, want 0", walAfter)
	}
	// Every document is still present, and the file checks clean.
	got, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 200 {
		t.Fatalf("documents after checkpoint = %d, want 200", got)
	}
	rep, err := db.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("check after checkpoint: err=%v valid=%v", err, rep.Valid)
	}
}

// TestCheckpointAcceptsModes confirms each SQLite checkpoint mode is accepted and an
// unknown mode is rejected.
func TestCheckpointAcceptsModes(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	for _, mode := range []string{"", "passive", "full", "restart", "truncate", "TRUNCATE"} {
		if err := db.Checkpoint(ctx, mode); err != nil {
			t.Fatalf("Checkpoint(%q): %v", mode, err)
		}
	}
	if err := db.Checkpoint(ctx, "bogus"); err == nil {
		t.Fatal("Checkpoint with an unknown mode should fail")
	}
}

// TestWalCheckpointPragma drives a checkpoint through PRAGMA wal_checkpoint and reads
// the WAL size back.
func TestWalCheckpointPragma(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	c := db.Database("d").Collection("c")
	for i := 0; i < 50; i++ {
		if _, err := c.InsertOne(ctx, M{"v": i}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := db.Pragma("wal_checkpoint", "checkpoint"); err != nil {
		t.Fatalf("pragma wal_checkpoint: %v", err)
	}
	if got, err := db.Pragma("wal_checkpoint", ""); err != nil || got != "0" {
		t.Fatalf("wal_checkpoint read = %q err=%v, want 0", got, err)
	}
}

// TestWalAutocheckpointPragma reads the default threshold, writes a new one, and
// rejects a negative value.
func TestWalAutocheckpointPragma(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Pragma("wal_autocheckpoint", "256"); err != nil {
		t.Fatalf("set wal_autocheckpoint: %v", err)
	}
	if got, err := db.Pragma("wal_autocheckpoint", ""); err != nil || got != "256" {
		t.Fatalf("wal_autocheckpoint read = %q err=%v, want 256", got, err)
	}
	if _, err := db.Pragma("wal_autocheckpoint", "-1"); err == nil {
		t.Fatal("negative wal_autocheckpoint should fail")
	}
}

// TestAutoVacuumPragma reads the default mode, accepts both the named and numeric
// spellings, and rejects an unknown value.
func TestAutoVacuumPragma(t *testing.T) {
	db := openTestDB(t)
	if got, err := db.Pragma("auto_vacuum", ""); err != nil || got != "none" {
		t.Fatalf("default auto_vacuum = %q err=%v, want none", got, err)
	}
	for in, want := range map[string]string{"incremental": "incremental", "2": "incremental", "1": "full", "full": "full", "0": "none", "none": "none"} {
		if got, err := db.Pragma("auto_vacuum", in); err != nil || got != want {
			t.Fatalf("auto_vacuum %q = %q err=%v, want %q", in, got, err, want)
		}
	}
	if _, err := db.Pragma("auto_vacuum", "sometimes"); err == nil {
		t.Fatal("unknown auto_vacuum value should fail")
	}
}

// TestIncrementalVacuumNoopWhenNone confirms vacuum reclaims nothing while auto_vacuum
// is none, matching SQLite.
func TestIncrementalVacuumNoopWhenNone(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("d").Collection("c")
	for i := 0; i < 100; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := 0; i < 100; i++ {
		if _, err := c.DeleteOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	n, err := db.IncrementalVacuum(ctx, 0)
	if err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed = %d, want 0 (auto_vacuum is none)", n)
	}
}

// TestIncrementalVacuumPragmaNoop drives the same no-op through the pragma surface.
func TestIncrementalVacuumPragmaNoop(t *testing.T) {
	db := openTestDB(t)
	if got, err := db.Pragma("incremental_vacuum", "0"); err != nil || got != "0" {
		t.Fatalf("incremental_vacuum = %q err=%v, want 0", got, err)
	}
	if _, err := db.Pragma("incremental_vacuum", "-5"); err == nil {
		t.Fatal("negative incremental_vacuum should fail")
	}
}

// TestIncrementalVacuumReclaimsFileSpace fills a file-backed database with a small
// collection to keep plus a large collection at the tail, drops the large one to free
// its pages, enables incremental auto_vacuum, and confirms a vacuum shrinks the file
// while the kept collection stays intact.
func TestIncrementalVacuumReclaimsFileSpace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vacuum.doc")
	db, err := Open(path, WithSyncLevel(SyncOff))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Documents above the spill threshold land in overflow page chains, the pages a
	// delete returns to the freelist. The kept documents are small and stay inline.
	small := make([]byte, 100)
	for i := range small {
		small[i] = 'k'
	}
	big := make([]byte, 50000)
	for i := range big {
		big[i] = 'x'
	}
	keep := db.Database("d").Collection("keep")
	for i := 0; i < 20; i++ {
		if _, err := keep.InsertOne(ctx, M{"_id": i, "blob": string(small)}); err != nil {
			t.Fatalf("insert keep: %v", err)
		}
	}
	// Large documents written last, so their overflow chains occupy the tail.
	tmp := db.Database("d").Collection("tmp")
	for i := 0; i < 300; i++ {
		if _, err := tmp.InsertOne(ctx, M{"_id": i, "blob": string(big)}); err != nil {
			t.Fatalf("insert tmp: %v", err)
		}
	}
	// Deleting them frees every overflow chain, leaving the file's tail reclaimable.
	if _, err := tmp.DeleteMany(ctx, M{}); err != nil {
		t.Fatalf("delete tmp: %v", err)
	}

	if _, err := db.Pragma("auto_vacuum", "incremental"); err != nil {
		t.Fatalf("set auto_vacuum: %v", err)
	}
	sizeBefore := fileSize(t, path)
	reclaimed, err := db.IncrementalVacuum(ctx, 0)
	if err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
	if reclaimed == 0 {
		t.Fatal("expected to reclaim trailing free pages")
	}
	sizeAfter := fileSize(t, path)
	if sizeAfter >= sizeBefore {
		t.Fatalf("file did not shrink: before %d, after %d (reclaimed %d)", sizeBefore, sizeAfter, reclaimed)
	}

	got, err := keep.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 20 {
		t.Fatalf("survivors = %d, want 20", got)
	}
	if err := keep.FindOne(ctx, M{"_id": 5}).Err(); err != nil {
		t.Fatalf("survivor lookup: %v", err)
	}
	rep, err := db.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("check after vacuum: err=%v valid=%v file=%v", err, rep.Valid, rep.FileProblems)
	}
}

// TestVacuumSurvivesReopen confirms the shrunken file is durable: after a vacuum the
// database closes and reopens with its survivors intact.
func TestVacuumSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vacuum-reopen.doc")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	small := make([]byte, 100)
	for i := range small {
		small[i] = 'k'
	}
	big := make([]byte, 50000)
	for i := range big {
		big[i] = 'x'
	}
	keep := db.Database("d").Collection("keep")
	for i := 0; i < 10; i++ {
		if _, err := keep.InsertOne(ctx, M{"_id": i, "blob": string(small)}); err != nil {
			t.Fatalf("insert keep: %v", err)
		}
	}
	tmp := db.Database("d").Collection("tmp")
	for i := 0; i < 200; i++ {
		if _, err := tmp.InsertOne(ctx, M{"_id": i, "blob": string(big)}); err != nil {
			t.Fatalf("insert tmp: %v", err)
		}
	}
	if _, err := tmp.DeleteMany(ctx, M{}); err != nil {
		t.Fatalf("delete tmp: %v", err)
	}
	if _, err := db.Pragma("auto_vacuum", "incremental"); err != nil {
		t.Fatalf("set auto_vacuum: %v", err)
	}
	if _, err := db.IncrementalVacuum(ctx, 0); err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	c2 := db2.Database("d").Collection("keep")
	got, err := c2.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count after reopen: %v", err)
	}
	if got != 10 {
		t.Fatalf("survivors after reopen = %d, want 10", got)
	}
	rep, err := db2.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("check after reopen: err=%v valid=%v", err, rep.Valid)
	}
}
