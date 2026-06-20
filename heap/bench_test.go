package heap

import (
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

func benchHeap(b *testing.B) (*pager.Pager, *Heap) {
	b.Helper()
	fs := vfs.NewMemFS()
	p, err := pager.Open(fs, dbPath, pager.Options{Sync: pager.SyncNormal, PoolPages: 4096, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatalf("pager open: %v", err)
	}
	h, err := Open(p, collID)
	if err != nil {
		b.Fatalf("heap open: %v", err)
	}
	return p, h
}

func BenchmarkInsertInline(b *testing.B) {
	p, h := benchHeap(b)
	defer p.Close()
	doc := makeDoc(120, 0x5A)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := h.Begin()
		if _, err := h.Insert(tx, doc); err != nil {
			b.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertBatchGroupCommit(b *testing.B) {
	p, h := benchHeap(b)
	defer p.Close()
	doc := makeDoc(120, 0x5A)
	b.ResetTimer()
	tx := h.Begin()
	for i := 0; i < b.N; i++ {
		if _, err := h.Insert(tx, doc); err != nil {
			b.Fatal(err)
		}
		if (i+1)%1000 == 0 {
			tx.Commit()
			tx = h.Begin()
		}
	}
	tx.Commit()
}

func BenchmarkLookup(b *testing.B) {
	p, h := benchHeap(b)
	defer p.Close()
	const n = 50000
	rids := seedBatch(b, h, n)
	ro := h.BeginReadOnly()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Lookup(ro, rids[i%n]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScan(b *testing.B) {
	p, h := benchHeap(b)
	defer p.Close()
	const n = 50000
	seedBatch(b, h, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cur, err := h.Scan(h.BeginReadOnly())
		if err != nil {
			b.Fatal(err)
		}
		for cur.Next() {
			_ = cur.Doc()
		}
		cur.Close()
	}
}

// seedBatch inserts n documents under a single committed transaction, so read
// benchmark setup is not dominated by per-insert fsync latency.
func seedBatch(b *testing.B, h *Heap, n int) []storage.RID {
	rids := make([]storage.RID, n)
	tx := h.Begin()
	for i := 0; i < n; i++ {
		rid, err := h.Insert(tx, makeDoc(120, byte(i)))
		if err != nil {
			b.Fatal(err)
		}
		rids[i] = rid
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return rids
}
