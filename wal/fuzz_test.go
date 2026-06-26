package wal

import (
	"bytes"
	"testing"
)

// segmentSeeds returns encoded segments of a few shapes (empty, one commit, several commits with
// multi-page frames) so the decoder fuzzer mutates from valid frames rather than only noise.
func segmentSeeds(pageSize uint32) [][]byte {
	mk := func(commits []Commit) []byte {
		s := &Segment{
			PageSize:     pageSize,
			BaseVersion:  10,
			EndVersion:   10 + uint64(len(commits)),
			BaseTimeUnix: 1_700_000_000,
			EndTimeUnix:  1_700_000_100,
			Commits:      commits,
		}
		return s.Encode()
	}
	page := func(id uint64, fill byte) PageImage {
		p := make([]byte, pageSize)
		for i := range p {
			p[i] = fill
		}
		return PageImage{PageID: id, Payload: p}
	}
	var seeds [][]byte
	seeds = append(seeds, mk(nil))
	seeds = append(seeds, mk([]Commit{{Version: 11, TimeUnix: 1_700_000_001, DBSizePages: 4, Frames: []PageImage{page(1, 0xaa)}}}))
	seeds = append(seeds, mk([]Commit{
		{Version: 11, TimeUnix: 1_700_000_001, DBSizePages: 4, Frames: []PageImage{page(1, 0x01), page(2, 0x02)}},
		{Version: 12, TimeUnix: 1_700_000_002, DBSizePages: 6, Frames: []PageImage{page(3, 0x03)}},
	}))
	return seeds
}

// FuzzDecodeSegment feeds arbitrary bytes to the segment decoder. DecodeSegment parses untrusted
// archive bytes during point-in-time recovery, so the contract from spec 19 §18 is hard: it must
// never panic, hang, or read out of bounds, no matter how the length, count, or checksum fields
// are corrupted. A returned error is the correct outcome for a bad segment; a panic is a bug.
func FuzzDecodeSegment(f *testing.F) {
	for _, s := range segmentSeeds(64) {
		f.Add(s)
	}
	// A handful of broken framings so the corpus carries the early-reject paths.
	f.Add([]byte{})
	f.Add(make([]byte, segHeaderSize)) // zeroed header, bad magic
	f.Fuzz(func(t *testing.T, data []byte) {
		seg, err := DecodeSegment(data)
		if err != nil {
			return // rejecting a corrupt segment is fine
		}
		// A segment that decodes must re-encode to the same bytes: the format is canonical, so a
		// successful decode followed by Encode is a fixed point. A mismatch means the decoder
		// accepted bytes the encoder would never produce.
		if got := seg.Encode(); !bytes.Equal(got, data) {
			t.Fatalf("decode/encode not a fixed point:\n in  = %x\n out = %x", data, got)
		}
	})
}

// headerSeeds returns valid 32-byte WAL headers across a spread of page sizes and salts.
func headerSeeds() [][]byte {
	var seeds [][]byte
	for _, ps := range []uint32{512, 4096, 65536} {
		seeds = append(seeds, NewHeader(ps, 1, 0xdeadbeef, 0x12345678).Encode())
	}
	seeds = append(seeds, NewHeader(4096, 0, 0, 0).Encode())
	return seeds
}

// FuzzDecodeWALHeader feeds arbitrary bytes to the 32-byte WAL header decoder. Like the segment
// decoder it parses untrusted bytes on open and recovery and must never panic or read out of
// bounds. When a header decodes cleanly it must round-trip: re-encoding the decoded header
// reproduces the original 32 bytes exactly.
func FuzzDecodeWALHeader(f *testing.F) {
	for _, s := range headerSeeds() {
		f.Add(s)
	}
	f.Add([]byte{})
	f.Add(make([]byte, WALHeaderSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := DecodeWALHeader(data)
		if err != nil {
			return
		}
		got := h.Encode()
		// The decoder reads exactly the first WALHeaderSize bytes; compare against that prefix.
		if !bytes.Equal(got, data[:WALHeaderSize]) {
			t.Fatalf("header round-trip mismatch:\n in  = %x\n out = %x", data[:WALHeaderSize], got)
		}
	})
}
