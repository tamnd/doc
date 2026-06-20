package heap

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/pager"
)

// writeOverflow allocates a chain of overflow pages, serializes payload across
// them, and returns the head page number. The caller holds h.mu.
//
// A single document's chain must fit in the buffer pool: M1 keeps uncommitted
// dirty pages resident (they are unstealable until commit), so a document large
// enough to need more overflow pages than the pool holds cannot be inserted until
// a larger pool is configured. This is the documented M1 ceiling; the streaming
// large-value path is future work.
func (h *Heap) writeOverflow(payload []byte) (uint32, error) {
	n := format.OverflowPageCount(len(payload), uint32(h.pgr.PageSize()))
	pages := make([][]byte, n)
	pageNos := make([]uint32, n)
	frames := make([]*pager.Frame, n)
	for i := 0; i < n; i++ {
		id, f, err := h.pgr.Allocate()
		if err != nil {
			// Unpin the frames acquired so far; their pages will be reclaimed by the
			// next recovery since the insert never commits.
			for j := 0; j < i; j++ {
				h.pgr.Unpin(frames[j])
			}
			return 0, err
		}
		pages[i] = f.Buf
		pageNos[i] = uint32(id)
		frames[i] = f
	}
	algo := h.pgr.Checksum()
	format.WriteOverflowChain(payload, pages, pageNos, h.collID, algo)
	for _, f := range frames {
		h.pgr.MarkDirty(f)
		// MarkDirty stamps the page LSN into the header after WriteOverflowChain
		// computed the trailing checksum, so refresh it: ReadOverflowChain verifies
		// the page checksum and may read this page while it is still resident-dirty.
		format.WritePageChecksum(f.Buf, algo)
		h.pgr.Unpin(f)
	}
	return pageNos[0], nil
}

// readOverflow reassembles and returns the document stored in the overflow chain
// whose head is at headPage. The caller holds h.mu.
func (h *Heap) readOverflow(headPage uint32) (bson.Raw, error) {
	fetch := func(pno uint32) ([]byte, error) {
		f, err := h.pgr.Fetch(uint64(pno), false)
		if err != nil {
			return nil, err
		}
		cp := make([]byte, len(f.Buf))
		copy(cp, f.Buf)
		h.pgr.Unpin(f)
		return cp, nil
	}
	payload, err := format.ReadOverflowChain(headPage, fetch, h.pgr.Checksum())
	if err != nil {
		return nil, err
	}
	return bson.Raw(payload), nil
}

// freeOverflow returns every page of the chain at headPage to the pager's
// freelist. The caller holds h.mu.
func (h *Heap) freeOverflow(headPage uint32) error {
	pno := headPage
	for pno != format.NullPage && pno != 0 {
		f, err := h.pgr.Fetch(uint64(pno), false)
		if err != nil {
			return err
		}
		next := format.DecodePageHeader(f.Buf).RightSib
		h.pgr.Unpin(f)
		if err := h.pgr.Free(uint64(pno)); err != nil {
			return err
		}
		pno = next
	}
	return nil
}
