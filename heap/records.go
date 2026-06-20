package heap

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/storage"
)

// Record is one live heap record with the MVCC version stamped on its cell. The
// collection layer reads these on Open to rebuild its in-memory version overlay,
// which needs each record's version (not just its bytes) to seed the visibility
// chains and the oracle's starting commit version (spec 2061 doc 06 §4.6).
type Record struct {
	RID     storage.RID
	Version uint64
	Doc     bson.Raw
}

// Records returns every live, non-forwarding record in the heap together with its
// stamped version. It is the version-aware companion to Scan, used once at Open;
// Scan stays the snapshot-driven cursor the SPI exposes. The returned Raw values
// are fresh copies and outlive the buffer pool.
func (h *Heap) Records() ([]Record, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Record
	for _, pno := range h.order {
		f, err := h.pgr.Fetch(uint64(pno), false)
		if err != nil {
			return nil, err
		}
		s := format.OpenSlotted(f.Buf)
		n := s.SlotCount()
		for i := 0; i < n; i++ {
			if !s.IsLive(i) || s.IsForward(i) {
				continue
			}
			cell, cerr := s.Cell(i)
			if cerr != nil {
				continue
			}
			hdr, ok := decodeCellHeader(cell)
			if !ok || hdr.flags&cellTombstone != 0 {
				continue
			}
			doc, rerr := h.readAt(pno, uint16(i))
			if rerr != nil {
				h.pgr.Unpin(f)
				return nil, rerr
			}
			out = append(out, Record{
				RID:     storage.RID{PageNo: pno, Slot: uint16(i)},
				Version: hdr.version,
				Doc:     doc,
			})
		}
		h.pgr.Unpin(f)
	}
	return out, nil
}
