package crypto

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
)

// The encryption super-header is a fixed 256-byte cleartext block at the front of an
// encrypted file, ahead of the page slots. It carries everything needed to derive the key
// and locate the pages without revealing any data: the magic, the page size, the cipher and
// KDF ids, the key epoch, the file id, the KDF salt and work factor, the verification blob,
// and the wrapped DEK (spec 2061 doc 17 §3.1, §4.2, §4.3).
//
// The spec describes a descriptor block at file offset 64 and a wrapped-DEK block at offset
// 128 overlaid on the format header. doc instead gathers those fields into one cleartext
// prefix the encrypting VFS layer owns, because that layer sits beneath the pager and the
// format header itself is encrypted inside page 0. The field set is the same; only the
// placement differs, and it is documented in the M8-d implementation notes.

const (
	// SuperHeaderSize is the length of the cleartext encryption prefix.
	SuperHeaderSize = 256

	headerMagic = 0x444F4345 // "DOCE"
	headerVer   = 1
)

// header is the decoded super-header.
type header struct {
	pageSize    uint32
	cipherID    byte
	kdfID       byte
	epoch       uint32
	fileID      [16]byte
	iterations  int
	salt        [32]byte
	verifyNonce [nonceLen]byte
	verifyBlob  [keyLen + tagLen]byte // 48 bytes: ciphertext of zeros32 + tag
	wrapNonce   [nonceLen]byte
	wrappedDEK  [keyLen + tagLen]byte // 48 bytes: wrapped DEK + tag
}

// header field offsets within the 256-byte block.
const (
	offMagic       = 0
	offPageSize    = 4
	offVersion     = 8
	offCipherID    = 12
	offKDFID       = 13
	offEpoch       = 14
	offFileID      = 20
	offIterations  = 36
	offSalt        = 40
	offVerifyNonce = 72
	offVerifyBlob  = 84
	offWrapNonce   = 132
	offWrappedDEK  = 144
)

// encode renders the super-header into its 256-byte on-disk form.
func (h *header) encode() []byte {
	b := make([]byte, SuperHeaderSize)
	binary.LittleEndian.PutUint32(b[offMagic:], headerMagic)
	binary.LittleEndian.PutUint32(b[offPageSize:], h.pageSize)
	binary.LittleEndian.PutUint32(b[offVersion:], headerVer)
	b[offCipherID] = h.cipherID
	b[offKDFID] = h.kdfID
	binary.LittleEndian.PutUint32(b[offEpoch:], h.epoch)
	copy(b[offFileID:], h.fileID[:])
	binary.LittleEndian.PutUint32(b[offIterations:], uint32(h.iterations))
	copy(b[offSalt:], h.salt[:])
	copy(b[offVerifyNonce:], h.verifyNonce[:])
	copy(b[offVerifyBlob:], h.verifyBlob[:])
	copy(b[offWrapNonce:], h.wrapNonce[:])
	copy(b[offWrappedDEK:], h.wrappedDEK[:])
	return b
}

// decodeHeader parses a 256-byte super-header, checking the magic and version.
func decodeHeader(b []byte) (*header, error) {
	if len(b) < SuperHeaderSize {
		return nil, errors.New("crypto: short encryption header")
	}
	if binary.LittleEndian.Uint32(b[offMagic:]) != headerMagic {
		return nil, errors.New("crypto: not an encrypted doc file")
	}
	if v := binary.LittleEndian.Uint32(b[offVersion:]); v != headerVer {
		return nil, errors.New("crypto: unsupported encryption header version")
	}
	h := &header{
		pageSize:   binary.LittleEndian.Uint32(b[offPageSize:]),
		cipherID:   b[offCipherID],
		kdfID:      b[offKDFID],
		epoch:      binary.LittleEndian.Uint32(b[offEpoch:]),
		iterations: int(binary.LittleEndian.Uint32(b[offIterations:])),
	}
	copy(h.fileID[:], b[offFileID:])
	copy(h.salt[:], b[offSalt:])
	copy(h.verifyNonce[:], b[offVerifyNonce:])
	copy(h.verifyBlob[:], b[offVerifyBlob:])
	copy(h.wrapNonce[:], b[offWrapNonce:])
	copy(h.wrappedDEK[:], b[offWrappedDEK:])
	return h, nil
}

// newHeader builds a fresh super-header for a new encrypted file: a random file id and salt,
// a freshly generated DEK wrapped under the KEK derived from key, and a verification blob.
// It returns the header and the plaintext DEK the caller keeps in memory.
func newHeader(pageSize uint32, key KeyOption) (*header, []byte, error) {
	h := &header{
		pageSize:   pageSize,
		cipherID:   cipherAES256GCM,
		kdfID:      key.kdfID(),
		epoch:      1,
		iterations: key.iterations(),
	}
	if _, err := rand.Read(h.fileID[:]); err != nil {
		return nil, nil, err
	}
	if _, err := rand.Read(h.salt[:]); err != nil {
		return nil, nil, err
	}
	kek, err := key.deriveKEK(h.salt[:], h.iterations)
	if err != nil {
		return nil, nil, err
	}
	dek, err := newDEK()
	if err != nil {
		return nil, nil, err
	}
	if err := h.sealKeys(kek, dek); err != nil {
		return nil, nil, err
	}
	return h, dek, nil
}

// sealKeys fills in the verification blob and the wrapped DEK for the header's current epoch
// under the given KEK. It is used both when creating a file and when rewrapping during
// passphrase rotation.
func (h *header) sealKeys(kek, dek []byte) error {
	if _, err := rand.Read(h.verifyNonce[:]); err != nil {
		return err
	}
	verify, err := sealVerification(kek, h.verifyNonce[:])
	if err != nil {
		return err
	}
	copy(h.verifyBlob[:], verify)

	wrapNonce, wrapped, err := wrapDEK(kek, dek, h.epoch)
	if err != nil {
		return err
	}
	copy(h.wrapNonce[:], wrapNonce)
	copy(h.wrappedDEK[:], wrapped)
	return nil
}

// openKeys derives the KEK from key, verifies it, and unwraps the DEK. A wrong key surfaces
// as ErrWrongKey from either the verification check or the unwrap, before any page is read.
func (h *header) openKeys(key KeyOption) (dek []byte, err error) {
	kek, err := key.deriveKEK(h.salt[:], h.iterations)
	if err != nil {
		return nil, err
	}
	if err := checkVerification(kek, h.verifyNonce[:], h.verifyBlob[:]); err != nil {
		return nil, err
	}
	return unwrapDEK(kek, h.wrapNonce[:], h.wrappedDEK[:], h.epoch)
}
