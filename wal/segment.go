package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// A segment is the archive unit for WAL shipping and point-in-time recovery (spec
// 2061 doc 18 §13.5, §14). It carries an ordered run of committed transactions: each
// commit's page images, plus the metadata a restore needs to stop at a target, the
// commit version and its cluster time. Segments are self-describing and checksummed
// so a restore can validate them without the original database.

// SegmentMagic identifies a segment file: "DSEG".
const SegmentMagic uint32 = 0x44534547

// SegmentFormatVersion is the on-disk segment layout version.
const SegmentFormatVersion uint16 = 1

const segHeaderSize = 4 + 2 + 2 + 4 + 4 + 8 + 8 + 8 + 8 + 4 // magic..endTime + crc

var segCRC = crc32.MakeTable(crc32.Castagnoli)

// ErrBadSegment reports a malformed or corrupt segment.
var ErrBadSegment = errors.New("wal: bad segment")

// Commit is one transaction's worth of archived page images with the version and
// cluster time it committed at.
type Commit struct {
	Version     uint64      // oracle commit version, 0 if the source was unannotated
	TimeUnix    int64       // cluster time in unix seconds at commit
	DBSizePages uint32      // database size in pages as of this commit
	Frames      []PageImage // the page images this commit wrote, in log order
}

// Segment is a decoded archive segment.
type Segment struct {
	PageSize     uint32
	BaseVersion  uint64 // the segment holds commits with version > BaseVersion
	EndVersion   uint64 // the highest version in the segment
	BaseTimeUnix int64
	EndTimeUnix  int64
	Commits      []Commit
}

// Encode serialises the segment to a single byte slice.
func (s *Segment) Encode() []byte {
	var body []byte
	tmp := make([]byte, 8)
	put32 := func(dst *[]byte, v uint32) {
		binary.LittleEndian.PutUint32(tmp[:4], v)
		*dst = append(*dst, tmp[:4]...)
	}
	put64 := func(dst *[]byte, v uint64) {
		binary.LittleEndian.PutUint64(tmp[:8], v)
		*dst = append(*dst, tmp[:8]...)
	}
	for _, c := range s.Commits {
		var rec []byte
		put64(&rec, c.Version)
		put64(&rec, uint64(c.TimeUnix))
		put32(&rec, c.DBSizePages)
		put32(&rec, uint32(len(c.Frames)))
		for _, f := range c.Frames {
			put64(&rec, f.PageID)
			rec = append(rec, f.Payload...)
		}
		var crc []byte
		put32(&crc, crc32.Checksum(rec, segCRC))
		body = append(body, rec...)
		body = append(body, crc...)
	}

	hdr := make([]byte, segHeaderSize)
	binary.LittleEndian.PutUint32(hdr[0:4], SegmentMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], SegmentFormatVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], 0) // flags, reserved
	binary.LittleEndian.PutUint32(hdr[8:12], s.PageSize)
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(len(s.Commits)))
	binary.LittleEndian.PutUint64(hdr[16:24], s.BaseVersion)
	binary.LittleEndian.PutUint64(hdr[24:32], s.EndVersion)
	binary.LittleEndian.PutUint64(hdr[32:40], uint64(s.BaseTimeUnix))
	binary.LittleEndian.PutUint64(hdr[40:48], uint64(s.EndTimeUnix))
	binary.LittleEndian.PutUint32(hdr[48:52], crc32.Checksum(hdr[0:48], segCRC))

	return append(hdr, body...)
}

// DecodeSegment parses a segment, verifying the header and every commit checksum.
func DecodeSegment(b []byte) (*Segment, error) {
	if len(b) < segHeaderSize {
		return nil, fmt.Errorf("%w: short header", ErrBadSegment)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != SegmentMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrBadSegment)
	}
	if v := binary.LittleEndian.Uint16(b[4:6]); v != SegmentFormatVersion {
		return nil, fmt.Errorf("%w: format version %d", ErrBadSegment, v)
	}
	if got := crc32.Checksum(b[0:48], segCRC); got != binary.LittleEndian.Uint32(b[48:52]) {
		return nil, fmt.Errorf("%w: header checksum", ErrBadSegment)
	}
	s := &Segment{
		PageSize:     binary.LittleEndian.Uint32(b[8:12]),
		BaseVersion:  binary.LittleEndian.Uint64(b[16:24]),
		EndVersion:   binary.LittleEndian.Uint64(b[24:32]),
		BaseTimeUnix: int64(binary.LittleEndian.Uint64(b[32:40])),
		EndTimeUnix:  int64(binary.LittleEndian.Uint64(b[40:48])),
	}
	commitCount := binary.LittleEndian.Uint32(b[12:16])
	if s.PageSize == 0 {
		return nil, fmt.Errorf("%w: zero page size", ErrBadSegment)
	}

	off := segHeaderSize
	s.Commits = make([]Commit, 0, commitCount)
	for i := uint32(0); i < commitCount; i++ {
		start := off
		if off+24 > len(b) {
			return nil, fmt.Errorf("%w: truncated commit %d", ErrBadSegment, i)
		}
		c := Commit{
			Version:     binary.LittleEndian.Uint64(b[off : off+8]),
			TimeUnix:    int64(binary.LittleEndian.Uint64(b[off+8 : off+16])),
			DBSizePages: binary.LittleEndian.Uint32(b[off+16 : off+20]),
		}
		frameCount := binary.LittleEndian.Uint32(b[off+20 : off+24])
		off += 24
		c.Frames = make([]PageImage, 0, frameCount)
		for f := uint32(0); f < frameCount; f++ {
			if off+8+int(s.PageSize) > len(b) {
				return nil, fmt.Errorf("%w: truncated frame in commit %d", ErrBadSegment, i)
			}
			pid := binary.LittleEndian.Uint64(b[off : off+8])
			off += 8
			payload := make([]byte, s.PageSize)
			copy(payload, b[off:off+int(s.PageSize)])
			off += int(s.PageSize)
			c.Frames = append(c.Frames, PageImage{PageID: pid, Payload: payload})
		}
		if off+4 > len(b) {
			return nil, fmt.Errorf("%w: missing checksum for commit %d", ErrBadSegment, i)
		}
		want := binary.LittleEndian.Uint32(b[off : off+4])
		if got := crc32.Checksum(b[start:off], segCRC); got != want {
			return nil, fmt.Errorf("%w: commit %d checksum", ErrBadSegment, i)
		}
		off += 4
		s.Commits = append(s.Commits, c)
	}
	return s, nil
}
