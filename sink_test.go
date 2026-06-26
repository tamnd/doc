package doc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirSinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewDirSink(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("new dir sink: %v", err)
	}
	if err := sink.Put("seg-a.seg", []byte("alpha")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := sink.Put("seg-b.seg", []byte("bravo")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A non-segment file must not show up in List.
	if err := os.WriteFile(filepath.Join(dir, "wal", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := sink.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("list returned %v, want two .seg files", names)
	}
	got, err := sink.Get("seg-a.seg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "alpha" {
		t.Fatalf("get returned %q, want alpha", got)
	}
}

func TestArchiveWALRejectsSecondArchiver(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	a1, err := db.ArchiveWAL(WALArchiverOptions{Sink: newMemSink()})
	if err != nil {
		t.Fatalf("first archiver: %v", err)
	}
	defer a1.Stop()
	if _, err := db.ArchiveWAL(WALArchiverOptions{Sink: newMemSink()}); err != ErrArchiveRunning {
		t.Fatalf("second archiver error = %v, want ErrArchiveRunning", err)
	}
	// After stopping the first, a new archiver may start.
	a1.Stop()
	a2, err := db.ArchiveWAL(WALArchiverOptions{Sink: newMemSink()})
	if err != nil {
		t.Fatalf("restart after stop: %v", err)
	}
	a2.Stop()
}

func TestArchiveWALRejectsNilSinkAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.doc")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ArchiveWAL(WALArchiverOptions{}); err == nil {
		t.Fatal("nil sink should be rejected")
	}
	if _, err := db.Database("d").Collection("c").InsertOne(context.Background(), M{"_id": 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rdb, err := Open(path, WithReadOnly(true))
	if err != nil {
		t.Fatalf("reopen ro: %v", err)
	}
	defer func() { _ = rdb.Close() }()
	if _, err := rdb.ArchiveWAL(WALArchiverOptions{Sink: newMemSink()}); err != ErrReadOnly {
		t.Fatalf("read-only archive error = %v, want ErrReadOnly", err)
	}
}
