package doc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// seedForBackup fills a database with a secondary index, inline documents, and a few
// overflow-sized documents, the mix a backup has to carry faithfully.
func seedForBackup(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	c := db.Database("d").Collection("c")
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: M{"n": 1}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	for i := 0; i < 300; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i % 7}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	big := make([]byte, 50000)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 10; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": 1000 + i, "blob": string(big)}); err != nil {
			t.Fatalf("insert big: %v", err)
		}
	}
}

// backupToFile streams a backup to a fresh file under the test's temp dir and returns
// its path.
func backupToFile(t *testing.T, db *DB, opts BackupOptions) (string, BackupResult) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.doc")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	res, err := db.Backup(context.Background(), f, opts)
	if err != nil {
		_ = f.Close()
		t.Fatalf("Backup: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close backup: %v", err)
	}
	return path, res
}

// assertValidBackup opens the backup read-only, checks it, and confirms the document
// count matches what was seeded.
func assertValidBackup(t *testing.T, path string, wantDocs int64) {
	t.Helper()
	ctx := context.Background()
	bdb, err := Open(path, WithReadOnly(true))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = bdb.Close() }()
	rep, err := bdb.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("backup check: err=%v valid=%v file=%v", err, rep.Valid, rep.FileProblems)
	}
	got, err := bdb.Database("d").Collection("c").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count in backup: %v", err)
	}
	if got != wantDocs {
		t.Fatalf("backup document count = %d, want %d", got, wantDocs)
	}
}

func TestBackupProducesValidFile(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.doc"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	seedForBackup(t, db)

	path, res := backupToFile(t, db, BackupOptions{})
	if res.Pages == 0 || res.Bytes == 0 {
		t.Fatalf("empty backup result: %+v", res)
	}
	assertValidBackup(t, path, 310)
}

func TestBackupFromMemoryDatabase(t *testing.T) {
	db := openTestDB(t)
	seedForBackup(t, db)
	path, _ := backupToFile(t, db, BackupOptions{Verify: true})
	assertValidBackup(t, path, 310)
}

func TestBackupVerifyCatchesNothingOnCleanCopy(t *testing.T) {
	db := openTestDB(t)
	seedForBackup(t, db)
	// Verify reads every page checksum back; a clean copy passes it.
	path, res := backupToFile(t, db, BackupOptions{Verify: true})
	if res.Pages == 0 {
		t.Fatal("verify backup wrote no pages")
	}
	assertValidBackup(t, path, 310)
}

// TestBackupStaysWritable confirms the database keeps accepting writes while a backup
// streams: the writes run concurrently with Backup and all succeed, and the backup is
// still a valid image of the database as of its snapshot.
func TestBackupStaysWritable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedForBackup(t, db)
	c := db.Database("d").Collection("c")

	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	started := make(chan struct{})
	// Insert new documents from the first progress callback onward, so the writes
	// overlap the copy in time rather than racing to start before it.
	once := sync.Once{}
	go func() {
		defer wg.Done()
		<-started
		for i := 0; i < 200; i++ {
			if _, err := c.InsertOne(ctx, M{"_id": 5000 + i, "n": i}); err != nil {
				t.Errorf("concurrent insert %d: %v", i, err)
				return
			}
		}
	}()

	res, err := db.Backup(ctx, &buf, BackupOptions{
		Progress: func(written, total int64) { once.Do(func() { close(started) }) },
	})
	wg.Wait()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if res.Pages == 0 {
		t.Fatal("backup wrote no pages")
	}

	// Every concurrent write landed in the live database.
	live, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("live count: %v", err)
	}
	if live != 510 {
		t.Fatalf("live count = %d, want 510 (310 seeded + 200 concurrent)", live)
	}

	// The backup image opens and checks clean. It holds at least the seeded set; the
	// concurrent writes may or may not be in it depending on the snapshot boundary.
	path := filepath.Join(t.TempDir(), "concurrent.doc")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	bdb, err := Open(path, WithReadOnly(true))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = bdb.Close() }()
	rep, err := bdb.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("backup check: err=%v valid=%v", err, rep.Valid)
	}
	got, err := bdb.Database("d").Collection("c").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count in backup: %v", err)
	}
	if got < 310 {
		t.Fatalf("backup holds %d documents, want at least the 310 seeded", got)
	}
}

func TestBackupIncrementalUnsupported(t *testing.T) {
	db := openTestDB(t)
	seedForBackup(t, db)
	var buf bytes.Buffer
	_, err := db.Backup(context.Background(), &buf, BackupOptions{SinceVersion: 5})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SinceVersion backup error = %v, want ErrUnsupported", err)
	}
}

func TestBackupNilWriter(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Backup(context.Background(), nil, BackupOptions{}); err == nil {
		t.Fatal("backup with a nil writer should fail")
	}
}

// TestOfflineCopyReopens is the offline-copy guarantee (spec 2061 doc 18 §11.1): a
// plain byte copy of a cleanly closed file is a complete, restorable backup.
func TestOfflineCopyReopens(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.doc")
	db, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedForBackup(t, db)
	// A clean close checkpoints, so the .doc file is self-contained.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	dst := filepath.Join(dir, "copy.doc")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	assertValidBackup(t, dst, 310)
}
