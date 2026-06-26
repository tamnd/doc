package heap

import (
	"fmt"

	"github.com/tamnd/doc/format"
)

// CheckResult reports a heap integrity walk: the live and dead record counts it
// observed on disk and every invariant violation it found. A nil Problems slice
// means every record-store invariant held.
type CheckResult struct {
	HeapPages   int
	LiveRecords uint64
	DeadRecords uint64
	Problems    []string
}

// Check walks every heap page this collection owns and verifies the record-store
// invariants of spec 2061 doc 19 §17 (the "Record store" row): the free-space
// directory's per-page accounting is accurate, no slot is both live and a
// tombstone, every forwarding chain is bounded to depth one, every overflow chain
// reassembles, and the cached live and dead counters match what is on the pages.
// It reads under h.mu and mutates nothing, so it is safe to run on an open heap.
func (h *Heap) Check() CheckResult {
	h.mu.Lock()
	defer h.mu.Unlock()

	var res CheckResult
	res.HeapPages = len(h.order)
	for _, pno := range h.order {
		h.checkPage(pno, &res)
	}
	if res.LiveRecords != h.liveCount {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"live-count mismatch: directory %d, pages %d", h.liveCount, res.LiveRecords))
	}
	if res.DeadRecords != h.deadCount {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"dead-count mismatch: directory %d, pages %d", h.deadCount, res.DeadRecords))
	}
	return res
}

// checkPage verifies one heap page and tallies its live and dead records into res.
func (h *Heap) checkPage(pno uint32, res *CheckResult) {
	f, err := h.pgr.Fetch(uint64(pno), false)
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("page %d: fetch failed: %v", pno, err))
		return
	}
	defer h.pgr.Unpin(f)

	ph := format.DecodePageHeader(f.Buf)
	if ph.Type != format.PageHeap {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d: directory lists it as heap but its type is %v", pno, ph.Type))
		return
	}
	if ph.CollIDEntry != h.collID {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d: owned by collection %d, expected %d", pno, ph.CollIDEntry, h.collID))
	}
	if vErr := format.VerifyPageChecksum(f.Buf, h.pgr.Checksum()); vErr != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("page %d: %v", pno, vErr))
	}

	s := format.OpenSlotted(f.Buf)
	if got, want := s.AvailBytes(), h.avail[pno]; got != want {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d: free-space directory says %d available, page has %d", pno, want, got))
	}
	n := s.SlotCount()
	for i := 0; i < n; i++ {
		h.checkSlot(pno, s, i, res)
	}
}

// checkSlot classifies one slot as live, dead, or a forwarding tombstone and
// verifies the invariant tied to its kind. The caller holds h.mu.
func (h *Heap) checkSlot(pno uint32, s format.SlottedPage, i int, res *CheckResult) {
	switch {
	case s.IsForward(i):
		h.checkForward(pno, s, i, res)
	case s.IsLive(i):
		cell, err := s.Cell(i)
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"page %d slot %d: live slot has no readable cell: %v", pno, i, err))
			res.DeadRecords++
			return
		}
		hdr, ok := decodeCellHeader(cell)
		if !ok {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"page %d slot %d: cell header is corrupt", pno, i))
			res.DeadRecords++
			return
		}
		if hdr.flags&cellTombstone != 0 {
			res.DeadRecords++
			return
		}
		if hdr.flags&cellOverflow != 0 {
			head, total := decodeOverflowPtr(cell[cellHeaderSize:])
			if doc, oerr := h.readOverflow(head); oerr != nil {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"page %d slot %d: overflow chain at page %d is unreadable: %v", pno, i, head, oerr))
			} else if len(doc) != int(total) {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"page %d slot %d: overflow chain holds %d bytes, header says %d", pno, i, len(doc), total))
			}
		}
		res.LiveRecords++
	default:
		res.DeadRecords++
	}
}

// checkForward verifies a forwarding tombstone resolves in one hop to a live,
// non-forwarding cell, the depth-one bound of spec 2061 doc 19 §17. The caller
// holds h.mu.
func (h *Heap) checkForward(pno uint32, s format.SlottedPage, i int, res *CheckResult) {
	tp, ts, ok := s.Forward(i)
	if !ok {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d slot %d: forwarding slot has no target", pno, i))
		return
	}
	f, err := h.pgr.Fetch(uint64(tp), false)
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d slot %d: forwarding target page %d unreadable: %v", pno, i, tp, err))
		return
	}
	defer h.pgr.Unpin(f)
	ts2 := format.OpenSlotted(f.Buf)
	if ts2.IsForward(int(ts)) {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d slot %d: forwarding chain exceeds depth one (target %d:%d also forwards)", pno, i, tp, ts))
		return
	}
	if !ts2.IsLive(int(ts)) {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"page %d slot %d: forwarding target %d:%d is not live", pno, i, tp, ts))
	}
}
