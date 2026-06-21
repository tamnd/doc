package doc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedForCheck fills a database with two databases, several collections, secondary
// indexes, and a mix of inserts and deletes, the shape a real check has to walk.
func seedForCheck(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	users := db.Database("shop").Collection("users")
	for i := 0; i < 200; i++ {
		if _, err := users.InsertOne(ctx, M{"_id": i, "age": i % 13, "city": "c" + itoa(i%5)}); err != nil {
			t.Fatalf("insert users: %v", err)
		}
	}
	if _, err := users.Indexes().CreateOne(ctx, IndexModel{Keys: M{"age": 1}}); err != nil {
		t.Fatalf("index age: %v", err)
	}
	if _, err := users.Indexes().CreateOne(ctx, IndexModel{Keys: M{"city": 1}}); err != nil {
		t.Fatalf("index city: %v", err)
	}
	for i := 0; i < 60; i++ {
		if _, err := users.DeleteOne(ctx, M{"_id": i * 3}); err != nil {
			t.Fatalf("delete users: %v", err)
		}
	}
	logs := db.Database("ops").Collection("logs")
	for i := 0; i < 50; i++ {
		if _, err := logs.InsertOne(ctx, M{"_id": i, "level": "info"}); err != nil {
			t.Fatalf("insert logs: %v", err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestCheckCleanDatabase runs both the cheap and the full check over a populated
// database and expects a clean verdict with accurate counts.
func TestCheckCleanDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedForCheck(t, db)

	for _, full := range []bool{false, true} {
		rep, err := db.Check(ctx, full)
		if err != nil {
			t.Fatalf("Check(full=%v): %v", full, err)
		}
		if !rep.Valid {
			t.Fatalf("Check(full=%v) not valid: file=%v colls=%+v", full, rep.FileProblems, rep.Collections)
		}
	}

	rep, _ := db.Check(ctx, true)
	got := map[string]int64{}
	for _, cc := range rep.Collections {
		got[cc.Namespace] = cc.Documents
	}
	if got["shop.users"] != 140 {
		t.Fatalf("shop.users documents = %d, want 140", got["shop.users"])
	}
	if got["ops.logs"] != 50 {
		t.Fatalf("ops.logs documents = %d, want 50", got["ops.logs"])
	}
}

// TestCheckClosedDatabase confirms Check on a closed handle returns ErrClosed.
func TestCheckClosedDatabase(t *testing.T) {
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.Check(context.Background(), false); err != ErrClosed {
		t.Fatalf("Check after close = %v, want ErrClosed", err)
	}
}

// TestCheckDetectsFileCorruption writes a file-backed database, flips a byte in a
// written data page, and confirms the full check reports the damage. The page that
// is corrupted is the highest non-empty one, which the open path does not read, so
// the database still opens and the checker is what surfaces the problem.
func TestCheckDetectsFileCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.doc")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedForCheck(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Fold the WAL into the main file so the corruption is not replayed away, then
	// drop the sidecar.
	_ = os.Remove(path + "-wal")

	const pageSize = 16384
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	pages := len(raw) / pageSize
	target := -1
	for p := pages - 1; p > 2; p-- {
		off := p * pageSize
		if !allZero(raw[off : off+pageSize]) {
			target = p
			break
		}
	}
	if target < 0 {
		t.Fatal("found no written data page to corrupt")
	}
	raw[target*pageSize+120] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// A page checksum is verified whenever the page is read. A page the open path
	// touches is rejected at Open, which is itself a detection; a page only the data
	// scan touches is caught by the full check. Either way the corruption must not
	// pass silently.
	db2, err := Open(path)
	if err != nil {
		if strings.Contains(err.Error(), "checksum") {
			return // detected at open, the file is refused before it can serve bad data
		}
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	rep, err := db2.Check(ctx, true)
	if err != nil {
		if strings.Contains(err.Error(), "checksum") {
			return
		}
		t.Fatalf("Check: %v", err)
	}
	if rep.Valid {
		t.Fatal("check should report the corrupted page")
	}
	if !mentionsChecksum(rep) {
		t.Fatalf("expected a checksum problem somewhere, got file=%v colls=%+v", rep.FileProblems, rep.Collections)
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// mentionsChecksum reports whether any problem string in the report names a
// checksum failure, at the file level or inside a collection.
func mentionsChecksum(rep *CheckReport) bool {
	for _, p := range rep.FileProblems {
		if strings.Contains(p, "checksum") {
			return true
		}
	}
	for _, cc := range rep.Collections {
		for _, p := range cc.HeapProblems {
			if strings.Contains(p, "checksum") {
				return true
			}
		}
		for _, ix := range cc.Indexes {
			for _, p := range ix.Problems {
				if strings.Contains(p, "checksum") {
					return true
				}
			}
		}
	}
	return false
}
