// Package wal implements the write-ahead log substrate that makes committed
// transactions durable (spec 2061 doc 05 §6-14). The WAL is a sidecar file of
// salt-seeded, checksum-chained page-image frames: a transaction appends the new
// image of every page it dirtied, ends with a frame carrying a nonzero commit
// marker, and fsyncs. Recovery walks the chain from the start, applies every
// frame up to the last valid commit marker, and discards a torn or uncommitted
// tail — restoring exactly the committed prefix and nothing else.
//
// The WAL knows nothing about documents: a frame says "page N now holds these
// bytes," never "document X was updated." Everything document-specific lives
// above this layer. This package is the doc-internal substrate the spec calls
// "reused from kv"; doc builds it itself behind the storage SPI seam.
package wal

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
)

// WALHeaderSize is the fixed 32-byte header prefixing the .doc-wal file
// (spec 2061 doc 05 §6.1).
const WALHeaderSize = 32

// FrameHeaderSize is the fixed 24-byte header prefixing each frame's payload
// (spec 2061 doc 05 §6.2).
const FrameHeaderSize = 24

// WALMagic is the WAL file signature at offset 0: "DWL\n". It also encodes byte
// order — a reader on the wrong byte order reads a different magic and fails
// fast rather than misparsing frame offsets (spec 2061 doc 05 §6.1).
const WALMagic uint32 = 0x44574C0A

// WALFormatVersion is the current WAL format version.
const WALFormatVersion uint16 = 1

// FlagDeltaFrames is header flags bit 0: delta frames are permitted in addition
// to full-page images. doc's M1 always logs full-page images (the first-touch
// rule degenerates to "every frame is a full image"), so this is off by default.
const FlagDeltaFrames uint16 = 1 << 0

// LSNEpoch is the per-generation LSN span (2^40). The first frame of a WAL
// generation starts at lsn = checkpoint_seq*LSNEpoch + 1, so LSNs stay globally
// monotone across checkpoint generations (spec 2061 doc 05 §6.2).
const LSNEpoch uint64 = 1 << 40

// Sentinel errors.
var (
	// ErrBadWALMagic reports a missing or byte-swapped WAL magic.
	ErrBadWALMagic = errors.New("wal: bad magic")
	// ErrWALVersion reports an unsupported WAL format version.
	ErrWALVersion = errors.New("wal: unsupported format version")
	// ErrWALHeaderCorrupt reports a WAL header whose checksum does not verify.
	ErrWALHeaderCorrupt = errors.New("wal: header checksum mismatch")
	// ErrWALShort reports an input shorter than the WAL header.
	ErrWALShort = errors.New("wal: input shorter than header")
	// ErrPageSizeMismatch reports a WAL page size disagreeing with the database.
	ErrPageSizeMismatch = errors.New("wal: page size disagrees with database")
	// ErrEmptyCommit reports a commit with no page frames.
	ErrEmptyCommit = errors.New("wal: commit has no frames")
)

// Header is the decoded 32-byte WAL header. Salt1/Salt2 are the generation
// identifier: every frame checksum is seeded by the salts, so frames written
// under a previous generation's salts fail the chain check under the current
// salts even if their bytes are intact (spec 2061 doc 05 §6.1, §6.3).
type Header struct {
	FormatVersion uint16
	Flags         uint16
	PageSize      uint32
	CheckpointSeq uint32
	Salt1         uint32
	Salt2         uint32
}

// NewHeader builds a header for a fresh WAL generation. The caller supplies the
// random salts (from a CSPRNG) so this package stays deterministic for tests.
func NewHeader(pageSize uint32, checkpointSeq, salt1, salt2 uint32) Header {
	return Header{
		FormatVersion: WALFormatVersion,
		Flags:         0,
		PageSize:      pageSize,
		CheckpointSeq: checkpointSeq,
		Salt1:         salt1,
		Salt2:         salt2,
	}
}

// Encode serializes the header into a fresh 32-byte slice, computing the 64-bit
// header checksum over bytes 0..23.
func (h Header) Encode() []byte {
	b := make([]byte, WALHeaderSize)
	binary.LittleEndian.PutUint32(b[0:4], WALMagic)
	binary.LittleEndian.PutUint16(b[4:6], h.FormatVersion)
	binary.LittleEndian.PutUint16(b[6:8], h.Flags)
	binary.LittleEndian.PutUint32(b[8:12], h.PageSize)
	binary.LittleEndian.PutUint32(b[12:16], h.CheckpointSeq)
	binary.LittleEndian.PutUint32(b[16:20], h.Salt1)
	binary.LittleEndian.PutUint32(b[20:24], h.Salt2)
	c1, c2 := headerChecksum(b[0:24])
	binary.LittleEndian.PutUint32(b[24:28], c1)
	binary.LittleEndian.PutUint32(b[28:32], c2)
	return b
}

// DecodeWALHeader parses and validates the 32-byte WAL header: magic, version,
// then checksum (spec 2061 doc 05 §6.1).
func DecodeWALHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < WALHeaderSize {
		return h, ErrWALShort
	}
	if binary.LittleEndian.Uint32(b[0:4]) != WALMagic {
		return h, ErrBadWALMagic
	}
	h.FormatVersion = binary.LittleEndian.Uint16(b[4:6])
	if h.FormatVersion != WALFormatVersion {
		return h, ErrWALVersion
	}
	c1 := binary.LittleEndian.Uint32(b[24:28])
	c2 := binary.LittleEndian.Uint32(b[28:32])
	g1, g2 := headerChecksum(b[0:24])
	if c1 != g1 || c2 != g2 {
		return h, ErrWALHeaderCorrupt
	}
	h.Flags = binary.LittleEndian.Uint16(b[6:8])
	h.PageSize = binary.LittleEndian.Uint32(b[8:12])
	h.CheckpointSeq = binary.LittleEndian.Uint32(b[12:16])
	h.Salt1 = binary.LittleEndian.Uint32(b[16:20])
	h.Salt2 = binary.LittleEndian.Uint32(b[20:24])
	return h, nil
}

// BaseLSN returns the first LSN of this header's generation:
// checkpoint_seq*LSNEpoch + 1 (spec 2061 doc 05 §6.2).
func (h Header) BaseLSN() uint64 {
	return uint64(h.CheckpointSeq)*LSNEpoch + 1
}

// initialPrevChecksum is the seed for the first frame's chained checksum. The
// WAL header is implicitly part of the chain, so the seed is derived from the
// header checksum (spec 2061 doc 05 §6.3).
func (h Header) initialPrevChecksum() uint32 {
	c1, _ := headerChecksum(h.Encode()[0:24])
	return c1
}

// FrameHeader is the decoded 24-byte frame header (spec 2061 doc 05 §6.2).
type FrameHeader struct {
	PageID       uint64
	FrameLSN     uint64
	CommitMarker uint32 // 0 except on the last frame of a commit, where it holds the post-commit db size in pages
	Checksum     uint32 // cumulative salt-seeded checksum
}

// IsCommit reports whether this frame ends a committed transaction.
func (fh FrameHeader) IsCommit() bool { return fh.CommitMarker != 0 }

// encodeFrameHeaderPrefix writes the first 20 bytes of a frame header (page_id,
// frame_lsn, commit_marker) into dst, leaving the 4-byte checksum field for the
// caller. dst must be at least FrameHeaderSize bytes.
func encodeFrameHeaderPrefix(dst []byte, pageID, lsn uint64, commitMarker uint32) {
	binary.LittleEndian.PutUint64(dst[0:8], pageID)
	binary.LittleEndian.PutUint64(dst[8:16], lsn)
	binary.LittleEndian.PutUint32(dst[16:20], commitMarker)
}

// frameChecksum computes the cumulative, salt-seeded frame checksum over the
// 20-byte header prefix and the payload, chained on prevCksum (spec 2061 doc 05
// §6.2). The seed mixes salt1, salt2, and prevCksum so a frame from a prior
// generation, or a reordered/torn frame, breaks the chain.
func frameChecksum(salt1, salt2, prevCksum uint32, headerPrefix, payload []byte) uint32 {
	h := fnv.New32a()
	var seed [12]byte
	binary.LittleEndian.PutUint32(seed[0:4], salt1)
	binary.LittleEndian.PutUint32(seed[4:8], salt2)
	binary.LittleEndian.PutUint32(seed[8:12], prevCksum)
	_, _ = h.Write(seed[:])
	_, _ = h.Write(headerPrefix)
	_, _ = h.Write(payload)
	return h.Sum32()
}

// headerChecksum computes the 64-bit WAL header checksum over the given bytes,
// returned as two 32-bit halves.
func headerChecksum(b []byte) (uint32, uint32) {
	h := fnv.New64a()
	_, _ = h.Write(b)
	sum := h.Sum64()
	return uint32(sum), uint32(sum >> 32)
}

// FrameSize returns the on-disk size of one frame for the given page size:
// the 24-byte header plus the page payload.
func FrameSize(pageSize uint32) int64 {
	return FrameHeaderSize + int64(pageSize)
}

// FrameOffset returns the byte offset of frame number frameNo (1-based) within
// a WAL file of the given page size.
func FrameOffset(frameNo uint32, pageSize uint32) int64 {
	return WALHeaderSize + int64(frameNo-1)*FrameSize(pageSize)
}
