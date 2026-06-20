package format

import "encoding/binary"

// Overflow chains store the bytes of a document (or large index key) that do not
// fit inline on a heap or B-tree page (spec 2061 doc 03 §7). A chain is a head
// page (PAGE_OVERFLOW_HEAD) carrying a small payload header plus the first run of
// payload bytes, followed by zero or more continuation pages (PAGE_OVERFLOW_CONT)
// whose entire body is payload. The head page records the total payload length
// and an end-to-end CRC32C over the reassembled payload, independent of the
// per-page checksums, so a mis-linked or truncated chain is detected on read.

const (
	// overflowHeadHdr is the body-relative size of the head page's payload
	// header: total_payload_length, inline_payload_length, chain_checksum, and a
	// reserved word (spec 2061 doc 03 §7.2).
	overflowHeadHdr = 16

	// OvflCompressed is flags bit 0 on the head page: the payload is compressed.
	OvflCompressed uint8 = 1 << 0
)

// OverflowHeadCapacity returns the number of payload bytes the head page of a
// chain can hold at the given page size: the body minus the 16-byte payload
// header.
func OverflowHeadCapacity(pageSize uint32) int { return BodySize(pageSize) - overflowHeadHdr }

// OverflowContCapacity returns the number of payload bytes a continuation page
// can hold: the entire body.
func OverflowContCapacity(pageSize uint32) int { return BodySize(pageSize) }

// OverflowPageCount returns the number of pages (head + continuations) needed to
// store a payload of n bytes at the given page size. n may be zero, which still
// needs one head page.
func OverflowPageCount(n int, pageSize uint32) int {
	head := OverflowHeadCapacity(pageSize)
	if n <= head {
		return 1
	}
	rem := n - head
	cont := OverflowContCapacity(pageSize)
	return 1 + (rem+cont-1)/cont
}

// WriteOverflowChain serializes payload across the supplied page buffers, which
// must be pageNos[i]-numbered pages each of length pageSize. It links them via
// right_sibling, stamps the head page's payload header (including the end-to-end
// CRC32C), and sets entry_count on the head to the continuation count. The caller
// allocates the page numbers and buffers (one head + continuations as returned by
// OverflowPageCount) and is responsible for persisting them. collID is the owning
// collection. It returns the head page number.
//
// pages[i] and pageNos[i] are parallel: pages[i] is the buffer for page number
// pageNos[i]. pages[0] becomes the head.
func WriteOverflowChain(payload []byte, pages [][]byte, pageNos []uint32, collID uint32, algo ChecksumAlgo) uint32 {
	n := len(pages)
	chainSum := crc32cOf(payload)
	// Head page.
	InitPage(pages[0], PageOverflowHead, collID)
	headBody := bodyOf(pages[0])
	headCap := len(headBody) - overflowHeadHdr
	inline := len(payload)
	if inline > headCap {
		inline = headCap
	}
	binary.LittleEndian.PutUint32(headBody[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(headBody[4:8], uint32(inline))
	binary.LittleEndian.PutUint32(headBody[8:12], chainSum)
	copy(headBody[overflowHeadHdr:overflowHeadHdr+inline], payload[:inline])
	hh := DecodePageHeader(pages[0])
	hh.EntryCount = uint32(n - 1)
	if n > 1 {
		hh.RightSib = pageNos[1]
	} else {
		hh.RightSib = NullPage
	}
	hh.EncodeInto(pages[0])

	// Continuation pages.
	off := inline
	for i := 1; i < n; i++ {
		InitPage(pages[i], PageOverflowCont, collID)
		body := bodyOf(pages[i])
		take := len(body)
		if off+take > len(payload) {
			take = len(payload) - off
		}
		copy(body[:take], payload[off:off+take])
		off += take
		ph := DecodePageHeader(pages[i])
		if i+1 < n {
			ph.RightSib = pageNos[i+1]
		} else {
			ph.RightSib = NullPage
		}
		ph.EncodeInto(pages[i])
	}
	// Finalize per-page checksums.
	for i := 0; i < n; i++ {
		WritePageChecksum(pages[i], algo)
	}
	return pageNos[0]
}

// OverflowReader reassembles an overflow chain. fetch returns the buffer for a
// given page number (from the pager/buffer pool). ReadOverflowChain starts at the
// head page number and walks right_sibling, appending payload, then verifies the
// assembled length and the end-to-end CRC32C. It returns the raw (still possibly
// compressed) payload bytes; decompression is the caller's concern.
type OverflowReader func(pageNo uint32) ([]byte, error)

// ReadOverflowChain reassembles the payload of the chain whose head is at
// headPage. It verifies per-page checksums via VerifyPageChecksum, the total
// length, and the chain checksum, returning ErrChainCorrupt on any mismatch.
func ReadOverflowChain(headPage uint32, fetch OverflowReader, algo ChecksumAlgo) ([]byte, error) {
	hp, err := fetch(headPage)
	if err != nil {
		return nil, err
	}
	if err := VerifyPageChecksum(hp, algo); err != nil {
		return nil, err
	}
	hh := DecodePageHeader(hp)
	if hh.Type != PageOverflowHead {
		return nil, ErrChainCorrupt
	}
	body := bodyOf(hp)
	total := int(binary.LittleEndian.Uint32(body[0:4]))
	inline := int(binary.LittleEndian.Uint32(body[4:8]))
	wantSum := binary.LittleEndian.Uint32(body[8:12])
	if inline > len(body)-overflowHeadHdr || inline > total {
		return nil, ErrChainCorrupt
	}
	out := make([]byte, 0, total)
	out = append(out, body[overflowHeadHdr:overflowHeadHdr+inline]...)
	next := hh.RightSib
	for next != NullPage {
		cp, err := fetch(next)
		if err != nil {
			return nil, err
		}
		if err := VerifyPageChecksum(cp, algo); err != nil {
			return nil, err
		}
		ch := DecodePageHeader(cp)
		if ch.Type != PageOverflowCont {
			return nil, ErrChainCorrupt
		}
		cb := bodyOf(cp)
		take := len(cb)
		if len(out)+take > total {
			take = total - len(out)
		}
		out = append(out, cb[:take]...)
		next = ch.RightSib
	}
	if len(out) != total {
		return nil, ErrChainCorrupt
	}
	if crc32cOf(out) != wantSum {
		return nil, ErrChainCorrupt
	}
	return out, nil
}

// bodyOf returns the body region of a page buffer.
func bodyOf(p []byte) []byte { return p[BodyOffset : len(p)-ChecksumSize] }

// crc32cOf is the chain checksum: always CRC32C regardless of the page checksum
// algorithm, because the chain checksum is a fixed end-to-end integrity field
// (spec 2061 doc 03 §7.2 names CRC32C explicitly).
func crc32cOf(b []byte) uint32 { return ChecksumCRC32C.Checksum(b) }
