package format

import "hash/crc32"

// ChecksumAlgo names the integrity algorithm applied to the file header and to
// every content page, recorded in the header's checksum_algo byte (spec 2061
// doc 03 §3.2).
type ChecksumAlgo uint8

const (
	// ChecksumNone disables checksums: the trailing four bytes of a page are
	// zero and never verified. Permitted but not the default.
	ChecksumNone ChecksumAlgo = 0x00
	// ChecksumCRC32C is CRC-32 with the Castagnoli polynomial, the default. It
	// is hardware-accelerated on amd64 and arm64 via Go's hash/crc32, so the
	// per-page cost is negligible (spec 2061 doc 03 §12.1).
	ChecksumCRC32C ChecksumAlgo = 0x01
	// ChecksumXXHash32 is xxHash32, an alternative non-cryptographic checksum.
	ChecksumXXHash32 ChecksumAlgo = 0x02
)

// crc32cTable is the Castagnoli table, built once. crc32.Update against this
// table dispatches to the SSE4.2/ARMv8 CRC instructions when available.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// Supported reports whether this build can compute the named algorithm. An open
// path consults this before trusting a header that names an algorithm, returning
// ErrUnsupportedChecksum when it is false.
func (a ChecksumAlgo) Supported() bool {
	switch a {
	case ChecksumNone, ChecksumCRC32C, ChecksumXXHash32:
		return true
	default:
		return false
	}
}

// String renders the algorithm name for diagnostics.
func (a ChecksumAlgo) String() string {
	switch a {
	case ChecksumNone:
		return "none"
	case ChecksumCRC32C:
		return "crc32c"
	case ChecksumXXHash32:
		return "xxhash32"
	default:
		return "unknown"
	}
}

// Checksum computes the checksum of data under algorithm a. For ChecksumNone it
// returns zero. The result is the u32 written into the header's checksum field
// or a page's trailing four bytes.
func (a ChecksumAlgo) Checksum(data []byte) uint32 {
	switch a {
	case ChecksumNone:
		return 0
	case ChecksumCRC32C:
		return crc32.Update(0, crc32cTable, data)
	case ChecksumXXHash32:
		return xxhash32(data, 0)
	default:
		// An unsupported algorithm should have been rejected at open; reaching
		// here is a programming error.
		panic("doc/format: checksum with unsupported algorithm")
	}
}

// Verify reports whether data checksums to want under algorithm a. For
// ChecksumNone it always reports true, since there is nothing to check.
func (a ChecksumAlgo) Verify(data []byte, want uint32) bool {
	if a == ChecksumNone {
		return true
	}
	return a.Checksum(data) == want
}

// xxHash32 constants from the reference specification.
const (
	xxPrime1 uint32 = 2654435761
	xxPrime2 uint32 = 2246822519
	xxPrime3 uint32 = 3266489917
	xxPrime4 uint32 = 668265263
	xxPrime5 uint32 = 374761393
)

func rotl32(x uint32, r uint) uint32 { return (x << r) | (x >> (32 - r)) }

// xxhash32 is a pure-Go implementation of xxHash32 (single-shot). It follows the
// canonical algorithm: a four-lane accumulator over 16-byte stripes for inputs
// of 16 bytes or more, then a tail mix over remaining 4-byte words and bytes,
// then the avalanche finalizer. It allocates nothing.
func xxhash32(data []byte, seed uint32) uint32 {
	var h uint32
	n := len(data)
	i := 0
	if n >= 16 {
		v1 := seed + xxPrime1 + xxPrime2
		v2 := seed + xxPrime2
		v3 := seed
		v4 := seed - xxPrime1
		for ; i+16 <= n; i += 16 {
			v1 = xxRound(v1, le32(data[i:]))
			v2 = xxRound(v2, le32(data[i+4:]))
			v3 = xxRound(v3, le32(data[i+8:]))
			v4 = xxRound(v4, le32(data[i+12:]))
		}
		h = rotl32(v1, 1) + rotl32(v2, 7) + rotl32(v3, 12) + rotl32(v4, 18)
	} else {
		h = seed + xxPrime5
	}
	h += uint32(n)
	for ; i+4 <= n; i += 4 {
		h += le32(data[i:]) * xxPrime3
		h = rotl32(h, 17) * xxPrime4
	}
	for ; i < n; i++ {
		h += uint32(data[i]) * xxPrime5
		h = rotl32(h, 11) * xxPrime1
	}
	h ^= h >> 15
	h *= xxPrime2
	h ^= h >> 13
	h *= xxPrime3
	h ^= h >> 16
	return h
}

func xxRound(acc, input uint32) uint32 {
	acc += input * xxPrime2
	acc = rotl32(acc, 13)
	acc *= xxPrime1
	return acc
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
