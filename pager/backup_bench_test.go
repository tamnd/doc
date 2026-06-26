package pager

import (
	"io"
	"testing"

	"github.com/tamnd/doc/vfs"
)

// BenchmarkBackup measures streaming a fixed-size image: checkpoint then copy every
// page to a discarding writer. The pages are allocated once outside the timer.
func BenchmarkBackup(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 8192, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 2000; i++ {
		_, f, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		p.MarkDirty(f)
		p.Unpin(f)
	}
	if err := p.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Backup(io.Discard, false, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBackupVerify measures the same copy with per-page checksum verification
// turned on, the cost of the --verify path.
func BenchmarkBackupVerify(b *testing.B) {
	fs := vfs.NewMemFS()
	p, err := Open(fs, dbPath, Options{Sync: SyncNormal, PoolPages: 8192, CheckpointFrames: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 2000; i++ {
		_, f, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		p.MarkDirty(f)
		p.Unpin(f)
	}
	if err := p.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Backup(io.Discard, true, nil); err != nil {
			b.Fatal(err)
		}
	}
}
