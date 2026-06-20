package index

import (
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

func benchTree(b *testing.B, opts pager.Options) (*pager.Pager, *BTree) {
	b.Helper()
	p, err := pager.Open(vfs.NewMemFS(), dbPath, opts)
	if err != nil {
		b.Fatalf("pager open: %v", err)
	}
	bt, err := Open(p, collID, true)
	if err != nil {
		b.Fatalf("btree open: %v", err)
	}
	return p, bt
}

// seedTree inserts n entries under a single committed transaction.
func seedTree(b *testing.B, bt *BTree, n int) {
	b.Helper()
	tx := bt.Begin()
	for i := 0; i < n; i++ {
		if err := bt.Put(tx, EncodeObjectID(oid(uint32(i))), rid(uint32(i)+1, 0)); err != nil {
			b.Fatalf("seed put %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("seed commit: %v", err)
	}
}

// BenchmarkPutGroupCommit measures insert throughput amortized over one fsync per
// batch, the realistic write path.
func BenchmarkPutGroupCommit(b *testing.B) {
	p, bt := benchTree(b, pager.Options{Sync: pager.SyncNormal, PoolPages: 256})
	defer p.Close()

	b.ResetTimer()
	tx := bt.Begin()
	for i := 0; i < b.N; i++ {
		if err := bt.Put(tx, EncodeObjectID(oid(uint32(i))), rid(uint32(i)+1, 0)); err != nil {
			b.Fatalf("put: %v", err)
		}
		if (i+1)%1000 == 0 {
			if err := tx.Commit(); err != nil {
				b.Fatalf("commit: %v", err)
			}
			tx = bt.Begin()
		}
	}
	_ = tx.Commit()
}

// BenchmarkGet measures point-lookup latency against a warm 50k-entry tree.
func BenchmarkGet(b *testing.B) {
	const n = 50000
	p, bt := benchTree(b, pager.Options{Sync: pager.SyncOff, PoolPages: 512})
	defer p.Close()
	seedTree(b, bt, n)

	tx := bt.BeginReadOnly()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bt.Get(tx, EncodeObjectID(oid(uint32(i%n)))); err != nil {
			b.Fatalf("get: %v", err)
		}
	}
}

// BenchmarkScan measures full forward-scan throughput across a 50k-entry tree.
func BenchmarkScan(b *testing.B) {
	const n = 50000
	p, bt := benchTree(b, pager.Options{Sync: pager.SyncOff, PoolPages: 512})
	defer p.Close()
	seedTree(b, bt, n)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := bt.BeginReadOnly()
		cur, err := bt.Scan(tx, nil, nil, storage.ScanOpts{})
		if err != nil {
			b.Fatalf("scan: %v", err)
		}
		count := 0
		for cur.Next() {
			count++
		}
		cur.Close()
		if count != n {
			b.Fatalf("scanned %d, want %d", count, n)
		}
	}
}
