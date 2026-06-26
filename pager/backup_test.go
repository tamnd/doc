package pager

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/vfs"
)

// TestBackupRoundTrip streams a pager image to a buffer and reopens it as a fresh
// file, confirming the copy is a complete, WAL-free database.
func TestBackupRoundTrip(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	ids := make([]uint64, 0, 8)
	bytesByID := map[uint64]byte{}
	for i := 0; i < 8; i++ {
		b := byte(0x10 + i)
		id := allocFilled(t, p, b)
		ids = append(ids, id)
		bytesByID[id] = b
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var buf bytes.Buffer
	info, err := p.Backup(&buf, true, nil)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if info.Pages == 0 || info.Bytes == 0 {
		t.Fatalf("empty backup info: %+v", info)
	}
	if int(info.Bytes) != buf.Len() {
		t.Fatalf("backup reported %d bytes, wrote %d", info.Bytes, buf.Len())
	}
	p.Close()

	// The image alone, with no WAL, must reopen and yield every committed page.
	rfs := loadFS(buf.Bytes(), nil)
	rp := mustOpen(t, rfs, Options{Sync: SyncFull})
	defer rp.Close()
	for _, id := range ids {
		readBody(t, rp, id, bytesByID[id])
	}
}

// TestBackupProgressReportsEveryPage checks the progress callback advances to the
// full image size.
func TestBackupProgressReportsEveryPage(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	defer p.Close()
	for i := 0; i < 5; i++ {
		allocFilled(t, p, byte(i))
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var buf bytes.Buffer
	var lastWritten, lastTotal int64
	calls := 0
	info, err := p.Backup(&buf, false, func(written, total int64) {
		calls++
		lastWritten, lastTotal = written, total
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress was never called")
	}
	if lastWritten != lastTotal || lastWritten != info.Bytes {
		t.Fatalf("progress ended at %d/%d, want %d/%d", lastWritten, lastTotal, info.Bytes, info.Bytes)
	}
}
