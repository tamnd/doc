package format

import (
	"errors"
	"testing"
)

func TestPageHeaderRoundTrip(t *testing.T) {
	h := PageHeader{
		Type:        PageHeap,
		Flags:       0x03,
		Codec:       0x01,
		EntryCount:  17,
		PageLSN:     0x0102030405060708,
		CollIDEntry: 99,
		FreeStart:   40,
		FreeEnd:     8000,
		RightSib:    1234,
	}
	p := make([]byte, PageSize8K)
	h.EncodeInto(p)
	got := DecodePageHeader(p)
	if got != h {
		t.Fatalf("page header round-trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

func TestInitPage(t *testing.T) {
	p := make([]byte, PageSize8K)
	InitPage(p, PageHeap, 7)
	h := DecodePageHeader(p)
	if h.Type != PageHeap {
		t.Fatalf("type = %v, want heap", h.Type)
	}
	if h.CollIDEntry != 7 {
		t.Fatalf("collID = %d, want 7", h.CollIDEntry)
	}
	if h.FreeStart != 0 {
		t.Fatalf("free_start = %d, want 0", h.FreeStart)
	}
	if int(h.FreeEnd) != BodySize(PageSize8K) {
		t.Fatalf("free_end = %d, want %d", h.FreeEnd, BodySize(PageSize8K))
	}
	if h.RightSib != NullPage {
		t.Fatalf("right_sib = %d, want NullPage", h.RightSib)
	}
}

func TestPageChecksumWriteVerify(t *testing.T) {
	p := make([]byte, PageSize8K)
	InitPage(p, PageHeap, 1)
	copy(p[BodyOffset:], []byte("some body content"))
	WritePageChecksum(p, ChecksumCRC32C)
	if err := VerifyPageChecksum(p, ChecksumCRC32C); err != nil {
		t.Fatalf("verify after write: %v", err)
	}
	// Flip a body bit: detection.
	p[BodyOffset+3] ^= 0x01
	if err := VerifyPageChecksum(p, ChecksumCRC32C); !errors.Is(err, ErrPageChecksum) {
		t.Fatalf("err = %v, want ErrPageChecksum", err)
	}
}

func TestPageChecksumBitFlipEverywhere(t *testing.T) {
	p := make([]byte, PageSize4K)
	InitPage(p, PageBTreeLeaf, 5)
	for i := range p[:BodyOffset+10] {
		p[i] = byte(i)
	}
	WritePageChecksum(p, ChecksumCRC32C)
	// Flip a bit at several positions across header and body; each must fail.
	for _, pos := range []int{0, 1, 8, 16, 31, 32, 100, len(p) - 5} {
		saved := p[pos]
		p[pos] ^= 0x80
		if err := VerifyPageChecksum(p, ChecksumCRC32C); err == nil {
			t.Errorf("bit flip at %d not detected", pos)
		}
		p[pos] = saved
	}
	// Restored bytes verify again.
	if err := VerifyPageChecksum(p, ChecksumCRC32C); err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
}

func TestPageChecksumNone(t *testing.T) {
	p := make([]byte, PageSize8K)
	InitPage(p, PageHeap, 1)
	WritePageChecksum(p, ChecksumNone)
	p[100] ^= 0xFF
	if err := VerifyPageChecksum(p, ChecksumNone); err != nil {
		t.Fatalf("ChecksumNone should never fail verification: %v", err)
	}
}

func TestPageTypeKnownAndString(t *testing.T) {
	known := []PageType{
		PageFree, PageHeap, PageBTreeInterior, PageBTreeLeaf, PageOverflowHead,
		PageOverflowCont, PageCatalog, PageFreelistTrunk, PageFreelistLeaf,
		PageColumnarDir, PageColumnarSeg, PageHeapFSMap,
	}
	for _, pt := range known {
		if !pt.Known() {
			t.Errorf("%v should be known", pt)
		}
		if pt.String() == "unknown" {
			t.Errorf("%v should have a name", pt)
		}
	}
	if PageType(0xEE).Known() {
		t.Fatal("0xEE should be unknown")
	}
	if PageType(0xEE).String() != "unknown" {
		t.Fatal("0xEE should stringify as unknown")
	}
}

func TestBodySize(t *testing.T) {
	cases := map[uint32]int{
		PageSize4K: 4096 - 32 - 4,
		PageSize8K: 8192 - 32 - 4,
	}
	for ps, want := range cases {
		if got := BodySize(ps); got != want {
			t.Errorf("BodySize(%d) = %d, want %d", ps, got, want)
		}
	}
}
