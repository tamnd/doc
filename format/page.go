package format

import "encoding/binary"

// PageType identifies the kind of a content page, stored in byte 0 of the common
// page header (spec 2061 doc 03 §4.2). Page 0 (the database header) carries no
// common header and is not described by a PageType.
type PageType uint8

const (
	PageFree          PageType = 0x00 // on the freelist; contents undefined
	PageHeap          PageType = 0x01 // slotted document heap page
	PageBTreeInterior PageType = 0x02 // B-tree interior node
	PageBTreeLeaf     PageType = 0x03 // B-tree leaf node
	PageOverflowHead  PageType = 0x04 // first page of an overflow chain
	PageOverflowCont  PageType = 0x05 // overflow continuation page
	PageCatalog       PageType = 0x06 // catalog B-tree page
	PageFreelistTrunk PageType = 0x07 // freelist trunk
	PageFreelistLeaf  PageType = 0x08 // freelist leaf run
	PageColumnarDir   PageType = 0x09 // columnar store directory
	PageColumnarSeg   PageType = 0x0A // columnar data segment
	PageHeapFSMap     PageType = 0x0B // per-collection free-space map
)

// Known reports whether t is a defined page type. An unknown type encountered
// while traversing a structure is corruption or a newer format and must raise
// ErrUnknownPageType rather than being interpreted.
func (t PageType) Known() bool { return t <= PageHeapFSMap }

// String renders the page type for diagnostics and the validate command.
func (t PageType) String() string {
	switch t {
	case PageFree:
		return "free"
	case PageHeap:
		return "heap"
	case PageBTreeInterior:
		return "btree-interior"
	case PageBTreeLeaf:
		return "btree-leaf"
	case PageOverflowHead:
		return "overflow-head"
	case PageOverflowCont:
		return "overflow-cont"
	case PageCatalog:
		return "catalog"
	case PageFreelistTrunk:
		return "freelist-trunk"
	case PageFreelistLeaf:
		return "freelist-leaf"
	case PageColumnarDir:
		return "columnar-dir"
	case PageColumnarSeg:
		return "columnar-segment"
	case PageHeapFSMap:
		return "heap-fsmap"
	default:
		return "unknown"
	}
}

// PageHeaderSize is the length of the common page header that prefixes every
// content page (spec 2061 doc 03 §4.1).
const PageHeaderSize = 32

// ChecksumSize is the length of the trailing page checksum.
const ChecksumSize = 4

// BodyOffset is the byte offset within a page where the body (and, for slotted
// pages, the slot directory) begins.
const BodyOffset = PageHeaderSize

// PageHeader is the decoded common page header. free_start and free_end are
// expressed relative to the start of the body (file offset = pageNo*pageSize +
// BodyOffset + free_start), matching the spec's convention.
type PageHeader struct {
	Type        PageType
	Flags       uint8
	Codec       uint8
	EntryCount  uint32
	PageLSN     uint64
	CollIDEntry uint32 // collection_id; the field is named collection_id on disk
	FreeStart   uint16
	FreeEnd     uint16
	RightSib    uint32
}

// EncodeInto writes the common page header into the first 32 bytes of p. It does
// not touch the body or the checksum.
func (h *PageHeader) EncodeInto(p []byte) {
	_ = p[PageHeaderSize-1]
	p[0] = byte(h.Type)
	p[1] = h.Flags
	p[2] = h.Codec
	p[3] = 0 // reserved
	binary.LittleEndian.PutUint32(p[4:8], h.EntryCount)
	binary.LittleEndian.PutUint64(p[8:16], h.PageLSN)
	binary.LittleEndian.PutUint32(p[16:20], h.CollIDEntry)
	binary.LittleEndian.PutUint16(p[20:22], h.FreeStart)
	binary.LittleEndian.PutUint16(p[22:24], h.FreeEnd)
	binary.LittleEndian.PutUint32(p[24:28], h.RightSib)
	binary.LittleEndian.PutUint32(p[28:32], 0) // reserved2
}

// DecodePageHeader reads the common page header from the first 32 bytes of p.
func DecodePageHeader(p []byte) PageHeader {
	_ = p[PageHeaderSize-1]
	return PageHeader{
		Type:        PageType(p[0]),
		Flags:       p[1],
		Codec:       p[2],
		EntryCount:  binary.LittleEndian.Uint32(p[4:8]),
		PageLSN:     binary.LittleEndian.Uint64(p[8:16]),
		CollIDEntry: binary.LittleEndian.Uint32(p[16:20]),
		FreeStart:   binary.LittleEndian.Uint16(p[20:22]),
		FreeEnd:     binary.LittleEndian.Uint16(p[22:24]),
		RightSib:    binary.LittleEndian.Uint32(p[24:28]),
	}
}

// BodySize returns the number of body bytes on a page of the given size: the
// page minus the 32-byte common header minus the 4-byte trailing checksum.
func BodySize(pageSize uint32) int { return int(pageSize) - PageHeaderSize - ChecksumSize }

// ComputePageChecksum returns the checksum over bytes 0..pageSize-5 of p under
// algo — the common header plus the body, but not the trailing four checksum
// bytes themselves (spec 2061 doc 03 §4.4).
func ComputePageChecksum(p []byte, algo ChecksumAlgo) uint32 {
	return algo.Checksum(p[:len(p)-ChecksumSize])
}

// WritePageChecksum computes and writes the trailing page checksum in place,
// returning the value written. Called by the pager just before a page leaves the
// buffer pool.
func WritePageChecksum(p []byte, algo ChecksumAlgo) uint32 {
	sum := ComputePageChecksum(p, algo)
	binary.LittleEndian.PutUint32(p[len(p)-ChecksumSize:], sum)
	return sum
}

// VerifyPageChecksum recomputes and checks the trailing page checksum, returning
// ErrPageChecksum on mismatch. With ChecksumNone it always succeeds.
func VerifyPageChecksum(p []byte, algo ChecksumAlgo) error {
	stored := binary.LittleEndian.Uint32(p[len(p)-ChecksumSize:])
	if !algo.Verify(p[:len(p)-ChecksumSize], stored) {
		return ErrPageChecksum
	}
	return nil
}

// InitPage stamps a freshly allocated page buffer of size pageSize with a header
// of the given type and collection id, an empty body (free space spanning the
// whole body), and a zero LSN. It is the starting point for a new heap, index,
// or overflow page before any cell is written.
func InitPage(p []byte, t PageType, collID uint32) {
	for i := range p {
		p[i] = 0
	}
	h := PageHeader{
		Type:        t,
		CollIDEntry: collID,
		FreeStart:   0,
		FreeEnd:     uint16(len(p) - PageHeaderSize - ChecksumSize),
		RightSib:    NullPage,
	}
	h.EncodeInto(p)
}
