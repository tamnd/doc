package doc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkArchiveCapture measures the cost the commit observer adds on the write
// path: it inserts documents with an archiver attached and a no-op sink, so the timing
// reflects the in-memory frame capture rather than sink I/O.
func BenchmarkArchiveCapture(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	db, err := Open(filepath.Join(dir, "src.doc"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	arch, err := db.ArchiveWAL(WALArchiverOptions{Sink: discardSink{}})
	if err != nil {
		b.Fatalf("archive: %v", err)
	}
	defer arch.Stop()
	c := db.Database("d").Collection("c")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i, "s": "value"}); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
}

// BenchmarkApplyWALReplay measures restore throughput: it replays a fixed archive of
// commits onto a fresh base each iteration.
func BenchmarkApplyWALReplay(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	db, err := Open(filepath.Join(dir, "src.doc"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	c := db.Database("d").Collection("c")

	base := filepath.Join(dir, "base.doc")
	bf, err := os.Create(base)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Backup(ctx, bf, BackupOptions{}); err != nil {
		b.Fatalf("backup: %v", err)
	}
	_ = bf.Close()

	sink := newMemSink()
	arch, err := db.ArchiveWAL(WALArchiverOptions{Sink: sink})
	if err != nil {
		b.Fatalf("archive: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	arch.Flush()
	arch.Stop()
	if err := db.Close(); err != nil {
		b.Fatalf("close: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		out := filepath.Join(dir, "restore.doc")
		_ = os.Remove(out)
		if err := RestoreBase(base, out); err != nil {
			b.Fatalf("restore base: %v", err)
		}
		b.StartTimer()
		if _, err := ApplyWAL(out, sink, RestoreOptions{}); err != nil {
			b.Fatalf("apply wal: %v", err)
		}
	}
}

// discardSink is a WALSink that throws every segment away, for capture-cost benchmarks.
type discardSink struct{}

func (discardSink) Put(string, []byte) error   { return nil }
func (discardSink) List() ([]string, error)    { return nil, nil }
func (discardSink) Get(string) ([]byte, error) { return nil, nil }
