package heap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

const dbPath = "test.doc"
const collID = 1

// makeDoc builds a structurally valid BSON document of exactly size bytes whose
// interior is filled with fill, so a round trip can be checked by content. The
// heap treats documents as opaque length-prefixed, NUL-terminated bytes in M1
// (the codec is M2), so this is sufficient to exercise every storage path.
func makeDoc(size int, fill byte) bson.Raw {
	if size < bson.MinDocLen {
		size = bson.MinDocLen
	}
	d := make(bson.Raw, size)
	binary.LittleEndian.PutUint32(d, uint32(size))
	for i := 4; i < size-1; i++ {
		d[i] = fill
	}
	d[size-1] = 0x00
	return d
}

func openHeap(t *testing.T, fs vfs.FS, opts pager.Options) (*pager.Pager, *Heap) {
	t.Helper()
	p, err := pager.Open(fs, dbPath, opts)
	if err != nil {
		t.Fatalf("pager open: %v", err)
	}
	h, err := Open(p, collID)
	if err != nil {
		t.Fatalf("heap open: %v", err)
	}
	return p, h
}

// insertCommit inserts one document in its own transaction and commits.
func insertCommit(t *testing.T, h *Heap, doc bson.Raw) storage.RID {
	t.Helper()
	tx := h.Begin()
	rid, err := h.Insert(tx, doc)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rid
}

func mustLookup(t *testing.T, h *Heap, rid storage.RID, want bson.Raw) {
	t.Helper()
	got, err := h.Lookup(h.BeginReadOnly(), rid)
	if err != nil {
		t.Fatalf("lookup %v: %v", rid, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("lookup %v: doc mismatch (got %d bytes, want %d)", rid, len(got), len(want))
	}
}

func TestInsertLookupRoundTrip(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})

	a := makeDoc(40, 0xA1)
	b := makeDoc(120, 0xB2)
	ra := insertCommit(t, h, a)
	rb := insertCommit(t, h, b)
	if ra == rb {
		t.Fatal("two inserts produced the same RID")
	}
	mustLookup(t, h, ra, a)
	mustLookup(t, h, rb, b)
}

func TestReopenPersists(t *testing.T) {
	fs := vfs.NewMemFS()
	p, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	docs := make([]bson.Raw, 50)
	rids := make([]storage.RID, 50)
	for i := range docs {
		docs[i] = makeDoc(30+i*3, byte(i+1))
		rids[i] = insertCommit(t, h, docs[i])
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2, h2 := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	defer p2.Close()
	for i := range docs {
		mustLookup(t, h2, rids[i], docs[i])
	}
	st := h2.FreeSpaceStats()
	if st.LiveRecords != 50 {
		t.Fatalf("live records after reopen = %d, want 50", st.LiveRecords)
	}
}

func TestScanReturnsEveryRecordOnce(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull, PoolPages: 8})
	const n = 200
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		d := makeDoc(50+(i%40), byte(i))
		insertCommit(t, h, d)
		seen[string(d)]++
	}
	cur, err := h.Scan(h.BeginReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
		seen[string(cur.Doc())]--
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if count != n {
		t.Fatalf("scan yielded %d records, want %d", count, n)
	}
	for d, c := range seen {
		if c != 0 {
			t.Fatalf("doc %x seen count off by %d", d[:4], c)
		}
	}
}

func TestUpdateInPlaceSameSize(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	rid := insertCommit(t, h, makeDoc(100, 0x11))

	tx := h.Begin()
	newDoc := makeDoc(100, 0x22)
	got, err := h.Update(tx, rid, newDoc)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got != rid {
		t.Fatalf("in-place update changed RID: %v -> %v", rid, got)
	}
	tx.Commit()
	mustLookup(t, h, rid, newDoc)
}

func TestUpdateShrinkStaysInPlace(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	rid := insertCommit(t, h, makeDoc(500, 0x33))

	tx := h.Begin()
	small := makeDoc(40, 0x44)
	got, err := h.Update(tx, rid, small)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got != rid {
		t.Fatalf("shrink changed RID: %v -> %v", rid, got)
	}
	tx.Commit()
	mustLookup(t, h, rid, small)
}

// TestUpdateGrowMovesAndForwards forces a grow-and-move: two documents share a
// page, then one grows past the page's remaining space, so it relocates and the
// canonical RID forwards to the new location transparently.
func TestUpdateGrowMovesAndForwards(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	a := makeDoc(4000, 0xAA)
	b := makeDoc(4000, 0xBB)
	ra := insertCommit(t, h, a)
	rb := insertCommit(t, h, b)
	if ra.PageNo != rb.PageNo {
		t.Fatalf("setup expected both docs on one page, got %v and %v", ra, rb)
	}

	tx := h.Begin()
	grown := makeDoc(6000, 0xCC)
	got, err := h.Update(tx, ra, grown)
	if err != nil {
		t.Fatalf("update grow: %v", err)
	}
	if got != ra {
		t.Fatalf("canonical RID changed on move: %v -> %v", ra, got)
	}
	tx.Commit()

	// The canonical RID still resolves, now to the grown content; b is untouched.
	mustLookup(t, h, ra, grown)
	mustLookup(t, h, rb, b)

	// Scan still yields exactly two records (the forwarding source is not double
	// counted).
	cur, _ := h.Scan(h.BeginReadOnly())
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
	}
	if count != 2 {
		t.Fatalf("scan after move yielded %d, want 2", count)
	}
}

func TestDeleteRemovesAndScanSkips(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	keep := insertCommit(t, h, makeDoc(60, 0x01))
	drop := insertCommit(t, h, makeDoc(60, 0x02))

	tx := h.Begin()
	if err := h.Delete(tx, drop); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tx.Commit()

	if _, err := h.Lookup(h.BeginReadOnly(), drop); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("lookup deleted: err = %v, want ErrNotFound", err)
	}
	cur, _ := h.Scan(h.BeginReadOnly())
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
		if cur.RID() == drop {
			t.Fatal("scan yielded a deleted record")
		}
	}
	if count != 1 {
		t.Fatalf("scan after delete yielded %d, want 1", count)
	}
	mustLookup(t, h, keep, makeDoc(60, 0x01))
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	ro := h.BeginReadOnly()
	if _, err := h.Insert(ro, makeDoc(10, 0x1)); !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("insert ro: err = %v, want ErrReadOnly", err)
	}
}

func TestRollbackUnsupportedInM1(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	tx := h.Begin()
	if err := tx.Rollback(); !errors.Is(err, ErrRollbackUnsupported) {
		t.Fatalf("rollback: err = %v, want ErrRollbackUnsupported", err)
	}
}
