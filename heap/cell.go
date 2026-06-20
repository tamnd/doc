// Package heap is doc's slotted-page document record store: the M1 layer that
// turns opaque BSON documents into RID-addressed records on heap pages, spilling
// oversized documents to overflow chains and relocating grown documents behind
// forwarding tombstones (spec 2061 doc 04 §3-§8). It implements storage.RecordStore
// over the pager, reusing format's slotted-page and overflow-chain primitives.
//
// What M1 ships here is the durable single-collection record store: Insert,
// Lookup, Scan, in-place and grow-and-move Update, and tombstone Delete. The cell
// header carries the MVCC version and previous-version fields the spec defines,
// but M1 is single-version (no version-chain traversal); the chain walk, snapshot
// visibility, and write-conflict detection arrive with the MVCC layer in M2.
package heap

import "encoding/binary"

// The per-record cell header prefixes every heap cell (spec 2061 doc 04 §4.5). It
// is 24 bytes: a magic byte that distinguishes a live cell from free bytes, a
// flags byte, the inline BSON length, the MVCC write version, and the RID of the
// previous version (zero in M1). Two reserved bytes pad the BSON payload to a
// 4-byte boundary.
const (
	cellHeaderSize = 24
	cellMagic      = 0xDC

	// Cell flag bits (spec 2061 doc 04 §4.5).
	cellTombstone uint8 = 0x01 // a delete marker; no payload
	cellOverflow  uint8 = 0x02 // payload is an overflow pointer, not inline BSON
	cellHasVer    uint8 = 0x04 // the version field is populated
)

// cellHeader is the decoded form of the 24-byte prefix.
type cellHeader struct {
	flags   uint8
	bsonLen uint32
	version uint64
	prevPtr uint64 // encoded RID of the previous version, 0 if first
}

// encode writes the header into the first cellHeaderSize bytes of dst.
func (h cellHeader) encode(dst []byte) {
	_ = dst[cellHeaderSize-1]
	dst[0] = cellMagic
	dst[1] = h.flags
	dst[2] = 0
	dst[3] = 0
	binary.LittleEndian.PutUint32(dst[4:8], h.bsonLen)
	binary.LittleEndian.PutUint64(dst[8:16], h.version)
	binary.LittleEndian.PutUint64(dst[16:24], h.prevPtr)
}

// decodeCellHeader reads a cell header from the front of cell. ok is false if the
// magic byte is wrong, which means the bytes are not a live cell.
func decodeCellHeader(cell []byte) (cellHeader, bool) {
	if len(cell) < cellHeaderSize || cell[0] != cellMagic {
		return cellHeader{}, false
	}
	return cellHeader{
		flags:   cell[1],
		bsonLen: binary.LittleEndian.Uint32(cell[4:8]),
		version: binary.LittleEndian.Uint64(cell[8:16]),
		prevPtr: binary.LittleEndian.Uint64(cell[16:24]),
	}, true
}

// The overflow pointer replaces the inline BSON payload when a document spills to
// an overflow chain (spec 2061 doc 04 §8.2): a 4-byte first-page number, a 4-byte
// total payload length, and 8 reserved bytes, for a 16-byte body atop the 24-byte
// cell header (a 40-byte cell).
const overflowPtrSize = 16

func encodeOverflowPtr(dst []byte, firstPage uint32, totalLen uint32) {
	_ = dst[overflowPtrSize-1]
	binary.LittleEndian.PutUint32(dst[0:4], firstPage)
	binary.LittleEndian.PutUint32(dst[4:8], totalLen)
	for i := 8; i < overflowPtrSize; i++ {
		dst[i] = 0
	}
}

func decodeOverflowPtr(src []byte) (firstPage uint32, totalLen uint32) {
	return binary.LittleEndian.Uint32(src[0:4]), binary.LittleEndian.Uint32(src[4:8])
}
