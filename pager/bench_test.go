package pager

import (
	"testing"

	"github.com/tamnd/doc/vfs"
)

// BenchmarkAllocateCommit measures the allocate + fill + durable-commit path
// (one page per commit), the single-document insert shape at the pager layer.
func BenchmarkAllocateCommit(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 4096, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, f, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		lo, hi := bodyRange(p.PageSize())
		for j := lo; j < hi; j++ {
			f.Buf[j] = byte(i)
		}
		p.MarkDirty(f)
		p.Unpin(f)
		if err := p.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFetchHot measures the buffer-pool hot-read path: a repeated fetch of a
// resident page (shard lookup, pin, policy touch).
func BenchmarkFetchHot(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 4096})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	id, f, _ := p.Allocate()
	p.MarkDirty(f)
	p.Unpin(f)
	p.Commit()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fr, err := p.Fetch(id, false)
		if err != nil {
			b.Fatal(err)
		}
		p.Unpin(fr)
	}
}

// BenchmarkFetchEvict measures the cold path: fetching across a working set far
// larger than the pool, forcing 2Q eviction and write-back on every miss.
func BenchmarkFetchEvict(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 64, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	const working = 4096
	ids := make([]uint64, working)
	for i := range ids {
		id, f, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		p.MarkDirty(f)
		p.Unpin(f)
		ids[i] = id
		// Commit each page so it becomes evictable; a tiny pool cannot hold an
		// unbounded set of uncommitted (unstealable) pages.
		if err := p.Commit(); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fr, err := p.Fetch(ids[i%working], false)
		if err != nil {
			b.Fatal(err)
		}
		p.Unpin(fr)
	}
}
