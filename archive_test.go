package doc

import (
	"context"
	"os"
	"path/filepath"
	"time"
	"testing"
)

// memSink is an in-memory WALSink for tests.
type memSink struct {
	data map[string][]byte
}

func newMemSink() *memSink { return &memSink{data: map[string][]byte{}} }

func (m *memSink) Put(name string, b []byte) error {
	cp := make([]byte, len(b))
	copy(cp, b)
	m.data[name] = cp
	return nil
}

func (m *memSink) List() ([]string, error) {
	names := make([]string, 0, len(m.data))
	for n := range m.data {
		names = append(names, n)
	}
	return names, nil
}

func (m *memSink) Get(name string) ([]byte, error) { return m.data[name], nil }

// archiveTo opens a fresh database, backs up an empty base, archives two batches of
// inserts, and returns the base path, the sink, the version after the first batch,
// and the cluster time recorded at that point.
func archiveTo(t *testing.T, dir string) (base string, sink *memSink, midVer uint64) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(dir, "src.doc")
	db, err := Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("d").Collection("c")

	base = filepath.Join(dir, "base.doc")
	bf, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(ctx, bf, BackupOptions{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	_ = bf.Close()

	sink = newMemSink()
	arch, err := db.ArchiveWAL(WALArchiverOptions{Sink: sink})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			t.Fatalf("batch A insert: %v", err)
		}
	}
	midVer = db.CurrentVersion()
	for i := 100; i < 150; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			t.Fatalf("batch B insert: %v", err)
		}
	}
	arch.Flush()
	arch.Stop()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return base, sink, midVer
}

func TestPITRToVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base, sink, midVer := archiveTo(t, dir)

	out := filepath.Join(dir, "pitr.doc")
	if err := RestoreBase(base, out); err != nil {
		t.Fatalf("restore base: %v", err)
	}
	if _, err := ApplyWAL(out, sink, RestoreOptions{TargetVersion: midVer}); err != nil {
		t.Fatalf("apply wal: %v", err)
	}

	rdb, err := Open(out, WithReadOnly(true))
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = rdb.Close() }()
	col := rdb.Database("d").Collection("c")
	got, err := col.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 50 {
		t.Fatalf("PITR to mid version gave %d docs, want 50 (only batch A)", got)
	}
	// A batch B document must not be present at the earlier target.
	n, err := col.CountDocuments(ctx, M{"_id": 100})
	if err != nil {
		t.Fatalf("count _id 100: %v", err)
	}
	if n != 0 {
		t.Fatalf("batch B document present after PITR to mid version")
	}
}

func TestPITRTimeBounds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base, sink, _ := archiveTo(t, dir)

	// A target far in the past applies nothing; the file stays at the empty base.
	past := filepath.Join(dir, "past.doc")
	if err := RestoreBase(base, past); err != nil {
		t.Fatalf("restore base past: %v", err)
	}
	pres, err := ApplyWAL(past, sink, RestoreOptions{TargetTime: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("apply past: %v", err)
	}
	if pres.AppliedCommits != 0 {
		t.Fatalf("a past target applied %d commits, want 0", pres.AppliedCommits)
	}

	// A target far in the future applies everything.
	future := filepath.Join(dir, "future.doc")
	if err := RestoreBase(base, future); err != nil {
		t.Fatalf("restore base future: %v", err)
	}
	if _, err := ApplyWAL(future, sink, RestoreOptions{TargetTime: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("apply future: %v", err)
	}
	fdb, err := Open(future, WithReadOnly(true))
	if err != nil {
		t.Fatalf("open future: %v", err)
	}
	defer func() { _ = fdb.Close() }()
	got, err := fdb.Database("d").Collection("c").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 100 {
		t.Fatalf("future target gave %d docs, want 100 (both batches)", got)
	}
}

func TestArchiveAndRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.doc")
	db, err := Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("d").Collection("c")

	// Seed a base, then take a full backup of it.
	for i := 0; i < 50; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	base := filepath.Join(dir, "base.doc")
	bf, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(ctx, bf, BackupOptions{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	_ = bf.Close()

	// Start archiving, then write more documents past the base.
	sink := newMemSink()
	arch, err := db.ArchiveWAL(WALArchiverOptions{Sink: sink})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	for i := 100; i < 200; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			t.Fatalf("post-base insert: %v", err)
		}
	}
	arch.Flush()
	st := arch.Stats()
	if st.Commits == 0 || st.Segments == 0 {
		t.Fatalf("archiver captured nothing: %+v", st)
	}
	arch.Stop()
	if err := db.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}

	// Restore: base, then replay all archived WAL.
	out := filepath.Join(dir, "restored.doc")
	if err := RestoreBase(base, out); err != nil {
		t.Fatalf("restore base: %v", err)
	}
	res, err := ApplyWAL(out, sink, RestoreOptions{})
	if err != nil {
		t.Fatalf("apply wal: %v", err)
	}
	if res.AppliedCommits == 0 {
		t.Fatal("restore applied no commits")
	}

	rdb, err := Open(out, WithReadOnly(true))
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = rdb.Close() }()
	rep, err := rdb.Check(ctx, true)
	if err != nil || !rep.Valid {
		t.Fatalf("restored check: err=%v valid=%v", err, rep.Valid)
	}
	got, err := rdb.Database("d").Collection("c").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 150 {
		t.Fatalf("restored doc count = %d, want 150 (50 base + 100 archived)", got)
	}
}
