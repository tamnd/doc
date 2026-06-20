package index

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

const dbPath = "index.doc"
const collID = 1

func openTree(t *testing.T, fs vfs.FS, opts pager.Options) (*pager.Pager, *BTree) {
	t.Helper()
	p, err := pager.Open(fs, dbPath, opts)
	if err != nil {
		t.Fatalf("pager open: %v", err)
	}
	bt, err := Open(p, collID, true)
	if err != nil {
		t.Fatalf("btree open: %v", err)
	}
	return p, bt
}

// oid builds a deterministic ObjectId from a counter so encoded keys sort in
// counter order (the leading bytes dominate).
func oid(n uint32) sys.ObjectID {
	var o sys.ObjectID
	binary.BigEndian.PutUint32(o[0:4], n)
	binary.BigEndian.PutUint32(o[8:12], n)
	return o
}

func rid(page uint32, slot uint16) storage.RID {
	return storage.RID{PageNo: page, Slot: slot}
}

func putCommit(t *testing.T, bt *BTree, key storage.IndexKey, r storage.RID) {
	t.Helper()
	tx := bt.Begin()
	if err := bt.Put(tx, key, r); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func mustGet(t *testing.T, bt *BTree, key storage.IndexKey) storage.RID {
	t.Helper()
	tx := bt.BeginReadOnly()
	r, err := bt.Get(tx, key)
	if err != nil {
		t.Fatalf("get %x: %v", key, err)
	}
	return r
}

func TestPutGetRoundTrip(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	want := rid(7, 3)
	key := EncodeObjectID(oid(42))
	putCommit(t, bt, key, want)

	got := mustGet(t, bt, key)
	if got != want {
		t.Fatalf("get returned %+v, want %+v", got, want)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	putCommit(t, bt, EncodeObjectID(oid(1)), rid(2, 0))

	tx := bt.BeginReadOnly()
	_, err := bt.Get(tx, EncodeObjectID(oid(2)))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestDuplicateKeyRejected(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	key := EncodeObjectID(oid(5))
	putCommit(t, bt, key, rid(1, 1))

	tx := bt.Begin()
	err := bt.Put(tx, key, rid(9, 9))
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("got %v, want ErrDuplicateKey", err)
	}
	// Re-putting the identical (key, rid) is idempotent.
	if err := bt.Put(tx, key, rid(1, 1)); err != nil {
		t.Fatalf("idempotent re-put: %v", err)
	}
}

func TestDeleteRemovesEntry(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	key := EncodeObjectID(oid(11))
	r := rid(4, 2)
	putCommit(t, bt, key, r)

	tx := bt.Begin()
	if err := bt.Delete(tx, key, r); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rtx := bt.BeginReadOnly()
	if _, err := bt.Get(rtx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after delete got %v, want ErrNotFound", err)
	}

	// Deleting a now-absent entry is ErrNotFound.
	dtx := bt.Begin()
	if err := bt.Delete(dtx, key, r); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("re-delete got %v, want ErrNotFound", err)
	}
}

func TestIntegerKeyOrderPreserved(t *testing.T) {
	mem := vfs.NewMemFS()
	p, bt := openTree(t, mem, pager.Options{})
	defer p.Close()

	// Insert out of order, including negatives, then scan ascending.
	vals := []int64{5, -3, 0, 100, -100, 42, 1}
	for i, v := range vals {
		putCommit(t, bt, EncodeInt64(v), rid(uint32(i+1), 0))
	}

	tx := bt.BeginReadOnly()
	cur, err := bt.Scan(tx, nil, nil, storage.ScanOpts{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()

	var got []int64
	for cur.Next() {
		// decode is not needed; verify monotonic byte order via the RID mapping.
		got = append(got, decodeNumKey(t, cur.Key()))
	}
	if cur.Err() != nil {
		t.Fatalf("cursor err: %v", cur.Err())
	}
	want := []int64{-100, -3, 0, 1, 5, 42, 100}
	if len(got) != len(want) {
		t.Fatalf("scanned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan order %v, want %v", got, want)
		}
	}
}

// decodeNumKey inverts encodeNumber for assertions.
func decodeNumKey(t *testing.T, k storage.IndexKey) int64 {
	t.Helper()
	if len(k) != 9 || k[0] != tagNumber {
		t.Fatalf("not a number key: %x", k)
	}
	bits := binary.BigEndian.Uint64(k[1:])
	if bits&(1<<63) != 0 {
		bits &^= 1 << 63
	} else {
		bits = ^bits
	}
	return int64(math.Float64frombits(bits))
}
