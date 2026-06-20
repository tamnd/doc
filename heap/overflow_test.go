package heap

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

// TestOverflowRoundTrip stores documents larger than the spill threshold, which
// must spill to overflow chains, and verifies they read back byte for byte both
// while resident and after a reopen.
func TestOverflowRoundTrip(t *testing.T) {
	fs := vfs.NewMemFS()
	p, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull, PoolPages: 64})

	sizes := []int{SpillThreshold + 1, 9000, 32 << 10, 200 << 10}
	rids := make([]storage.RID, len(sizes))
	want := make([][]byte, len(sizes))
	for i, sz := range sizes {
		d := makeDoc(sz, byte(0x40+i))
		want[i] = d
		rids[i] = insertCommit(t, h, d)
	}
	for i := range sizes {
		mustLookup(t, h, rids[i], want[i])
	}

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	p2, h2 := openHeap(t, fs, pager.Options{Sync: pager.SyncFull, PoolPages: 64})
	defer p2.Close()
	for i := range sizes {
		mustLookup(t, h2, rids[i], want[i])
	}
}

// TestUpdateInlineToOverflowAndBack exercises the two cross-representation update
// transitions: an inline document grown past the spill threshold (now an overflow
// chain) and an overflow document shrunk back inline (its chain must be freed).
func TestUpdateInlineToOverflowAndBack(t *testing.T) {
	fs := vfs.NewMemFS()
	p, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull, PoolPages: 64})

	rid := insertCommit(t, h, makeDoc(100, 0x01))
	before := h.pgr.PageCount()

	// Grow inline -> overflow (overflow pages are not heap pages, so the file's
	// total page count grows even though FreeSpaceStats.PageCount does not).
	big := makeDoc(40<<10, 0x02)
	tx := h.Begin()
	if _, err := h.Update(tx, rid, big); err != nil {
		t.Fatalf("grow to overflow: %v", err)
	}
	tx.Commit()
	mustLookup(t, h, rid, big)
	grew := h.pgr.PageCount()
	if grew <= before {
		t.Fatalf("overflow update did not allocate pages: before=%d after=%d", before, grew)
	}

	// Shrink overflow -> inline; the chain pages are returned to the freelist.
	small := makeDoc(80, 0x03)
	tx2 := h.Begin()
	if _, err := h.Update(tx2, rid, small); err != nil {
		t.Fatalf("shrink to inline: %v", err)
	}
	tx2.Commit()
	mustLookup(t, h, rid, small)

	// The freed overflow pages are reusable: a fresh large doc reuses them rather
	// than growing the file unboundedly.
	freedHigh := h.pgr.PageCount()
	big2 := makeDoc(40<<10, 0x04)
	insertCommit(t, h, big2)
	if h.pgr.PageCount() > freedHigh {
		// Some growth is acceptable, but it must not exceed the prior high-water mark
		// by more than the chain length (i.e. the freelist was consulted).
		if h.pgr.PageCount() > freedHigh+2 {
			t.Fatalf("freed overflow pages were not reused: high=%d now=%d", freedHigh, h.pgr.PageCount())
		}
	}

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestDeleteRecyclesSpace confirms a page's space is reclaimed after a delete:
// inserting a same-sized record afterward does not grow the file.
func TestDeleteRecyclesSpace(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})

	// Fill one page with several records, then delete one and reinsert.
	var rids []storage.RID
	for i := 0; i < 10; i++ {
		rids = append(rids, insertCommit(t, h, makeDoc(200, byte(i))))
	}
	pagesBefore := h.pgr.PageCount()

	tx := h.Begin()
	if err := h.Delete(tx, rids[3]); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tx.Commit()

	// Reinsert a same-sized record; it should land in the recycled space without
	// extending the file.
	insertCommit(t, h, makeDoc(200, 0xEE))
	if h.pgr.PageCount() != pagesBefore {
		t.Fatalf("file grew after reusing freed slot space: before=%d after=%d", pagesBefore, h.pgr.PageCount())
	}
}

// TestManyInsertsReadableAfterReopen is the scaled M1 exit criterion: insert a
// large batch of documents, close, reopen, and verify every one is readable. The
// count is kept modest so the suite stays fast under -race; the millions-scale run
// lives in the benchmark.
func TestManyInsertsReadableAfterReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	p, h := openHeap(t, fs, pager.Options{Sync: pager.SyncNormal, PoolPages: 256})

	const n = 20000
	rids := make([]storage.RID, n)
	for i := 0; i < n; i++ {
		// Vary sizes and sprinkle in overflow documents.
		sz := 32 + (i % 300)
		if i%500 == 0 {
			sz = SpillThreshold + 100
		}
		rids[i] = insertCommit(t, h, makeDoc(sz, byte(i)))
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2, h2 := openHeap(t, fs, pager.Options{Sync: pager.SyncNormal, PoolPages: 256})
	defer p2.Close()
	if got := h2.FreeSpaceStats().LiveRecords; got != n {
		t.Fatalf("live records after reopen = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		want := makeDoc(32+(i%300), byte(i))
		if i%500 == 0 {
			want = makeDoc(SpillThreshold+100, byte(i))
		}
		got, err := h2.Lookup(h2.BeginReadOnly(), rids[i])
		if err != nil {
			t.Fatalf("lookup %d after reopen: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("doc %d mismatch after reopen", i)
		}
	}
}
