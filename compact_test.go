package doc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCompactInMemoryPreservesData fills a collection, deletes most of it, compacts
// the in-memory database, and confirms the survivors, their _id index, and a
// secondary index all still resolve, with a clean check afterward.
func TestCompactInMemoryPreservesData(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	for i := 0; i < 300; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "age": i % 10}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: M{"age": 1}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	for i := 0; i < 300; i++ {
		if i%5 != 0 { // keep every fifth document
			if _, err := c.DeleteOne(ctx, M{"_id": i}); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
	}

	before, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	if err := db.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("document count changed across compact: before %d, after %d", before, after)
	}
	// A survivor is still found by _id and through the secondary index.
	if err := c.FindOne(ctx, M{"_id": 100}).Err(); err != nil {
		t.Fatalf("survivor lookup by _id: %v", err)
	}
	n, err := c.CountDocuments(ctx, M{"age": 0})
	if err != nil {
		t.Fatalf("count by indexed field: %v", err)
	}
	if n == 0 {
		t.Fatal("secondary index lookup found nothing after compact")
	}
	rep, err := db.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("check after compact: err=%v valid=%v", err, rep.Valid)
	}
}

// TestCompactReclaimsFileSpace writes a file-backed database, deletes most of it,
// and confirms compaction shrinks the file on disk while the survivors remain.
func TestCompactReclaimsFileSpace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "compact.doc")
	db, err := Open(path, WithSyncLevel(SyncOff))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	c := db.Database("d").Collection("c")
	big := make([]byte, 2000)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 800; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "blob": string(big)}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := 0; i < 800; i++ {
		if i >= 20 { // keep only the first twenty
			if _, err := c.DeleteOne(ctx, M{"_id": i}); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
	}

	sizeBefore := fileSize(t, path)
	if err := db.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	sizeAfter := fileSize(t, path)
	if sizeAfter >= sizeBefore {
		t.Fatalf("file did not shrink: before %d, after %d", sizeBefore, sizeAfter)
	}

	got, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 20 {
		t.Fatalf("survivors = %d, want 20", got)
	}
	rep, err := db.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("check after compact: err=%v valid=%v file=%v", err, rep.Valid, rep.FileProblems)
	}
}

// TestCompactSurvivesReopen confirms the rewritten file is durable: after compaction
// the database can be closed and reopened with its data intact.
func TestCompactSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.doc")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := db.Database("d").Collection("c")
	for i := 0; i < 100; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "v": i}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := db.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	got, err := db2.Database("d").Collection("c").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count after reopen: %v", err)
	}
	if got != 100 {
		t.Fatalf("documents after reopen = %d, want 100", got)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
