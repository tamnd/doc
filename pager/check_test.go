package pager

import (
	"strings"
	"testing"

	"github.com/tamnd/doc/vfs"
)

// TestCheckPagesCleanFile reports no problems on a freshly written, consistent
// file, in both the cheap freelist-only mode and the full checksum mode.
func TestCheckPagesCleanFile(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	defer p.Close()
	for i := range 8 {
		allocFilled(t, p, byte(i+1))
	}
	p.Commit()

	if probs := p.CheckPages(false); len(probs) != 0 {
		t.Fatalf("freelist check on a clean file: %v", probs)
	}
	if probs := p.CheckPages(true); len(probs) != 0 {
		t.Fatalf("full check on a clean file: %v", probs)
	}
}

// TestCheckPagesDetectsBadChecksum flips a body byte in an allocated page and
// confirms the full sweep reports the broken checksum, while the cheap freelist
// pass does not read page bodies and so stays silent.
func TestCheckPagesDetectsBadChecksum(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	id := allocFilled(t, p, 0x44)
	p.Commit()
	p.Close()

	raw := fs.Snapshot(dbPath)
	off := int(id) * 8192
	raw[off+100] ^= 0xFF
	fs2 := loadFS(raw, fs.Snapshot(dbPath+"-wal"))

	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()

	if probs := p2.CheckPages(false); len(probs) != 0 {
		t.Fatalf("freelist pass should not read bodies, got %v", probs)
	}
	probs := p2.CheckPages(true)
	if len(probs) == 0 {
		t.Fatal("full check should report the corrupted page")
	}
	joined := strings.Join(probs, "; ")
	if !strings.Contains(joined, "checksum") {
		t.Fatalf("expected a checksum problem, got %v", probs)
	}
}

// TestCheckPagesDetectsFreelistOverlap frees a page, then corrupts it on disk. The
// freelist walk reads each chained page, so it must surface the damaged free page
// rather than walk past it. (Damaging the type byte also breaks the page checksum,
// so the read fails first; either way the walk reports the page.)
func TestCheckPagesDetectsFreelistOverlap(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	id := allocFilled(t, p, 0x44)
	p.Commit()
	if err := p.Free(id); err != nil {
		t.Fatalf("free: %v", err)
	}
	p.Commit()
	p.Close()

	raw := fs.Snapshot(dbPath)
	off := int(id) * 8192
	// The page type is the first byte of the page header. Overwrite the free marker
	// with the heap type so the freelist walk sees a live page on its chain.
	raw[off] = 0x02
	fs2 := loadFS(raw, fs.Snapshot(dbPath+"-wal"))

	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	probs := p2.CheckPages(false)
	if len(probs) == 0 {
		t.Fatal("freelist walk should flag a live page on the free chain")
	}
}
