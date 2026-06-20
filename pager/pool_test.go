package pager

import (
	"testing"

	"github.com/tamnd/doc/vfs"
)

// admitMiss models a Fetch miss: obtain a frame (evicting if full) and admit it
// for pageID. It is the pool half of fetchLocked, used to drive the 2Q policy
// deterministically without file I/O.
func admitMiss(pl *pool, pageID uint64) *pool {
	f, _, _ := pl.obtainFrame()
	if f != nil {
		pl.admit(f, pageID)
	}
	return pl
}

// TestTwoQGhostPromotion drives the 2Q replacement policy directly: a page
// evicted from the probation FIFO leaves a ghost, and re-referencing it while
// the ghost is live promotes it to the hot am list (admit's ghost branch and
// removeGhost). A sequential scan, by contrast, never promotes — that is the
// scan-resistance 2Q exists for.
func TestTwoQGhostPromotion(t *testing.T) {
	// capacity 4, kin = 4/4 -> 1, ghostCap = 4/2 -> 2.
	pl := newPool(64, 4)
	admitMiss(pl, 1) // a1in: [1]
	admitMiss(pl, 2) // a1in: [2,1]
	admitMiss(pl, 3) // a1in: [3,2,1]
	admitMiss(pl, 4) // a1in: [4,3,2,1] (full)
	if pl.am.size != 0 {
		t.Fatalf("am should be empty before any re-reference, got %d", pl.am.size)
	}
	admitMiss(pl, 5) // miss, full: evict a1in tail (1) -> ghost{1}; a1in: [5,4,3,2]
	if _, ghost := pl.a1out[1]; !ghost {
		t.Fatal("page 1 should be in the ghost set after eviction from a1in")
	}
	// Re-reference 1 while its ghost is still live (ghostCap 2 survives evicting
	// one more page during this obtainFrame): it must promote to the hot am list.
	admitMiss(pl, 1)
	if pl.am.size != 1 {
		t.Fatalf("am size = %d, want 1 (page 1 promoted)", pl.am.size)
	}
	if _, ghost := pl.a1out[1]; ghost {
		t.Fatal("page 1's ghost entry should be cleared on promotion")
	}
	if pl.lookup(1) == nil || pl.lookup(1).loc != listAm {
		t.Fatal("page 1 should now live in the hot am list")
	}
	// A hit on an am page moves it to the MRU end without changing membership.
	pl.recordHit(pl.lookup(1))
	if pl.am.size != 1 || pl.am.head != pl.lookup(1) {
		t.Fatal("am hit should keep the page resident at the MRU end")
	}
}

// TestTwoQScanResistance confirms a pure sequential scan of distinct pages never
// pollutes the hot am list — every page stays in the probation FIFO.
func TestTwoQScanResistance(t *testing.T) {
	pl := newPool(64, 5)
	for id := uint64(1); id <= 100; id++ {
		admitMiss(pl, id)
	}
	if pl.am.size != 0 {
		t.Fatalf("am size = %d, want 0; a sequential scan must not promote", pl.am.size)
	}
}

// TestTwoQEndToEndReadable is the integration check: a hot subset re-referenced
// across a scan stays correct through all the promotion bookkeeping.
func TestTwoQEndToEndReadable(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, PoolPages: 8, CheckpointFrames: 1 << 20})
	defer p.Close()

	ids := make([]uint64, 20)
	for i := range ids {
		ids[i] = allocFilled(t, p, byte(i+1))
		if err := p.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	hot := ids[:2]
	for round := 0; round < 5; round++ {
		for _, id := range ids {
			f, _ := p.Fetch(id, false)
			p.Unpin(f)
			for _, h := range hot {
				hf, _ := p.Fetch(h, false)
				p.Unpin(hf)
			}
		}
	}
	for i, id := range ids {
		readBody(t, p, id, byte(i+1))
	}
}

func TestPoolExhaustionWhenAllPinned(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, PoolPages: 3, CheckpointFrames: 1 << 20})
	defer p.Close()

	// Allocate and keep pinned (do not unpin): the header takes one slot, leaving
	// two. A third pinned page must exhaust the pool.
	var held []*Frame
	for i := 0; i < 2; i++ {
		_, f, err := p.Allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		p.MarkDirty(f)
		held = append(held, f)
	}
	if _, _, err := p.Allocate(); err != ErrPoolExhausted {
		t.Fatalf("err = %v, want ErrPoolExhausted", err)
	}
	for _, f := range held {
		p.Unpin(f)
	}
}

func TestFrameAccessorsAndPagerSync(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	defer p.Close()

	if p.PageSize() != 8192 {
		t.Fatalf("page size = %d, want 8192", p.PageSize())
	}
	_, f, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if f.Pins() != 1 {
		t.Fatalf("pins = %d, want 1", f.Pins())
	}
	p.MarkDirty(f)
	if !f.Dirty() {
		t.Fatal("frame should be dirty after MarkDirty")
	}
	if f.PageLSN() == 0 {
		t.Fatal("page LSN should be nonzero after MarkDirty")
	}
	p.Unpin(f)
	if f.Pins() != 0 {
		t.Fatalf("pins = %d after unpin, want 0", f.Pins())
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// TestCorruptContentPageDetected verifies a bit flip in a non-header page is
// caught by the page checksum on read (the M0 checksum-detection criterion,
// surfaced through the pager).
func TestCorruptContentPageDetected(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	id := allocFilled(t, p, 0x44)
	p.Commit()
	p.Close()

	raw := fs.Snapshot(dbPath)
	off := int(id) * 8192
	raw[off+100] ^= 0xFF // flip a body byte; checksum must now fail
	fs2 := loadFS(raw, fs.Snapshot(dbPath+"-wal"))

	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	if _, err := p2.Fetch(id, false); err == nil {
		t.Fatal("fetch of a corrupted page should fail the checksum check")
	}
}
