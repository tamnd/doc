package pager

import (
	"testing"

	"github.com/tamnd/doc/vfs"
)

// BenchmarkCheckpoint measures one online checkpoint: flush dirty pages, fold the
// WAL into the main file, fsync, and reset the WAL generation. Each iteration first
// dirties one page so the checkpoint has work to fold.
func BenchmarkCheckpoint(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 4096, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		_, f, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		p.MarkDirty(f)
		p.Unpin(f)
		if err := p.Commit(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := p.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIncrementalVacuum measures reclaiming a fixed run of trailing free pages.
// Each iteration rebuilds the same shape outside the timer: allocate a block, free
// its tail, then time the vacuum that truncates it.
func BenchmarkIncrementalVacuum(b *testing.B) {
	const block = 64
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 4096, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ids := make([]uint64, block)
		for j := range ids {
			id, f, err := p.Allocate()
			if err != nil {
				b.Fatal(err)
			}
			p.MarkDirty(f)
			p.Unpin(f)
			ids[j] = id
		}
		if err := p.Commit(); err != nil {
			b.Fatal(err)
		}
		for j := len(ids) - 1; j >= 0; j-- {
			if err := p.Free(ids[j]); err != nil {
				b.Fatal(err)
			}
		}
		if err := p.Commit(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := p.IncrementalVacuum(0); err != nil {
			b.Fatal(err)
		}
	}
}
