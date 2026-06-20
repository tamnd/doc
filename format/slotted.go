package format

import "encoding/binary"

// The slotted-page discipline (spec 2061 doc 03 §4.3, §5). A slotted page splits
// its body into a slot directory that grows upward from body offset 0 and a cell
// content area that grows downward from the body end. A slot is a fixed 4-byte
// entry: a u16 cell offset (relative to the body) and a u16 cell length. The
// contiguous free space is the gap between the end of the slot directory
// (free_start) and the lowest written cell (free_end).
//
// SlottedPage is the generic container used by heap pages, catalog pages, and
// B-tree leaf pages. It stores opaque byte payloads addressed by a stable slot
// number; it knows nothing of BSON, MVCC version chains, or forwarding
// tombstones. Those are layered above it by the record store in M1 (the heap
// cell header) and by the index in M3. The only tombstone the generic layer
// understands is the dead tombstone, which deletes a slot without renumbering
// the survivors so outstanding RIDs stay valid.
type SlottedPage struct {
	// Buf is the full page buffer; len(Buf) must equal the database page size.
	Buf []byte
}

const (
	slotEntrySize = 4 // u16 cell_offset + u16 cell_length

	// Sentinel cell offsets stored in a slot's first u16 (spec 2061 doc 03 §5.3).
	cellOffsetDead    = 0xFFFF // dead tombstone: deleted, no forwarding
	cellOffsetForward = 0xFFFE // forwarding tombstone (heap layer, M1)

	// maxCellLen is the largest payload that can be addressed by the u16 length
	// field. Documents larger than a page use overflow chains (overflow.go); the
	// record store enforces the higher logical 16 MiB limit.
	maxCellLen = 0xFFFD
)

// OpenSlotted wraps a page buffer. The buffer must already carry a valid common
// page header (use InitPage to start a fresh slotted page).
func OpenSlotted(buf []byte) SlottedPage { return SlottedPage{Buf: buf} }

// body returns the mutable body region between the common header and the
// trailing checksum.
func (s SlottedPage) body() []byte {
	return s.Buf[BodyOffset : len(s.Buf)-ChecksumSize]
}

func (s SlottedPage) header() PageHeader     { return DecodePageHeader(s.Buf) }
func (s SlottedPage) setHeader(h PageHeader) { h.EncodeInto(s.Buf) }
func (s SlottedPage) bodyLen() int           { return len(s.Buf) - PageHeaderSize - ChecksumSize }

// SlotCount returns the number of allocated slots (live, dead, and forwarding).
func (s SlottedPage) SlotCount() int { return int(s.header().EntryCount) }

// FreeBytes returns the size of the contiguous free gap, the space immediately
// available for a new cell without compaction.
func (s SlottedPage) FreeBytes() int {
	h := s.header()
	return int(h.FreeEnd) - int(h.FreeStart)
}

// slotAt returns the (offset, length) pair stored in slot i.
func (s SlottedPage) slotAt(i int) (off, length uint16) {
	b := s.body()
	p := i * slotEntrySize
	return binary.LittleEndian.Uint16(b[p : p+2]), binary.LittleEndian.Uint16(b[p+2 : p+4])
}

func (s SlottedPage) setSlot(i int, off, length uint16) {
	b := s.body()
	p := i * slotEntrySize
	binary.LittleEndian.PutUint16(b[p:p+2], off)
	binary.LittleEndian.PutUint16(b[p+2:p+4], length)
}

// IsLive reports whether slot i holds a live cell (not a dead or forwarding
// tombstone). It returns false for out-of-range slots.
func (s SlottedPage) IsLive(i int) bool {
	if i < 0 || i >= s.SlotCount() {
		return false
	}
	off, _ := s.slotAt(i)
	return off != cellOffsetDead && off != cellOffsetForward
}

// IsForward reports whether slot i is a forwarding tombstone.
func (s SlottedPage) IsForward(i int) bool {
	if i < 0 || i >= s.SlotCount() {
		return false
	}
	off, _ := s.slotAt(i)
	return off == cellOffsetForward
}

// liveTotal returns the sum of live (and forwarding) cell lengths plus the slot
// directory size, i.e. the bytes that would remain occupied after a compaction.
// It is used to decide whether compaction can satisfy an allocation.
func (s SlottedPage) liveTotal() int {
	total := s.SlotCount() * slotEntrySize
	n := s.SlotCount()
	for i := 0; i < n; i++ {
		off, length := s.slotAt(i)
		if off == cellOffsetDead {
			continue
		}
		if off == cellOffsetForward {
			total += forwardCellLen
			continue
		}
		total += int(length)
	}
	return total
}

// AvailBytes returns the largest cell payload the page could accept after a full
// compaction, not counting the slot-directory entry the new cell would need. It
// is the heap's free-space-directory accounting value: it reflects bytes locked
// up by dead tombstones and superseded cells that a compaction would reclaim, not
// just the contiguous gap that FreeBytes reports.
func (s SlottedPage) AvailBytes() int { return s.bodyLen() - s.liveTotal() }

// CanAdd reports whether a payload of payloadLen bytes can be placed on the page,
// compacting if necessary. It mirrors AddCell's own success condition (room for
// the payload plus one new slot-directory entry).
func (s SlottedPage) CanAdd(payloadLen int) bool {
	return s.bodyLen()-s.liveTotal() >= payloadLen+slotEntrySize
}

// AddCell writes payload as a new cell and returns its slot number. It first
// tries the contiguous gap; if that is too small but a compaction would free
// enough room, it compacts and retries; otherwise it returns ErrNoSpace. payload
// longer than a single page (maxCellLen) is rejected - the caller spills it to an
// overflow chain.
func (s SlottedPage) AddCell(payload []byte) (int, error) {
	if len(payload) > maxCellLen {
		return 0, ErrNoSpace
	}
	need := len(payload) + slotEntrySize
	if s.FreeBytes() < need {
		// Would compaction help? available = bodyLen - liveTotal.
		if s.bodyLen()-s.liveTotal() < need {
			return 0, ErrNoSpace
		}
		s.Compact()
	}
	h := s.header()
	cellOff := int(h.FreeEnd) - len(payload)
	copy(s.body()[cellOff:cellOff+len(payload)], payload)
	slot := int(h.EntryCount)
	s.setSlot(slot, uint16(cellOff), uint16(len(payload)))
	h.EntryCount++
	h.FreeStart += slotEntrySize
	h.FreeEnd = uint16(cellOff)
	s.setHeader(h)
	return slot, nil
}

// Cell returns the payload bytes stored in slot i. The returned slice aliases the
// page buffer; callers that retain it past the page's lifetime must copy. It
// returns ErrBadSlot for an out-of-range slot and ErrSlotDead for a dead or
// forwarding tombstone (use Forward to read a forwarding target).
func (s SlottedPage) Cell(i int) ([]byte, error) {
	if i < 0 || i >= s.SlotCount() {
		return nil, ErrBadSlot
	}
	off, length := s.slotAt(i)
	if off == cellOffsetDead || off == cellOffsetForward {
		return nil, ErrSlotDead
	}
	return s.body()[off : int(off)+int(length)], nil
}

// UpdateInPlace overwrites slot i's cell with payload when payload is the same
// length as the existing cell. A length change cannot be done in place on a
// slotted page (it would require relocating the cell); the record store handles
// that at the heap layer by allocating a new cell. Returns ErrNoSpace when the
// lengths differ and ErrBadSlot/ErrSlotDead as Cell does.
func (s SlottedPage) UpdateInPlace(i int, payload []byte) error {
	if i < 0 || i >= s.SlotCount() {
		return ErrBadSlot
	}
	off, length := s.slotAt(i)
	if off == cellOffsetDead || off == cellOffsetForward {
		return ErrSlotDead
	}
	if int(length) != len(payload) {
		return ErrNoSpace
	}
	copy(s.body()[off:int(off)+len(payload)], payload)
	return nil
}

// ReplaceCell rewrites slot i's cell with a new payload of a possibly different
// length, keeping the slot number (and therefore the RID) stable. It is the
// heap's same-page update path: the old cell's bytes are reclaimed and the new
// payload is written into the page, compacting first if the contiguous gap is too
// small. It returns ErrNoSpace when the page cannot hold the new payload even
// after compaction (the heap then relocates the record and leaves a forwarding
// tombstone); on that error slot i is left untouched. ErrBadSlot/ErrSlotDead are
// returned as Cell does.
func (s SlottedPage) ReplaceCell(i int, payload []byte) error {
	if i < 0 || i >= s.SlotCount() {
		return ErrBadSlot
	}
	off, oldLen := s.slotAt(i)
	if off == cellOffsetDead || off == cellOffsetForward {
		return ErrSlotDead
	}
	if len(payload) > maxCellLen {
		return ErrNoSpace
	}
	// Same length is a straight overwrite with no space accounting.
	if int(oldLen) == len(payload) {
		copy(s.body()[off:int(off)+len(payload)], payload)
		return nil
	}
	// Space available for slot i if its old cell is reclaimed: total reclaimable
	// (bodyLen - liveTotal) plus the old cell's own bytes. No slot-directory growth,
	// because we reuse slot i.
	if s.bodyLen()-s.liveTotal()+int(oldLen) < len(payload) {
		return ErrNoSpace
	}
	// Drop the old cell; its bytes become reclaimable. The contiguous gap is
	// unchanged by this, so a fitting payload can still be written without a
	// compaction when the gap alone is large enough.
	s.setSlot(i, cellOffsetDead, 0)
	if s.FreeBytes() < len(payload) {
		s.Compact()
	}
	h := s.header()
	cellOff := int(h.FreeEnd) - len(payload)
	copy(s.body()[cellOff:cellOff+len(payload)], payload)
	s.setSlot(i, uint16(cellOff), uint16(len(payload)))
	h.FreeEnd = uint16(cellOff)
	s.setHeader(h)
	return nil
}

// DeleteCell turns slot i into a dead tombstone. The slot is not removed and
// later slot numbers are unchanged, so every outstanding RID for a surviving
// cell remains valid. The freed bytes are reclaimed by the next Compact.
func (s SlottedPage) DeleteCell(i int) error {
	if i < 0 || i >= s.SlotCount() {
		return ErrBadSlot
	}
	s.setSlot(i, cellOffsetDead, 0)
	return nil
}

// forwardCellLen is the size of the forwarding-target record written into the
// content area for a forwarding tombstone (spec 2061 doc 03 §5.8): a u32 target
// page, a u16 target slot, and two reserved bytes.
const forwardCellLen = 8

// SetForward turns slot i into a forwarding tombstone pointing at
// (targetPage, targetSlot). The 8-byte target record is written into the content
// area and the slot offset is set to the forwarding sentinel. The record store
// uses this when a growing update relocates a document off its page (M1).
func (s SlottedPage) SetForward(i int, targetPage uint32, targetSlot uint16) error {
	if i < 0 || i >= s.SlotCount() {
		return ErrBadSlot
	}
	off, _ := s.slotAt(i)
	// If the slot already holds a live cell of at least forwardCellLen bytes,
	// reuse its space; otherwise allocate a fresh forwarding cell from the gap.
	var cellOff int
	if off != cellOffsetDead && off != cellOffsetForward {
		_, length := s.slotAt(i)
		if int(length) >= forwardCellLen {
			cellOff = int(off)
		}
	}
	if cellOff == 0 {
		h := s.header()
		if int(h.FreeEnd)-int(h.FreeStart) < forwardCellLen {
			if s.bodyLen()-s.liveTotal() < forwardCellLen {
				return ErrNoSpace
			}
			s.Compact()
			h = s.header()
		}
		cellOff = int(h.FreeEnd) - forwardCellLen
		h.FreeEnd = uint16(cellOff)
		s.setHeader(h)
	}
	b := s.body()
	binary.LittleEndian.PutUint32(b[cellOff:cellOff+4], targetPage)
	binary.LittleEndian.PutUint16(b[cellOff+4:cellOff+6], targetSlot)
	binary.LittleEndian.PutUint16(b[cellOff+6:cellOff+8], 0)
	// Mark the slot as forwarding; stash the real cell offset in a side channel
	// is unnecessary because Forward re-reads it: we keep cell_length pointing at
	// the content offset so Forward can locate the 8-byte record.
	s.setSlot(i, cellOffsetForward, uint16(cellOff))
	return nil
}

// Forward returns the (page, slot) target of a forwarding tombstone at slot i.
// ok is false when slot i is not a forwarding tombstone.
func (s SlottedPage) Forward(i int) (page uint32, slot uint16, ok bool) {
	if i < 0 || i >= s.SlotCount() {
		return 0, 0, false
	}
	off, contentOff := s.slotAt(i)
	if off != cellOffsetForward {
		return 0, 0, false
	}
	b := s.body()
	co := int(contentOff)
	return binary.LittleEndian.Uint32(b[co : co+4]), binary.LittleEndian.Uint16(b[co+4 : co+6]), true
}

// Compact rewrites all live and forwarding cells against the high end of the
// body, reclaiming the holes left by dead tombstones and superseded cells. Slot
// numbers and the slot-directory length are unchanged, so all outstanding RIDs
// for live cells stay valid (spec 2061 doc 03 §5.7).
func (s SlottedPage) Compact() {
	n := s.SlotCount()
	b := s.body()
	// Snapshot the payloads of all live/forwarding cells before overwriting.
	type cell struct {
		slot    int
		forward bool
		data    []byte
	}
	cells := make([]cell, 0, n)
	for i := 0; i < n; i++ {
		off, length := s.slotAt(i)
		switch off {
		case cellOffsetDead:
			continue
		case cellOffsetForward:
			co := int(length)
			cp := make([]byte, forwardCellLen)
			copy(cp, b[co:co+forwardCellLen])
			cells = append(cells, cell{slot: i, forward: true, data: cp})
		default:
			cp := make([]byte, int(length))
			copy(cp, b[off:int(off)+int(length)])
			cells = append(cells, cell{slot: i, data: cp})
		}
	}
	// Rewrite from the high end downward, preserving each cell's slot number.
	end := s.bodyLen()
	for _, c := range cells {
		start := end - len(c.data)
		copy(b[start:end], c.data)
		if c.forward {
			s.setSlot(c.slot, cellOffsetForward, uint16(start))
		} else {
			s.setSlot(c.slot, uint16(start), uint16(len(c.data)))
		}
		end = start
	}
	h := s.header()
	h.FreeEnd = uint16(end)
	// free_start is unchanged: the slot directory length does not change.
	s.setHeader(h)
}
