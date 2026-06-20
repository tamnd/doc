package wal

import (
	"testing"

	"github.com/tamnd/doc/vfs"
)

func benchWAL(b *testing.B) vfs.File {
	b.Helper()
	fs := vfs.NewMemFS()
	f, err := fs.Open("bench.doc-wal", vfs.OpenCreate)
	if err != nil {
		b.Fatalf("open wal: %v", err)
	}
	return f
}

// BenchmarkAppendCommitSingle measures the cost of appending and fsyncing a
// one-page commit - the single-document insert hot path.
func BenchmarkAppendCommitSingle(b *testing.B) {
	f := benchWAL(b)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2))
	if err != nil {
		b.Fatal(err)
	}
	pg := page(0x5A)
	frames := []PageImage{{PageID: 1, Payload: pg}}
	b.SetBytes(testPageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frames[0].PageID = uint64(i + 1)
		if _, _, err := w.AppendCommit(frames, uint32(i+2)); err != nil {
			b.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendCommitBatch measures a multi-page commit (a transaction that
// dirtied several pages) amortizing one fsync across the batch - the group-commit
// shape.
func BenchmarkAppendCommitBatch(b *testing.B) {
	const pages = 16
	f := benchWAL(b)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2))
	if err != nil {
		b.Fatal(err)
	}
	frames := make([]PageImage, pages)
	for i := range frames {
		frames[i] = PageImage{PageID: uint64(i + 1), Payload: page(byte(i))}
	}
	b.SetBytes(int64(pages) * testPageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := w.AppendCommit(frames, uint32(pages+1)); err != nil {
			b.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScan measures recovery throughput over a WAL holding many commits.
func BenchmarkScan(b *testing.B) {
	const commits = 256
	f := benchWAL(b)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2))
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < commits; i++ {
		if _, _, err := w.AppendCommit([]PageImage{{PageID: uint64(i + 1), Payload: page(byte(i))}}, uint32(i+2)); err != nil {
			b.Fatal(err)
		}
	}
	_ = w.Sync()
	b.SetBytes(int64(commits) * testPageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := Scan(f, testPageSize)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Committed) != commits {
			b.Fatalf("recovered %d, want %d", len(res.Committed), commits)
		}
	}
}
