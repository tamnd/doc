package format

import (
	"encoding/binary"
)

// HeaderSize is the fixed length of the database header prefix on page 0. Every
// field below lives at a constant file offset within these 128 bytes so a reader
// can parse magic, version, and page size in a single 128-byte read before it
// knows the page size (spec 2061 doc 03 §3.1).
const HeaderSize = 128

// Magic is the 16-byte file signature at offset 0 (spec 2061 doc 03 §3.3). The
// embedded NULs, the 0xFF byte, and the CR/LF/SUB/LF bytes catch text-mode
// mangling, codepage transliteration, and DOS EOF truncation respectively.
var Magic = [16]byte{
	0x64, 0x6F, 0x63, 0x00, // "doc\0"
	0x66, 0x6D, 0x74, 0x20, // "fmt "
	0x31, 0x0A, 0x00, 0xFF, // "1\n", NUL, 0xFF
	0x0D, 0x0A, 0x1A, 0x0A, // CR LF SUB LF
}

// Format version constants. FormatMajorCurrent is the highest major this build
// writes and the highest it will open; a file with a greater major is rejected
// with ErrUnsupportedMajor.
const (
	FormatMajorCurrent uint16 = 1
	FormatMinorCurrent uint16 = 0
)

// NullPage is the sentinel page number meaning "no page" in header pointer
// fields (freelist_root, catalog_root, columnar_root) and in page-header
// sibling pointers.
const NullPage uint32 = 0xFFFFFFFF

// Permitted page sizes. Fixed at creation; any other value is ErrInvalidPageSize.
const (
	PageSize4K  uint32 = 4096
	PageSize8K  uint32 = 8192 // the default
	PageSize16K uint32 = 16384
	PageSize32K uint32 = 32768
	PageSize64K uint32 = 65536
)

// ValidPageSize reports whether ps is one of the five permitted sizes.
func ValidPageSize(ps uint32) bool {
	switch ps {
	case PageSize4K, PageSize8K, PageSize16K, PageSize32K, PageSize64K:
		return true
	default:
		return false
	}
}

// Feature-flag bits in the feature_flags bitmask at offset 64.
const (
	FeatureEncryption  uint64 = 1 << 0
	FeatureCompression uint64 = 1 << 1
	FeatureColumnar    uint64 = 1 << 2
	FeaturePageNo64    uint64 = 1 << 3 // reserved; not used in format version 1
)

// requiredFeatureMask is the set of feature bits that a reader MUST understand to
// open the file safely. In format version 1 no optional feature is marked
// required: encryption and compression are transparent to a reader that has the
// keys/codec, and an unknown bit above the defined ones is treated as required
// to be safe.
const knownFeatureMask = FeatureEncryption | FeatureCompression | FeatureColumnar | FeaturePageNo64

// Header is the decoded database header (spec 2061 doc 03 §3.2). Field order and
// types mirror the on-disk layout; Encode/Decode map between this struct and the
// 128 bytes on page 0.
type Header struct {
	FormatMajor       uint16
	FormatMinor       uint16
	PageSize          uint32
	PageCount         uint32
	FileChangeCounter uint32
	VersionValidFor   uint32
	FreelistRoot      uint32
	FreelistPageCount uint32
	CatalogRoot       uint32
	SchemaCookie      uint32
	ColumnarRoot      uint32
	TxnHighWater      uint64
	FeatureFlags      uint64
	DefaultCodec      uint8
	ChecksumAlgo      ChecksumAlgo
	TextEncoding      uint8
	EncryptionFlags   uint8
	ApplicationID     uint32
	UserVersion       uint32
	WALSalt           uint64
	DatabaseUUID      [16]byte
	// HeaderChecksum is the value read from or written to offset 124. After a
	// successful Decode it equals the recomputed checksum; Encode fills it in.
	HeaderChecksum uint32
}

// NewHeader returns a Header for a freshly created database with the given page
// size and checksum algorithm, all pointers null and all counters zero. The
// caller supplies the random wal salt and uuid (drawn from a CSPRNG by the pager
// at create time) so that this package stays free of randomness and is fully
// deterministic for tests.
func NewHeader(pageSize uint32, algo ChecksumAlgo, walSalt uint64, uuid [16]byte) Header {
	return Header{
		FormatMajor:       FormatMajorCurrent,
		FormatMinor:       FormatMinorCurrent,
		PageSize:          pageSize,
		PageCount:         1, // page 0 (the header) exists
		FileChangeCounter: 0,
		VersionValidFor:   0,
		FreelistRoot:      NullPage,
		FreelistPageCount: 0,
		CatalogRoot:       NullPage,
		SchemaCookie:      0,
		ColumnarRoot:      NullPage,
		TxnHighWater:      0,
		FeatureFlags:      0,
		DefaultCodec:      0,
		ChecksumAlgo:      algo,
		TextEncoding:      1, // UTF-8, the only defined value
		EncryptionFlags:   0,
		WALSalt:           walSalt,
		DatabaseUUID:      uuid,
	}
}

// Encode serializes the header into a freshly allocated 128-byte slice with the
// header_checksum field computed over bytes 0..123 under h.ChecksumAlgo. The
// returned slice is the fixed prefix only; the caller pads it to page_size with
// zeros before writing page 0. It takes a value receiver so it can be called on
// a freshly constructed header (NewHeader(...).Encode()); the HeaderChecksum
// field of the caller's header is not modified.
func (h Header) Encode() []byte {
	if !h.ChecksumAlgo.Supported() {
		panic("doc/format: Encode with unsupported checksum algorithm")
	}
	b := make([]byte, HeaderSize)
	copy(b[0:16], Magic[:])
	binary.LittleEndian.PutUint16(b[16:18], h.FormatMajor)
	binary.LittleEndian.PutUint16(b[18:20], h.FormatMinor)
	binary.LittleEndian.PutUint32(b[20:24], h.PageSize)
	binary.LittleEndian.PutUint32(b[24:28], h.PageCount)
	binary.LittleEndian.PutUint32(b[28:32], h.FileChangeCounter)
	binary.LittleEndian.PutUint32(b[32:36], h.VersionValidFor)
	binary.LittleEndian.PutUint32(b[36:40], h.FreelistRoot)
	binary.LittleEndian.PutUint32(b[40:44], h.FreelistPageCount)
	binary.LittleEndian.PutUint32(b[44:48], h.CatalogRoot)
	binary.LittleEndian.PutUint32(b[48:52], h.SchemaCookie)
	binary.LittleEndian.PutUint32(b[52:56], h.ColumnarRoot)
	binary.LittleEndian.PutUint64(b[56:64], h.TxnHighWater)
	binary.LittleEndian.PutUint64(b[64:72], h.FeatureFlags)
	b[72] = h.DefaultCodec
	b[73] = byte(h.ChecksumAlgo)
	b[74] = h.TextEncoding
	b[75] = h.EncryptionFlags
	binary.LittleEndian.PutUint32(b[76:80], h.ApplicationID)
	binary.LittleEndian.PutUint32(b[80:84], h.UserVersion)
	binary.LittleEndian.PutUint64(b[84:92], h.WALSalt)
	copy(b[92:108], h.DatabaseUUID[:])
	// b[108:124] reserved, already zero.
	sum := h.ChecksumAlgo.Checksum(b[0:124])
	binary.LittleEndian.PutUint32(b[124:128], sum)
	return b
}

// DecodeHeader parses and validates the 128-byte header prefix following the
// open sequence of spec 2061 doc 03 §3.4: magic, then checksum-algo support,
// then header checksum, then major version, then page size, then feature flags.
// Each field is validated before any later field is trusted. The input must be
// at least HeaderSize bytes; extra bytes (the rest of page 0) are ignored.
func DecodeHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize {
		return h, ErrTooSmall
	}
	// Step 2: magic.
	var magic [16]byte
	copy(magic[:], b[0:16])
	if magic != Magic {
		return h, ErrNotDocDB
	}
	// Step 3: checksum algorithm support (read before it is used in step 4).
	algo := ChecksumAlgo(b[73])
	if !algo.Supported() {
		return h, ErrUnsupportedChecksum
	}
	// Step 4: header checksum over bytes 0..123.
	stored := binary.LittleEndian.Uint32(b[124:128])
	if !algo.Verify(b[0:124], stored) {
		return h, ErrHeaderCorrupt
	}
	// Step 5: major version.
	h.FormatMajor = binary.LittleEndian.Uint16(b[16:18])
	if h.FormatMajor > FormatMajorCurrent {
		return h, ErrUnsupportedMajor
	}
	// Step 6: page size.
	h.PageSize = binary.LittleEndian.Uint32(b[20:24])
	if !ValidPageSize(h.PageSize) {
		return h, ErrInvalidPageSize
	}
	// Step 7: feature flags - any bit outside the known set is treated as a
	// required feature we do not implement.
	h.FeatureFlags = binary.LittleEndian.Uint64(b[64:72])
	if h.FeatureFlags&^knownFeatureMask != 0 {
		return h, ErrUnsupportedFeature
	}
	// Remaining fields: trusted now that the header is authenticated.
	h.FormatMinor = binary.LittleEndian.Uint16(b[18:20])
	h.PageCount = binary.LittleEndian.Uint32(b[24:28])
	h.FileChangeCounter = binary.LittleEndian.Uint32(b[28:32])
	h.VersionValidFor = binary.LittleEndian.Uint32(b[32:36])
	h.FreelistRoot = binary.LittleEndian.Uint32(b[36:40])
	h.FreelistPageCount = binary.LittleEndian.Uint32(b[40:44])
	h.CatalogRoot = binary.LittleEndian.Uint32(b[44:48])
	h.SchemaCookie = binary.LittleEndian.Uint32(b[48:52])
	h.ColumnarRoot = binary.LittleEndian.Uint32(b[52:56])
	h.TxnHighWater = binary.LittleEndian.Uint64(b[56:64])
	h.DefaultCodec = b[72]
	h.ChecksumAlgo = algo
	h.TextEncoding = b[74]
	h.EncryptionFlags = b[75]
	h.ApplicationID = binary.LittleEndian.Uint32(b[76:80])
	h.UserVersion = binary.LittleEndian.Uint32(b[80:84])
	h.WALSalt = binary.LittleEndian.Uint64(b[84:92])
	copy(h.DatabaseUUID[:], b[92:108])
	h.HeaderChecksum = stored
	return h, nil
}

// PageCountForFileLen returns the page count a reader should trust: the stored
// PageCount when FileChangeCounter equals VersionValidFor, otherwise the count
// derived from the physical file length (spec 2061 doc 03 §3.4 step 8).
func (h *Header) PageCountForFileLen(fileLen int64) uint32 {
	if h.FileChangeCounter == h.VersionValidFor {
		return h.PageCount
	}
	return uint32(fileLen / int64(h.PageSize))
}
