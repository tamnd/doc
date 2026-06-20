package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"sync"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
)

// slotDirEntry is the per-cell slot-directory overhead a slotted page charges,
// mirrored here so the B-tree can compute whether a set of cells fits a page
// before it writes them (format.SlottedPage charges 4 bytes per slot).
const slotDirEntry = 4

// ErrKeyTooLarge reports an index key so large that a single entry could not be
// guaranteed to fit a B-tree page with room to split. M1 indexes only small
// scalar _id keys, so this is a guard, not a real limit; oversized-key handling
// (key overflow pages) is future work.
var ErrKeyTooLarge = errors.New("index: key too large for a B-tree page")

// BTree is one persistent B+tree index over the pager, implementing
// storage.IndexStore (spec 2061 doc 07 §2). Leaf cells hold (field-key, RID)
// pairs ordered by the field key with the RID as a tiebreaker; interior cells
// hold (separator, child-page) pairs. The root page number is persisted in the
// pager's catalog-root header slot (M1: one collection, one index).
//
// M1 is single-writer, so a process-wide mutex serializes structural mutation.
// MVCC-versioned entries and concurrent index access arrive in M2.
type BTree struct {
	pgr     *pager.Pager
	collID  uint32
	unique  bool
	bodyLen int
	maxKey  int
	onRoot  func(uint32) // persists a newly created/changed root page

	mu        sync.Mutex
	root      uint32
	txCounter uint64
}

// Open builds a BTree over an already-open pager. unique enables duplicate-key
// rejection (the _id index is unique). The root is read from the pager's
// catalog-root slot and created lazily on the first insert when absent. This is
// the _id index constructor: the _id index owns the header's catalog-root slot.
func Open(pgr *pager.Pager, collID uint32, unique bool) (*BTree, error) {
	return OpenWithRoot(pgr, collID, unique, pgr.CatalogRoot(), pgr.SetCatalogRoot)
}

// OpenWithRoot builds a BTree whose root page is supplied explicitly and whose
// root changes are reported through onRoot, so a secondary index can persist its
// root in the catalog rather than the header's single catalog-root slot (spec
// 2061 doc 07 §5.1, doc 09 §7.1). root is format.NullPage when the index has no
// pages yet; the first insert allocates the root and calls onRoot with it. A nil
// onRoot drops root notifications, for a throwaway in-memory tree.
func OpenWithRoot(pgr *pager.Pager, collID uint32, unique bool, root uint32, onRoot func(uint32)) (*BTree, error) {
	body := format.BodySize(uint32(pgr.PageSize()))
	if onRoot == nil {
		onRoot = func(uint32) {}
	}
	return &BTree{
		pgr:     pgr,
		collID:  collID,
		unique:  unique,
		bodyLen: body,
		maxKey:  body / 4, // a single key plus its RID always leaves room to split
		onRoot:  onRoot,
		root:    root,
	}, nil
}

// Root returns the index's current root page number, or format.NullPage when the
// tree is still empty. The catalog reads this after an index build to persist the
// final root.
func (t *BTree) Root() uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.root
}

// Begin starts a read-write index transaction; BeginReadOnly starts a read-only
// one. These mirror the heap's handles for M1 standalone use and tests.
func (t *BTree) Begin() *Tx {
	t.mu.Lock()
	t.txCounter++
	v := t.txCounter
	t.mu.Unlock()
	return &Tx{t: t, version: v}
}

func (t *BTree) BeginReadOnly() *Tx {
	t.mu.Lock()
	v := t.txCounter
	t.mu.Unlock()
	return &Tx{t: t, version: v, readOnly: true}
}

// ---- node payload codecs -------------------------------------------------

// A leaf cell payload is the full tree key: the field encoding followed by the
// 6-byte RID suffix. The RID is the trailing bytes; the field is the prefix.

// An interior cell payload is the separator (a full tree key, empty for the
// leftmost child) followed by a 4-byte big-endian child page number.

type intEntry struct {
	sep   []byte // full tree key; empty marks the leftmost child
	child uint32
}

func encodeInterior(e intEntry) []byte {
	p := make([]byte, len(e.sep)+4)
	copy(p, e.sep)
	binary.BigEndian.PutUint32(p[len(e.sep):], e.child)
	return p
}

func decodeInterior(payload []byte) intEntry {
	n := len(payload) - 4
	sep := make([]byte, n)
	copy(sep, payload[:n])
	return intEntry{sep: sep, child: binary.BigEndian.Uint32(payload[n:])}
}

// ---- node load / store ---------------------------------------------------

// loadLeaf returns the page's leaf keys (full tree keys), copied out of the pool,
// along with the leaf's right-sibling pointer.
func (t *BTree) loadLeaf(pno uint32) (keys [][]byte, rightSib uint32, err error) {
	f, err := t.pgr.Fetch(uint64(pno), false)
	if err != nil {
		return nil, 0, err
	}
	s := format.OpenSlotted(f.Buf)
	n := s.SlotCount()
	keys = make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if !s.IsLive(i) {
			continue
		}
		cell, cerr := s.Cell(i)
		if cerr != nil {
			continue
		}
		k := make([]byte, len(cell))
		copy(k, cell)
		keys = append(keys, k)
	}
	rightSib = format.DecodePageHeader(f.Buf).RightSib
	t.pgr.Unpin(f)
	return keys, rightSib, nil
}

func (t *BTree) loadInterior(pno uint32) ([]intEntry, error) {
	f, err := t.pgr.Fetch(uint64(pno), false)
	if err != nil {
		return nil, err
	}
	s := format.OpenSlotted(f.Buf)
	n := s.SlotCount()
	ents := make([]intEntry, 0, n)
	for i := 0; i < n; i++ {
		if !s.IsLive(i) {
			continue
		}
		cell, cerr := s.Cell(i)
		if cerr != nil {
			continue
		}
		ents = append(ents, decodeInterior(cell))
	}
	t.pgr.Unpin(f)
	return ents, nil
}

func (t *BTree) pageType(pno uint32) (format.PageType, error) {
	f, err := t.pgr.Fetch(uint64(pno), false)
	if err != nil {
		return 0, err
	}
	ty := format.DecodePageHeader(f.Buf).Type
	t.pgr.Unpin(f)
	return ty, nil
}

// writeLeaf rewrites an existing leaf page with keys in order and the given
// right-sibling link.
func (t *BTree) writeLeaf(pno uint32, keys [][]byte, rightSib uint32) error {
	f, err := t.pgr.Fetch(uint64(pno), true)
	if err != nil {
		return err
	}
	if err := fillLeaf(f.Buf, t.collID, keys, rightSib); err != nil {
		t.pgr.Unpin(f)
		return err
	}
	t.pgr.MarkDirty(f)
	t.pgr.Unpin(f)
	return nil
}

func (t *BTree) writeInterior(pno uint32, ents []intEntry) error {
	f, err := t.pgr.Fetch(uint64(pno), true)
	if err != nil {
		return err
	}
	if err := fillInterior(f.Buf, t.collID, ents); err != nil {
		t.pgr.Unpin(f)
		return err
	}
	t.pgr.MarkDirty(f)
	t.pgr.Unpin(f)
	return nil
}

// newLeaf allocates a fresh leaf page and fills it.
func (t *BTree) newLeaf(keys [][]byte, rightSib uint32) (uint32, error) {
	id, f, err := t.pgr.Allocate()
	if err != nil {
		return 0, err
	}
	if err := fillLeaf(f.Buf, t.collID, keys, rightSib); err != nil {
		t.pgr.Unpin(f)
		return 0, err
	}
	t.pgr.MarkDirty(f)
	t.pgr.Unpin(f)
	return uint32(id), nil
}

func (t *BTree) newInterior(ents []intEntry) (uint32, error) {
	id, f, err := t.pgr.Allocate()
	if err != nil {
		return 0, err
	}
	if err := fillInterior(f.Buf, t.collID, ents); err != nil {
		t.pgr.Unpin(f)
		return 0, err
	}
	t.pgr.MarkDirty(f)
	t.pgr.Unpin(f)
	return uint32(id), nil
}

func fillLeaf(buf []byte, collID uint32, keys [][]byte, rightSib uint32) error {
	format.InitPage(buf, format.PageBTreeLeaf, collID)
	s := format.OpenSlotted(buf)
	for _, k := range keys {
		if _, err := s.AddCell(k); err != nil {
			return err
		}
	}
	h := format.DecodePageHeader(buf)
	h.RightSib = rightSib
	h.EncodeInto(buf)
	return nil
}

func fillInterior(buf []byte, collID uint32, ents []intEntry) error {
	format.InitPage(buf, format.PageBTreeInterior, collID)
	s := format.OpenSlotted(buf)
	for _, e := range ents {
		if _, err := s.AddCell(encodeInterior(e)); err != nil {
			return err
		}
	}
	return nil
}

// ---- fit checks ----------------------------------------------------------

func (t *BTree) leafFits(keys [][]byte) bool {
	total := 0
	for _, k := range keys {
		total += len(k) + slotDirEntry
	}
	return total <= t.bodyLen
}

func (t *BTree) interiorFits(ents []intEntry) bool {
	total := 0
	for _, e := range ents {
		total += len(e.sep) + 4 + slotDirEntry
	}
	return total <= t.bodyLen
}

// ---- descent -------------------------------------------------------------

// descend walks from the root to the leaf whose range covers searchKey,
// returning the leaf page number and the interior page numbers visited (the
// path used to propagate splits back up).
func (t *BTree) descend(searchKey []byte) (uint32, []uint32, error) {
	pno := t.root
	var path []uint32
	for {
		ty, err := t.pageType(pno)
		if err != nil {
			return 0, nil, err
		}
		if ty == format.PageBTreeLeaf {
			return pno, path, nil
		}
		ents, err := t.loadInterior(pno)
		if err != nil {
			return 0, nil, err
		}
		idx := 0
		for i := 1; i < len(ents); i++ {
			if bytes.Compare(ents[i].sep, searchKey) <= 0 {
				idx = i
			} else {
				break
			}
		}
		path = append(path, pno)
		pno = ents[idx].child
	}
}

// ---- IndexStore: Put -----------------------------------------------------

// Put inserts (key, rid) into the index. A unique index rejects a different RID
// for an existing key with storage.ErrDuplicateKey; re-putting the same (key,
// rid) is a no-op. The write is visible to this transaction immediately and
// becomes durable on Commit.
func (t *BTree) Put(txn storage.Txn, key storage.IndexKey, rid storage.RID) error {
	if txn.IsReadOnly() {
		return storage.ErrReadOnly
	}
	full := treeKey(key, rid)
	if len(full)+slotDirEntry > t.maxKey {
		return ErrKeyTooLarge
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.root == format.NullPage {
		root, err := t.newLeaf([][]byte{full}, format.NullPage)
		if err != nil {
			return err
		}
		t.root = root
		t.onRoot(root)
		return nil
	}

	if t.unique {
		existing, err := t.getLocked(key)
		if err == nil && existing != rid {
			return storage.ErrDuplicateKey
		}
		if err == nil && existing == rid {
			return nil // idempotent re-put
		}
	}

	leafPno, path, err := t.descend(full)
	if err != nil {
		return err
	}
	keys, rightSib, err := t.loadLeaf(leafPno)
	if err != nil {
		return err
	}
	pos := sort.Search(len(keys), func(i int) bool { return bytes.Compare(keys[i], full) >= 0 })
	if pos < len(keys) && bytes.Equal(keys[pos], full) {
		return nil // exact (key,rid) already present
	}
	keys = append(keys, nil)
	copy(keys[pos+1:], keys[pos:])
	keys[pos] = full

	if t.leafFits(keys) {
		return t.writeLeaf(leafPno, keys, rightSib)
	}
	return t.splitLeaf(leafPno, keys, rightSib, path)
}

// splitLeaf divides an overfull leaf into two, links the new right leaf into the
// sibling chain, and propagates the separator to the parent.
func (t *BTree) splitLeaf(leafPno uint32, keys [][]byte, oldRightSib uint32, path []uint32) error {
	mid := len(keys) / 2
	left := keys[:mid]
	right := keys[mid:]
	sep := append([]byte(nil), right[0]...)

	rPno, err := t.newLeaf(right, oldRightSib)
	if err != nil {
		return err
	}
	if err := t.writeLeaf(leafPno, left, rPno); err != nil {
		return err
	}
	return t.insertIntoParent(path, leafPno, sep, rPno)
}

// insertIntoParent installs a separator pointing at rightChild into the parent of
// leftChild. With an empty path, leftChild was the root and a new interior root
// is grown.
func (t *BTree) insertIntoParent(path []uint32, leftChild uint32, sep []byte, rightChild uint32) error {
	if len(path) == 0 {
		root, err := t.newInterior([]intEntry{
			{sep: nil, child: leftChild},
			{sep: sep, child: rightChild},
		})
		if err != nil {
			return err
		}
		t.root = root
		t.onRoot(root)
		return nil
	}

	parent := path[len(path)-1]
	ents, err := t.loadInterior(parent)
	if err != nil {
		return err
	}
	pos := sort.Search(len(ents), func(i int) bool { return bytes.Compare(ents[i].sep, sep) > 0 })
	ents = append(ents, intEntry{})
	copy(ents[pos+1:], ents[pos:])
	ents[pos] = intEntry{sep: sep, child: rightChild}

	if t.interiorFits(ents) {
		return t.writeInterior(parent, ents)
	}

	// Split the interior: the middle entry's separator is promoted to the
	// grandparent, and that entry becomes the leftmost child of the new right node.
	mid := len(ents) / 2
	promote := append([]byte(nil), ents[mid].sep...)
	left := ents[:mid]
	right := append([]intEntry(nil), ents[mid:]...)
	right[0].sep = nil

	rPno, err := t.newInterior(right)
	if err != nil {
		return err
	}
	if err := t.writeInterior(parent, left); err != nil {
		return err
	}
	return t.insertIntoParent(path[:len(path)-1], parent, promote, rPno)
}

// ---- IndexStore: Get -----------------------------------------------------

// Get returns the RID stored for the exact field key, or storage.ErrNotFound.
func (t *BTree) Get(txn storage.Txn, key storage.IndexKey) (storage.RID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getLocked(key)
}

func (t *BTree) getLocked(key storage.IndexKey) (storage.RID, error) {
	if t.root == format.NullPage {
		return storage.NullRID, storage.ErrNotFound
	}
	// Seek to the smallest tree key for this field (RID zero sorts first).
	search := treeKey(key, storage.RID{PageNo: 0, Slot: 0})
	p, err := t.seek(search)
	if err != nil {
		return storage.NullRID, err
	}
	if p.done {
		return storage.NullRID, storage.ErrNotFound
	}
	k := p.keys[p.idx]
	if bytes.Equal(fieldOf(k), key) {
		return ridFromSuffix(k), nil
	}
	return storage.NullRID, storage.ErrNotFound
}

// ---- IndexStore: Delete --------------------------------------------------

// Delete removes the entry (key, rid). It returns storage.ErrNotFound if the
// exact entry is absent. M1 does not merge or rebalance underfull nodes; freed
// space is reclaimed lazily by later inserts into the same leaf.
func (t *BTree) Delete(txn storage.Txn, key storage.IndexKey, rid storage.RID) error {
	if txn.IsReadOnly() {
		return storage.ErrReadOnly
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == format.NullPage {
		return storage.ErrNotFound
	}
	full := treeKey(key, rid)
	leafPno, _, err := t.descend(full)
	if err != nil {
		return err
	}
	keys, rightSib, err := t.loadLeaf(leafPno)
	if err != nil {
		return err
	}
	pos := sort.Search(len(keys), func(i int) bool { return bytes.Compare(keys[i], full) >= 0 })
	if pos >= len(keys) || !bytes.Equal(keys[pos], full) {
		return storage.ErrNotFound
	}
	keys = append(keys[:pos], keys[pos+1:]...)
	return t.writeLeaf(leafPno, keys, rightSib)
}

// ---- IndexStore: Stats ---------------------------------------------------

// Stats walks the leaf chain to count entries and descends the leftmost spine to
// measure height, for the planner's cost model.
func (t *BTree) Stats() storage.IndexStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == format.NullPage {
		return storage.IndexStats{}
	}
	// Height: count levels down the leftmost spine.
	height := 1
	pno := t.root
	for {
		ty, err := t.pageType(pno)
		if err != nil || ty == format.PageBTreeLeaf {
			break
		}
		ents, err := t.loadInterior(pno)
		if err != nil || len(ents) == 0 {
			break
		}
		height++
		pno = ents[0].child
	}
	// Entries: sum live cells across the leaf chain from the leftmost leaf.
	var entries uint64
	leaf, _, err := t.descend([]byte{})
	if err == nil {
		for leaf != format.NullPage {
			keys, rs, lerr := t.loadLeaf(leaf)
			if lerr != nil {
				break
			}
			entries += uint64(len(keys))
			leaf = rs
		}
	}
	return storage.IndexStats{Entries: entries, DistinctKeys: entries, Height: height}
}
