package format

import (
	"hash/crc32"
	"testing"
)

func TestCRC32CMatchesStdlib(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	want := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	if got := ChecksumCRC32C.Checksum(data); got != want {
		t.Fatalf("CRC32C = %#x, want %#x", got, want)
	}
}

func TestXXHash32EmptyVector(t *testing.T) {
	// The empty-input value for seed 0 is the canonical xxHash32 test vector.
	if got := xxhash32(nil, 0); got != 0x02CC5D05 {
		t.Fatalf("xxhash32(\"\") = %#08x, want 0x02CC5D05", got)
	}
}

func TestXXHash32Deterministic(t *testing.T) {
	// Exercise both the >=16-byte stripe loop and the tail mixer, and assert the
	// function is deterministic and sensitive to a single-bit change.
	for _, n := range []int{1, 4, 7, 15, 16, 31, 64, 8192} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i * 31)
		}
		a := xxhash32(data, 0)
		b := xxhash32(data, 0)
		if a != b {
			t.Fatalf("xxhash32 not deterministic for n=%d", n)
		}
		if n > 0 {
			data[n-1] ^= 0x01
			if xxhash32(data, 0) == a {
				t.Fatalf("xxhash32 insensitive to a bit flip for n=%d", n)
			}
		}
	}
}

func TestChecksumNone(t *testing.T) {
	if got := ChecksumNone.Checksum([]byte("anything")); got != 0 {
		t.Fatalf("ChecksumNone should be 0, got %#x", got)
	}
	if !ChecksumNone.Verify([]byte("anything"), 12345) {
		t.Fatal("ChecksumNone.Verify should always be true")
	}
}

func TestChecksumVerify(t *testing.T) {
	data := []byte("payload bytes for verification")
	for _, algo := range []ChecksumAlgo{ChecksumCRC32C, ChecksumXXHash32} {
		sum := algo.Checksum(data)
		if !algo.Verify(data, sum) {
			t.Errorf("%v.Verify failed on correct sum", algo)
		}
		if algo.Verify(data, sum^0xFFFFFFFF) {
			t.Errorf("%v.Verify accepted wrong sum", algo)
		}
	}
}

func TestChecksumAlgoSupported(t *testing.T) {
	for _, a := range []ChecksumAlgo{ChecksumNone, ChecksumCRC32C, ChecksumXXHash32} {
		if !a.Supported() {
			t.Errorf("%v should be supported", a)
		}
	}
	if ChecksumAlgo(0x7F).Supported() {
		t.Fatal("unknown algo should not be supported")
	}
}

func BenchmarkCRC32C8K(b *testing.B) {
	data := make([]byte, 8192)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		_ = ChecksumCRC32C.Checksum(data)
	}
}

func BenchmarkXXHash32_8K(b *testing.B) {
	data := make([]byte, 8192)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		_ = xxhash32(data, 0)
	}
}
