package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func sampleHeader() Header {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	h := NewHeader(PageSize8K, ChecksumCRC32C, 0xDEADBEEFCAFEF00D, uuid)
	h.PageCount = 42
	h.FileChangeCounter = 7
	h.VersionValidFor = 7
	h.CatalogRoot = 3
	h.FreelistRoot = 9
	h.TxnHighWater = 12345
	h.ApplicationID = 0x646F6342
	h.UserVersion = 99
	return h
}

func TestHeaderRoundTrip(t *testing.T) {
	h := sampleHeader()
	enc := h.Encode()
	if len(enc) != HeaderSize {
		t.Fatalf("encoded header is %d bytes, want %d", len(enc), HeaderSize)
	}
	got, err := DecodeHeader(enc)
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	// Encode does not write the computed checksum back into the source header
	// (value receiver), so set the expectation before the struct comparison.
	h.HeaderChecksum = got.HeaderChecksum
	if got != h {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, h)
	}
	// Re-encode the decoded header: must be byte-for-byte identical.
	if enc2 := got.Encode(); !bytes.Equal(enc, enc2) {
		t.Fatal("re-encoded header bytes differ from original")
	}
}

func TestHeaderMagicBytes(t *testing.T) {
	want := []byte{0x64, 0x6F, 0x63, 0x00, 0x66, 0x6D, 0x74, 0x20, 0x31, 0x0A, 0x00, 0xFF, 0x0D, 0x0A, 0x1A, 0x0A}
	if !bytes.Equal(Magic[:], want) {
		t.Fatalf("magic = % x, want % x", Magic[:], want)
	}
	enc := sampleHeader().Encode()
	if !bytes.Equal(enc[0:16], want) {
		t.Fatal("encoded header does not start with the magic")
	}
}

func TestHeaderChecksumDetectsBitFlip(t *testing.T) {
	enc := sampleHeader().Encode()
	// Flip a bit in every covered byte position and confirm detection. Byte 73
	// is the checksum-algo selector: flipping its low bit turns CRC32C (0x01)
	// into None (0x00), which by definition disables verification. That single
	// case is an inherent limitation of self-describing the algorithm inside the
	// region it protects; it is covered separately by
	// TestHeaderAlgoFlipToOtherAlgoDetected below. Every other byte must be
	// caught by the magic check or the header checksum.
	for i := 0; i < 124; i++ {
		if i == 73 {
			continue
		}
		corrupt := append([]byte(nil), enc...)
		corrupt[i] ^= 0x01
		if _, err := DecodeHeader(corrupt); err == nil {
			t.Fatalf("bit flip at byte %d not detected", i)
		}
	}
}

func TestHeaderAlgoFlipToOtherAlgoDetected(t *testing.T) {
	// Flipping checksum_algo from CRC32C (0x01) to xxHash32 (0x02) reinterprets
	// the stored checksum under a different algorithm, which must fail.
	enc := sampleHeader().Encode()
	enc[73] = byte(ChecksumXXHash32)
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrHeaderCorrupt) {
		t.Fatalf("err = %v, want ErrHeaderCorrupt", err)
	}
}

func TestHeaderErrTooSmall(t *testing.T) {
	if _, err := DecodeHeader(make([]byte, 64)); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("err = %v, want ErrTooSmall", err)
	}
}

func TestHeaderErrNotDocDB(t *testing.T) {
	enc := sampleHeader().Encode()
	enc[1] = 'X' // corrupt the magic
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrNotDocDB) {
		t.Fatalf("err = %v, want ErrNotDocDB", err)
	}
}

func TestHeaderErrUnsupportedMajor(t *testing.T) {
	h := sampleHeader()
	h.FormatMajor = FormatMajorCurrent + 1
	enc := h.Encode() // recomputes a valid checksum over the bumped major
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrUnsupportedMajor) {
		t.Fatalf("err = %v, want ErrUnsupportedMajor", err)
	}
}

func TestHeaderErrInvalidPageSize(t *testing.T) {
	h := sampleHeader()
	enc := h.Encode()
	binary.LittleEndian.PutUint32(enc[20:24], 5000) // not a permitted size
	// Recompute the header checksum so we exercise the page-size check, not the
	// checksum check.
	sum := ChecksumCRC32C.Checksum(enc[0:124])
	binary.LittleEndian.PutUint32(enc[124:128], sum)
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrInvalidPageSize) {
		t.Fatalf("err = %v, want ErrInvalidPageSize", err)
	}
}

func TestHeaderErrUnsupportedChecksum(t *testing.T) {
	enc := sampleHeader().Encode()
	enc[73] = 0x7F // unsupported checksum algo
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrUnsupportedChecksum) {
		t.Fatalf("err = %v, want ErrUnsupportedChecksum", err)
	}
}

func TestHeaderErrUnsupportedFeature(t *testing.T) {
	h := sampleHeader()
	h.FeatureFlags = 1 << 40 // a reserved/unknown required feature bit
	enc := h.Encode()
	if _, err := DecodeHeader(enc); !errors.Is(err, ErrUnsupportedFeature) {
		t.Fatalf("err = %v, want ErrUnsupportedFeature", err)
	}
}

func TestHeaderKnownFeaturesAccepted(t *testing.T) {
	h := sampleHeader()
	h.FeatureFlags = FeatureCompression | FeatureColumnar
	enc := h.Encode()
	if _, err := DecodeHeader(enc); err != nil {
		t.Fatalf("known feature flags should be accepted: %v", err)
	}
}

func TestPageCountForFileLen(t *testing.T) {
	h := sampleHeader()
	h.FileChangeCounter = 7
	h.VersionValidFor = 7
	h.PageCount = 42
	if got := h.PageCountForFileLen(0); got != 42 {
		t.Fatalf("trusting stored count: got %d, want 42", got)
	}
	h.VersionValidFor = 6 // stale
	if got := h.PageCountForFileLen(10 * int64(PageSize8K)); got != 10 {
		t.Fatalf("deriving from file len: got %d, want 10", got)
	}
}

func TestValidPageSize(t *testing.T) {
	for _, ps := range []uint32{PageSize4K, PageSize8K, PageSize16K, PageSize32K, PageSize64K} {
		if !ValidPageSize(ps) {
			t.Errorf("%d should be valid", ps)
		}
	}
	for _, ps := range []uint32{0, 1024, 2048, 5000, 100000} {
		if ValidPageSize(ps) {
			t.Errorf("%d should be invalid", ps)
		}
	}
}

func BenchmarkHeaderEncode(b *testing.B) {
	h := sampleHeader()
	for i := 0; i < b.N; i++ {
		_ = h.Encode()
	}
}

func BenchmarkHeaderDecode(b *testing.B) {
	enc := sampleHeader().Encode()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeHeader(enc)
	}
}
