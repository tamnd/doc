package vfs

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
)

// fsFactory builds a fresh FS and a path within it for a conformance run.
type fsFactory struct {
	name string
	make func(t *testing.T) (FS, string)
}

func factories(t *testing.T) []fsFactory {
	return []fsFactory{
		{"memfs", func(t *testing.T) (FS, string) { return NewMemFS(), "db.doc" }},
		{"osfs", func(t *testing.T) (FS, string) {
			return NewOSFS(), filepath.Join(t.TempDir(), "db.doc")
		}},
	}
}

func TestFSConformance(t *testing.T) {
	for _, f := range factories(t) {
		t.Run(f.name, func(t *testing.T) {
			fs, path := f.make(t)

			exists, err := fs.Exists(path)
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if exists {
				t.Fatal("file should not exist yet")
			}

			file, err := fs.Open(path, OpenCreate)
			if err != nil {
				t.Fatalf("Open create: %v", err)
			}

			payload := []byte("hello, doc database")
			if n, err := file.WriteAt(payload, 0); err != nil || n != len(payload) {
				t.Fatalf("WriteAt = (%d,%v)", n, err)
			}
			if err := file.Sync(SyncFull); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			sz, err := file.Size()
			if err != nil || sz != int64(len(payload)) {
				t.Fatalf("Size = (%d,%v), want %d", sz, err, len(payload))
			}

			buf := make([]byte, len(payload))
			if _, err := file.ReadAt(buf, 0); err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			if string(buf) != string(payload) {
				t.Fatalf("read %q, want %q", buf, payload)
			}

			// Write at an offset, growing the file.
			if _, err := file.WriteAt([]byte("XYZ"), 100); err != nil {
				t.Fatalf("WriteAt offset 100: %v", err)
			}
			sz, _ = file.Size()
			if sz != 103 {
				t.Fatalf("Size after offset write = %d, want 103", sz)
			}

			// Truncate down and up.
			if err := file.Truncate(10); err != nil {
				t.Fatalf("Truncate down: %v", err)
			}
			sz, _ = file.Size()
			if sz != 10 {
				t.Fatalf("Size after truncate = %d, want 10", sz)
			}

			// Reading past EOF yields io.EOF.
			big := make([]byte, 1000)
			if _, err := file.ReadAt(big, 0); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt past EOF: err = %v, want io.EOF", err)
			}

			if err := file.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			exists, _ = fs.Exists(path)
			if !exists {
				t.Fatal("file should exist after creation")
			}

			if err := fs.Delete(path, false); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			exists, _ = fs.Exists(path)
			if exists {
				t.Fatal("file should be gone after delete")
			}
		})
	}
}

func TestOpenMissingWithoutCreate(t *testing.T) {
	fs := NewMemFS()
	if _, err := fs.Open("nope.doc", OpenRead); err == nil {
		t.Fatal("opening a missing file without OpenCreate should fail")
	}
}

func TestShmMapNotImplemented(t *testing.T) {
	for _, f := range factories(t) {
		fs, _ := f.make(t)
		if _, err := fs.ShmMap("p", 0, true); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s ShmMap err = %v, want ErrNotImplemented", f.name, err)
		}
	}
}

func TestMemFSSnapshotIndependent(t *testing.T) {
	fs := NewMemFS()
	file, _ := fs.Open("s.doc", OpenCreate)
	file.WriteAt([]byte("abcdef"), 0)
	snap := fs.Snapshot("s.doc")
	file.WriteAt([]byte("ZZZ"), 0)
	if string(snap) != "abcdef" {
		t.Fatalf("snapshot mutated to %q", snap)
	}
}
