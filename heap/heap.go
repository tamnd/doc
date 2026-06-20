package heap

import (
	"errors"
	"sync"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
)

// Tunables (spec 2061 doc 04 §5.2, §8.1).
const (
	// SpillThreshold is the inline BSON size limit: a document whose payload is at
	// most this many bytes is stored inline in a heap cell; a larger one spills to
	// an overflow chain. The default leaves roughly a quarter of an 8 KiB page free
	// for cohabitation and updates.
	SpillThreshold = 6144

	// MaxDocSize is the hard 16 MiB document ceiling the wire protocol also
	// enforces; Insert and Update reject anything larger.
	MaxDocSize = 16 << 20
)

// ErrDocumentTooLarge reports a document whose BSON payload exceeds MaxDocSize.
var ErrDocumentTooLarge = errors.New("heap: document exceeds 16 MiB limit")

// Heap is a single collection's slotted-page record store over a pager. It owns
// every heap and overflow page in the file for its collection id and maintains an
// in-memory free-space directory rebuilt by scanning the heap on Open.
//
// M1 keeps one collection per file; the catalog that multiplexes collections and
// persists the free-space map arrives in M3. The free-space directory here is a
// derived accelerator (the records themselves are the durable truth), so
// rebuilding it on Open costs one scan and needs no separate on-disk structure.
type Heap struct {
	pgr    *pager.Pager
	collID uint32

	mu        sync.Mutex
	order     []uint32       // heap page numbers, in discovery/allocation order
	avail     map[uint32]int // page -> AvailBytes (compaction-aware free space)
	cursor    int            // next-fit cursor into order
	liveCount uint64
	deadCount uint64
	txCounter uint64
}

// Open builds a Heap over an already-open pager, scanning existing pages to
// recover the free-space directory and live-record count. collID is the owning
// collection id stamped into every page this heap allocates.
func Open(pgr *pager.Pager, collID uint32) (*Heap, error) {
	h := &Heap{
		pgr:    pgr,
		collID: collID,
		avail:  make(map[uint32]int),
	}
	if err := h.rebuild(); err != nil {
		return nil, err
	}
	return h, nil
}

// rebuild scans every page in the file, registering heap pages in the free-space
// directory and counting live records. Overflow and other page types are skipped.
func (h *Heap) rebuild() error {
	pages := h.pgr.PageCount()
	for pno := uint32(1); pno < pages; pno++ {
		f, err := h.pgr.Fetch(uint64(pno), false)
		if err != nil {
			return err
		}
		ph := format.DecodePageHeader(f.Buf)
		if ph.Type == format.PageHeap && ph.CollIDEntry == h.collID {
			s := format.OpenSlotted(f.Buf)
			h.order = append(h.order, pno)
			h.avail[pno] = s.AvailBytes()
			h.countLive(s)
		}
		h.pgr.Unpin(f)
	}
	return nil
}

// countLive tallies the live and dead records on a heap page into the heap's
// counters during rebuild.
func (h *Heap) countLive(s format.SlottedPage) {
	n := s.SlotCount()
	for i := 0; i < n; i++ {
		if s.IsForward(i) {
			continue // counted at the destination cell
		}
		if !s.IsLive(i) {
			h.deadCount++
			continue
		}
		cell, err := s.Cell(i)
		if err != nil {
			continue
		}
		if hdr, ok := decodeCellHeader(cell); ok && hdr.flags&cellTombstone == 0 {
			h.liveCount++
		} else {
			h.deadCount++
		}
	}
}

// Begin starts a read-write transaction.
func (h *Heap) Begin() *Tx {
	h.mu.Lock()
	h.txCounter++
	v := h.txCounter
	h.mu.Unlock()
	return &Tx{h: h, version: v}
}

// BeginReadOnly starts a read-only transaction.
func (h *Heap) BeginReadOnly() *Tx {
	h.mu.Lock()
	v := h.txCounter
	h.mu.Unlock()
	return &Tx{h: h, version: v, readOnly: true}
}

// Insert encodes doc into a cell, places it on a heap page (spilling to an
// overflow chain when oversized), and returns the new record's RID.
func (h *Heap) Insert(txn storage.Txn, doc bson.Raw) (storage.RID, error) {
	if txn.IsReadOnly() {
		return storage.NullRID, storage.ErrReadOnly
	}
	if err := doc.Validate(); err != nil {
		return storage.NullRID, err
	}
	if len(doc) > MaxDocSize {
		return storage.NullRID, ErrDocumentTooLarge
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	cell, err := h.buildCell(doc, txn.WriteVersion())
	if err != nil {
		return storage.NullRID, err
	}
	rid, err := h.addCell(cell)
	if err != nil {
		return storage.NullRID, err
	}
	h.liveCount++
	return rid, nil
}

// buildCell produces the cell payload for doc: an inline cell when it fits under
// the spill threshold, otherwise a 40-byte overflow-pointer cell with the chain
// already written to the pager.
func (h *Heap) buildCell(doc bson.Raw, version uint64) ([]byte, error) {
	if len(doc) <= SpillThreshold {
		cell := make([]byte, cellHeaderSize+len(doc))
		cellHeader{flags: cellHasVer, bsonLen: uint32(len(doc)), version: version}.encode(cell)
		copy(cell[cellHeaderSize:], doc)
		return cell, nil
	}
	head, err := h.writeOverflow(doc)
	if err != nil {
		return nil, err
	}
	cell := make([]byte, cellHeaderSize+overflowPtrSize)
	cellHeader{flags: cellHasVer | cellOverflow, bsonLen: 0, version: version}.encode(cell)
	encodeOverflowPtr(cell[cellHeaderSize:], head, uint32(len(doc)))
	return cell, nil
}

// addCell finds (or grows) a heap page with room for the cell and writes it,
// returning the RID. It uses a next-fit search over the free-space directory.
func (h *Heap) addCell(cell []byte) (storage.RID, error) {
	if pno, ok := h.findPage(len(cell)); ok {
		f, err := h.pgr.Fetch(uint64(pno), true)
		if err != nil {
			return storage.NullRID, err
		}
		s := format.OpenSlotted(f.Buf)
		slot, err := s.AddCell(cell)
		if err != nil {
			h.pgr.Unpin(f)
			return storage.NullRID, err
		}
		h.pgr.MarkDirty(f)
		h.avail[pno] = s.AvailBytes()
		h.pgr.Unpin(f)
		return storage.RID{PageNo: pno, Slot: uint16(slot)}, nil
	}
	return h.addCellNewPage(cell)
}

// findPage returns a heap page that can hold a cell of the given length, scanning
// next-fit from the cursor and wrapping once.
func (h *Heap) findPage(cellLen int) (uint32, bool) {
	n := len(h.order)
	for i := 0; i < n; i++ {
		idx := (h.cursor + i) % n
		pno := h.order[idx]
		if h.avail[pno] >= cellLen+4 { // +4 for the new slot-directory entry
			h.cursor = idx
			return pno, true
		}
	}
	return 0, false
}

// addCellNewPage allocates a fresh heap page and writes the cell as slot 0.
func (h *Heap) addCellNewPage(cell []byte) (storage.RID, error) {
	id, f, err := h.pgr.Allocate()
	if err != nil {
		return storage.NullRID, err
	}
	format.InitPage(f.Buf, format.PageHeap, h.collID)
	s := format.OpenSlotted(f.Buf)
	slot, err := s.AddCell(cell)
	if err != nil {
		h.pgr.Unpin(f)
		return storage.NullRID, err
	}
	h.pgr.MarkDirty(f)
	pno := uint32(id)
	h.order = append(h.order, pno)
	h.avail[pno] = s.AvailBytes()
	h.cursor = len(h.order) - 1
	h.pgr.Unpin(f)
	return storage.RID{PageNo: pno, Slot: uint16(slot)}, nil
}

// resolve follows a forwarding tombstone at rid to the physical (page, slot) of
// the live cell. It returns the resolved location and whether a forwarding hop
// occurred. The caller holds h.mu.
func (h *Heap) resolve(rid storage.RID) (page uint32, slot uint16, forwarded bool, err error) {
	f, err := h.pgr.Fetch(uint64(rid.PageNo), false)
	if err != nil {
		return 0, 0, false, err
	}
	s := format.OpenSlotted(f.Buf)
	if s.IsForward(int(rid.Slot)) {
		tp, ts, _ := s.Forward(int(rid.Slot))
		h.pgr.Unpin(f)
		return tp, ts, true, nil
	}
	h.pgr.Unpin(f)
	return rid.PageNo, rid.Slot, false, nil
}

// Lookup returns the snapshot-visible BSON at rid, transparently following a
// forwarding tombstone. The returned Raw is a fresh copy and outlives the page.
func (h *Heap) Lookup(txn storage.Txn, rid storage.RID) (bson.Raw, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	page, slot, _, err := h.resolve(rid)
	if err != nil {
		return nil, err
	}
	return h.readAt(page, slot)
}

// readAt reads and copies the document stored at a resolved (page, slot). The
// caller holds h.mu.
func (h *Heap) readAt(page uint32, slot uint16) (bson.Raw, error) {
	f, err := h.pgr.Fetch(uint64(page), false)
	if err != nil {
		return nil, err
	}
	s := format.OpenSlotted(f.Buf)
	if !s.IsLive(int(slot)) {
		h.pgr.Unpin(f)
		return nil, storage.ErrNotFound
	}
	cell, err := s.Cell(int(slot))
	if err != nil {
		h.pgr.Unpin(f)
		return nil, storage.ErrNotFound
	}
	hdr, ok := decodeCellHeader(cell)
	if !ok || hdr.flags&cellTombstone != 0 {
		h.pgr.Unpin(f)
		return nil, storage.ErrNotFound
	}
	if hdr.flags&cellOverflow != 0 {
		head, _ := decodeOverflowPtr(cell[cellHeaderSize:])
		h.pgr.Unpin(f)
		return h.readOverflow(head)
	}
	out := make(bson.Raw, hdr.bsonLen)
	copy(out, cell[cellHeaderSize:cellHeaderSize+int(hdr.bsonLen)])
	h.pgr.Unpin(f)
	return out, nil
}

// Delete tombstones the record at rid. A forwarded record's source and
// destination slots are both marked dead, and any overflow chain is freed.
func (h *Heap) Delete(txn storage.Txn, rid storage.RID) error {
	if txn.IsReadOnly() {
		return storage.ErrReadOnly
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	page, slot, forwarded, err := h.resolve(rid)
	if err != nil {
		return err
	}
	if err := h.freeCellOverflow(page, slot); err != nil {
		return err
	}
	if err := h.deadSlot(page, slot); err != nil {
		return err
	}
	if forwarded {
		// Drop the forwarding tombstone at the canonical RID too.
		if err := h.deadSlot(rid.PageNo, rid.Slot); err != nil {
			return err
		}
	}
	h.liveCount--
	h.deadCount++
	return nil
}

// deadSlot marks a slot dead and refreshes the free-space entry. The caller holds
// h.mu.
func (h *Heap) deadSlot(page uint32, slot uint16) error {
	f, err := h.pgr.Fetch(uint64(page), true)
	if err != nil {
		return err
	}
	s := format.OpenSlotted(f.Buf)
	if err := s.DeleteCell(int(slot)); err != nil {
		h.pgr.Unpin(f)
		return err
	}
	h.pgr.MarkDirty(f)
	h.avail[page] = s.AvailBytes()
	h.pgr.Unpin(f)
	return nil
}

// freeCellOverflow frees the overflow chain backing the cell at (page, slot) if
// it is an overflow cell; inline cells are a no-op. The caller holds h.mu.
func (h *Heap) freeCellOverflow(page uint32, slot uint16) error {
	f, err := h.pgr.Fetch(uint64(page), false)
	if err != nil {
		return err
	}
	s := format.OpenSlotted(f.Buf)
	cell, err := s.Cell(int(slot))
	if err != nil {
		h.pgr.Unpin(f)
		return nil
	}
	hdr, ok := decodeCellHeader(cell)
	if !ok || hdr.flags&cellOverflow == 0 {
		h.pgr.Unpin(f)
		return nil
	}
	head, _ := decodeOverflowPtr(cell[cellHeaderSize:])
	h.pgr.Unpin(f)
	return h.freeOverflow(head)
}

// FreeSpaceStats reports the heap's occupancy for the planner.
func (h *Heap) FreeSpaceStats() storage.FreeSpaceStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	var free uint64
	for _, a := range h.avail {
		free += uint64(a)
	}
	return storage.FreeSpaceStats{
		PageCount:   uint64(len(h.order)),
		LiveRecords: h.liveCount,
		DeadRecords: h.deadCount,
		FreeBytes:   free,
	}
}
