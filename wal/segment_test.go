package wal

import (
	"bytes"
	"errors"
	"testing"
)

func sampleSegment(pageSize int) *Segment {
	page := func(b byte) []byte {
		p := make([]byte, pageSize)
		for i := range p {
			p[i] = b
		}
		return p
	}
	return &Segment{
		PageSize:     uint32(pageSize),
		BaseVersion:  10,
		EndVersion:   12,
		BaseTimeUnix: 1000,
		EndTimeUnix:  1200,
		Commits: []Commit{
			{
				Version:     11,
				TimeUnix:    1100,
				DBSizePages: 4,
				Frames: []PageImage{
					{PageID: 0, Payload: page(0x01)},
					{PageID: 3, Payload: page(0x02)},
				},
			},
			{
				Version:     12,
				TimeUnix:    1200,
				DBSizePages: 5,
				Frames:      []PageImage{{PageID: 4, Payload: page(0x03)}},
			},
		},
	}
}

func TestSegmentEncodeDecodeRoundTrip(t *testing.T) {
	const pageSize = 256
	in := sampleSegment(pageSize)
	out, err := DecodeSegment(in.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PageSize != in.PageSize || out.BaseVersion != in.BaseVersion || out.EndVersion != in.EndVersion {
		t.Fatalf("header mismatch: %+v vs %+v", out, in)
	}
	if out.BaseTimeUnix != in.BaseTimeUnix || out.EndTimeUnix != in.EndTimeUnix {
		t.Fatalf("time mismatch: %+v vs %+v", out, in)
	}
	if len(out.Commits) != len(in.Commits) {
		t.Fatalf("commit count = %d, want %d", len(out.Commits), len(in.Commits))
	}
	for i, c := range out.Commits {
		want := in.Commits[i]
		if c.Version != want.Version || c.TimeUnix != want.TimeUnix || c.DBSizePages != want.DBSizePages {
			t.Fatalf("commit %d metadata mismatch: %+v vs %+v", i, c, want)
		}
		if len(c.Frames) != len(want.Frames) {
			t.Fatalf("commit %d frame count = %d, want %d", i, len(c.Frames), len(want.Frames))
		}
		for j, f := range c.Frames {
			wf := want.Frames[j]
			if f.PageID != wf.PageID || !bytes.Equal(f.Payload, wf.Payload) {
				t.Fatalf("commit %d frame %d mismatch", i, j)
			}
		}
	}
}

func TestSegmentEmptyRoundTrips(t *testing.T) {
	in := &Segment{PageSize: 128, BaseVersion: 7, EndVersion: 7}
	out, err := DecodeSegment(in.Encode())
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(out.Commits) != 0 || out.BaseVersion != 7 {
		t.Fatalf("empty segment decoded wrong: %+v", out)
	}
}

func TestDecodeSegmentRejectsShortBuffer(t *testing.T) {
	if _, err := DecodeSegment([]byte{1, 2, 3}); !errors.Is(err, ErrBadSegment) {
		t.Fatalf("short buffer error = %v, want ErrBadSegment", err)
	}
}

func TestDecodeSegmentRejectsBadMagic(t *testing.T) {
	b := sampleSegment(128).Encode()
	b[0] ^= 0xFF
	if _, err := DecodeSegment(b); !errors.Is(err, ErrBadSegment) {
		t.Fatalf("bad magic error = %v, want ErrBadSegment", err)
	}
}

func TestDecodeSegmentCatchesHeaderCorruption(t *testing.T) {
	b := sampleSegment(128).Encode()
	// Flip a byte inside the header that the header CRC covers (the page size field).
	b[8] ^= 0x01
	if _, err := DecodeSegment(b); !errors.Is(err, ErrBadSegment) {
		t.Fatalf("header corruption error = %v, want ErrBadSegment", err)
	}
}

func TestDecodeSegmentCatchesCommitCorruption(t *testing.T) {
	b := sampleSegment(128).Encode()
	// Flip a byte deep in the body (a page payload), which the per-commit CRC covers.
	b[len(b)-10] ^= 0x01
	if _, err := DecodeSegment(b); !errors.Is(err, ErrBadSegment) {
		t.Fatalf("commit corruption error = %v, want ErrBadSegment", err)
	}
}
