package index

import (
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

// TestManyInsertsForceSplitsAndStayReadable inserts enough ObjectId keys to grow
// the tree past one leaf (forcing leaf and interior splits), then verifies every
// key still resolves and a full scan returns them in order exactly once.
func TestManyInsertsForceSplitsAndStayReadable(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{Sync: pager.SyncOff, PoolPages: 64})
	defer p.Close()

	const n = 5000
	tx := bt.Begin()
	for i := 0; i < n; i++ {
		key := EncodeObjectID(oid(uint32(i)))
		if err := bt.Put(tx, key, rid(uint32(i)+1, uint16(i%97))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	st := bt.Stats()
	if st.Entries != n {
		t.Fatalf("Stats.Entries = %d, want %d", st.Entries, n)
	}
	if st.Height < 2 {
		t.Fatalf("Stats.Height = %d, expected splits to grow height >= 2", st.Height)
	}

	// Every key resolves to its RID.
	for i := 0; i < n; i++ {
		got := mustGet(t, bt, EncodeObjectID(oid(uint32(i))))
		want := rid(uint32(i)+1, uint16(i%97))
		if got != want {
			t.Fatalf("get %d = %+v, want %+v", i, got, want)
		}
	}

	// A full scan yields every key once, strictly ascending.
	stx := bt.BeginReadOnly()
	cur, err := bt.Scan(stx, nil, nil, storage.ScanOpts{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	count := 0
	var prev storage.IndexKey
	for cur.Next() {
		k := append(storage.IndexKey(nil), cur.Key()...)
		if prev != nil && string(k) <= string(prev) {
			t.Fatalf("scan not strictly ascending at %d: %x then %x", count, prev, k)
		}
		prev = k
		count++
	}
	if cur.Err() != nil {
		t.Fatalf("cursor err: %v", cur.Err())
	}
	if count != n {
		t.Fatalf("scan returned %d entries, want %d", count, n)
	}
}

// TestReopenPersistsRoot writes a split tree, closes the pager, reopens, and
// confirms the catalog-root header slot rehydrated the same tree.
func TestReopenPersistsRoot(t *testing.T) {
	mem := vfs.NewMemFS()

	func() {
		p, bt := openTree(t, mem, pager.Options{Sync: pager.SyncNormal, PoolPages: 64})
		defer p.Close()
		tx := bt.Begin()
		for i := 0; i < 2000; i++ {
			if err := bt.Put(tx, EncodeObjectID(oid(uint32(i))), rid(uint32(i)+1, 0)); err != nil {
				t.Fatalf("put %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}()

	p2, bt2 := openTree(t, mem, pager.Options{Sync: pager.SyncNormal, PoolPages: 64})
	defer p2.Close()
	if bt2.Stats().Entries != 2000 {
		t.Fatalf("after reopen Entries = %d, want 2000", bt2.Stats().Entries)
	}
	for i := 0; i < 2000; i++ {
		got := mustGet(t, bt2, EncodeObjectID(oid(uint32(i))))
		if got != rid(uint32(i)+1, 0) {
			t.Fatalf("after reopen get %d = %+v", i, got)
		}
	}
}

// TestScanRange exercises bounded scans with inclusive/exclusive endpoints.
func TestScanRange(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	for i := int64(0); i < 10; i++ {
		putCommit(t, bt, EncodeInt64(i), rid(uint32(i)+1, 0))
	}

	collect := func(lo, hi storage.IndexKey, opts storage.ScanOpts) []int64 {
		tx := bt.BeginReadOnly()
		cur, err := bt.Scan(tx, lo, hi, opts)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		defer cur.Close()
		var out []int64
		for cur.Next() {
			out = append(out, decodeNumKey(t, cur.Key()))
		}
		return out
	}

	// [3, 7): include lo, exclude hi -> 3,4,5,6
	got := collect(EncodeInt64(3), EncodeInt64(7), storage.ScanOpts{IncludeLo: true})
	if !eqI64(got, []int64{3, 4, 5, 6}) {
		t.Fatalf("[3,7) = %v", got)
	}
	// (3, 7]: exclude lo, include hi -> 4,5,6,7
	got = collect(EncodeInt64(3), EncodeInt64(7), storage.ScanOpts{IncludeHi: true})
	if !eqI64(got, []int64{4, 5, 6, 7}) {
		t.Fatalf("(3,7] = %v", got)
	}
}

func eqI64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
