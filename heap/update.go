package heap

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/storage"
)

// Update replaces the document at rid with newDoc. It updates in place when the
// new cell fits on the record's current page (same RID), and otherwise relocates
// the record to a new page and leaves a forwarding tombstone at the canonical RID
// so existing index entries stay valid (spec 2061 doc 04 §6). It returns the
// canonical RID, which is unchanged: lookups and index references continue to
// resolve through the forwarding tombstone.
func (h *Heap) Update(txn storage.Txn, rid storage.RID, newDoc bson.Raw) (storage.RID, error) {
	if txn.IsReadOnly() {
		return storage.NullRID, storage.ErrReadOnly
	}
	if err := newDoc.Validate(); err != nil {
		return storage.NullRID, err
	}
	if len(newDoc) > MaxDocSize {
		return storage.NullRID, ErrDocumentTooLarge
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	page, slot, forwarded, err := h.resolve(rid)
	if err != nil {
		return storage.NullRID, err
	}
	// Capture the old chain head before overwriting, so it can be freed once the
	// new cell is committed in place or relocated.
	oldHead, oldOverflow, err := h.cellOverflowHead(page, slot)
	if err != nil {
		return storage.NullRID, err
	}

	cell, err := h.buildCell(newDoc, txn.WriteVersion())
	if err != nil {
		return storage.NullRID, err
	}

	// Try an in-place replace on the record's current page.
	done, err := h.replaceInPlace(page, slot, cell)
	if err != nil {
		return storage.NullRID, err
	}
	if done {
		if oldOverflow {
			if err := h.freeOverflow(oldHead); err != nil {
				return storage.NullRID, err
			}
		}
		return rid, nil
	}

	// Grow-and-move: place the new cell elsewhere and forward the canonical RID.
	newRID, err := h.addCell(cell)
	if err != nil {
		return storage.NullRID, err
	}
	if err := h.forwardTo(rid, page, slot, forwarded, newRID); err != nil {
		return storage.NullRID, err
	}
	if oldOverflow {
		if err := h.freeOverflow(oldHead); err != nil {
			return storage.NullRID, err
		}
	}
	return rid, nil
}

// cellOverflowHead reports the overflow head of the cell at (page, slot) if it is
// an overflow cell. The caller holds h.mu.
func (h *Heap) cellOverflowHead(page uint32, slot uint16) (head uint32, isOverflow bool, err error) {
	f, err := h.pgr.Fetch(uint64(page), false)
	if err != nil {
		return 0, false, err
	}
	s := format.OpenSlotted(f.Buf)
	cell, cerr := s.Cell(int(slot))
	if cerr != nil {
		h.pgr.Unpin(f)
		return 0, false, nil
	}
	hdr, ok := decodeCellHeader(cell)
	if !ok || hdr.flags&cellOverflow == 0 {
		h.pgr.Unpin(f)
		return 0, false, nil
	}
	head, _ = decodeOverflowPtr(cell[cellHeaderSize:])
	h.pgr.Unpin(f)
	return head, true, nil
}

// replaceInPlace attempts a same-page, same-slot rewrite of the cell. done is
// false (with a nil error) when the page cannot hold the new cell, signaling the
// caller to relocate. The caller holds h.mu.
func (h *Heap) replaceInPlace(page uint32, slot uint16, cell []byte) (done bool, err error) {
	f, err := h.pgr.Fetch(uint64(page), true)
	if err != nil {
		return false, err
	}
	s := format.OpenSlotted(f.Buf)
	switch rerr := s.ReplaceCell(int(slot), cell); rerr {
	case nil:
		h.pgr.MarkDirty(f)
		h.avail[page] = s.AvailBytes()
		h.pgr.Unpin(f)
		return true, nil
	case format.ErrNoSpace:
		h.pgr.Unpin(f)
		return false, nil
	default:
		h.pgr.Unpin(f)
		return false, rerr
	}
}

// forwardTo records a forwarding tombstone at the canonical RID pointing to
// newRID. If the record was already forwarded, the canonical tombstone is
// repointed straight at newRID (avoiding a two-hop chain, spec 2061 doc 04 §6.5)
// and the now-stale destination cell is marked dead. The caller holds h.mu.
func (h *Heap) forwardTo(rid storage.RID, page uint32, slot uint16, forwarded bool, newRID storage.RID) error {
	cf, err := h.pgr.Fetch(uint64(rid.PageNo), true)
	if err != nil {
		return err
	}
	cs := format.OpenSlotted(cf.Buf)
	if err := cs.SetForward(int(rid.Slot), newRID.PageNo, newRID.Slot); err != nil {
		h.pgr.Unpin(cf)
		return err
	}
	h.pgr.MarkDirty(cf)
	h.avail[rid.PageNo] = cs.AvailBytes()
	h.pgr.Unpin(cf)

	if forwarded {
		// The old physical cell at (page, slot) is no longer referenced; reclaim it.
		return h.deadSlot(page, slot)
	}
	return nil
}
