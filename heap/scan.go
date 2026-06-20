package heap

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/storage"
)

// Scan returns a cursor over every snapshot-visible record in heap order: pages
// in page-number order and, within a page, slots in slot order. Forwarding
// tombstones are skipped because the relocated record is itself a live cell
// yielded at its physical destination, so each document appears exactly once.
func (h *Heap) Scan(txn storage.Txn) (storage.RecordCursor, error) {
	h.mu.Lock()
	pages := make([]uint32, len(h.order))
	copy(pages, h.order)
	h.mu.Unlock()
	return &cursor{h: h, pages: pages, slot: 0, pi: 0}, nil
}

// cursor walks the heap's pages lazily. It copies each yielded document out of
// the buffer pool, so the page may be evicted between Next calls.
type cursor struct {
	h      *Heap
	pages  []uint32
	pi     int // index into pages
	slot   int // next slot to examine on the current page
	curRID storage.RID
	curDoc bson.Raw
	err    error
	closed bool
}

func (c *cursor) Next() bool {
	if c.closed || c.err != nil {
		return false
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	for c.pi < len(c.pages) {
		pno := c.pages[c.pi]
		f, err := c.h.pgr.Fetch(uint64(pno), false)
		if err != nil {
			c.err = err
			return false
		}
		s := format.OpenSlotted(f.Buf)
		count := s.SlotCount()
		for c.slot < count {
			slot := c.slot
			c.slot++
			if !s.IsLive(slot) {
				continue
			}
			cell, cerr := s.Cell(slot)
			if cerr != nil {
				continue
			}
			hdr, ok := decodeCellHeader(cell)
			if !ok || hdr.flags&cellTombstone != 0 {
				continue
			}
			c.h.pgr.Unpin(f)
			doc, rerr := c.h.readAt(pno, uint16(slot))
			if rerr != nil {
				c.err = rerr
				return false
			}
			c.curRID = storage.RID{PageNo: pno, Slot: uint16(slot)}
			c.curDoc = doc
			return true
		}
		c.h.pgr.Unpin(f)
		c.pi++
		c.slot = 0
	}
	return false
}

func (c *cursor) RID() storage.RID { return c.curRID }
func (c *cursor) Doc() bson.Raw    { return c.curDoc }
func (c *cursor) Err() error       { return c.err }
func (c *cursor) Close() error     { c.closed = true; return nil }
