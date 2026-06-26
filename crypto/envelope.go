// Package crypto implements doc's encryption at rest: the per-page AEAD envelope, the
// two-tier key hierarchy, the cleartext encryption header, and a VFS layer that encrypts
// the main database file transparently beneath the pager (spec 2061 doc 17 §3 to §7).
//
// Everything here is standard-library crypto: crypto/aes, crypto/cipher, crypto/sha256,
// crypto/hkdf, crypto/pbkdf2, crypto/rand. The module carries no third-party dependency, so
// two spec choices are substituted and called out where they are made: the passphrase KDF
// is PBKDF2-HMAC-SHA256 rather than Argon2id (Argon2 lives in golang.org/x/crypto), and the
// only cipher is AES-256-GCM (ChaCha20-Poly1305 also lives in golang.org/x/crypto). Both
// substitutions keep the zero-dependency invariant the rest of the module holds to.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
)

const (
	// keyLen is the AES-256 key length in bytes.
	keyLen = 32
	// nonceLen is the GCM nonce length: 96 bits, the recommended size.
	nonceLen = 12
	// tagLen is the GCM authentication tag length: 128 bits.
	tagLen = 16
	// EnvelopeOverhead is the per-page on-disk cost of the envelope: a stored nonce plus
	// the trailing GCM tag (spec 2061 doc 17 §3.3).
	EnvelopeOverhead = nonceLen + tagLen
)

// ErrIntegrityViolation is returned when an AEAD tag does not verify on a page read. It
// covers tampering, page substitution, cross-file copying, and storage corruption alike;
// doc fails closed and never falls back to returning unauthenticated bytes (spec 2061 doc
// 17 §7.2).
var ErrIntegrityViolation = errors.New("doc: page integrity check failed: AEAD tag mismatch")

// sealPage encrypts one page plaintext into the on-disk envelope: a freshly random nonce,
// the AES-256-GCM ciphertext, and the appended tag. The nonce is stored in the envelope, so
// decryption never re-derives it. A random 96-bit nonce per write keeps (key, nonce) pairs
// unique with overwhelming probability, which is the property GCM needs; doc takes the
// NIST SP 800-38D RBG construction rather than threading the page LSN through the VFS seam
// to derive the nonce deterministically.
//
// The AAD binds the page number, key epoch, and file id without encrypting them, so a page
// silently relocated to another page number, replayed from another epoch, or copied out of
// a different file fails authentication (spec 2061 doc 17 §3.3, §7.1).
func sealPage(dek []byte, pageNum uint32, epoch uint32, fileID [16]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aad := pageAAD(pageNum, epoch, fileID)
	// Seal appends ciphertext+tag to the nonce prefix, producing nonce||ct||tag in one
	// allocation.
	return gcm.Seal(nonce[:nonceLen:nonceLen], nonce, plaintext, aad), nil
}

// openPage reverses sealPage. It reads the stored nonce, reconstructs the AAD from the page
// coordinates, and authenticates and decrypts. A tag mismatch returns ErrIntegrityViolation
// rather than the error GCM produces, so the fail-closed contract surfaces a single, stable
// error to callers.
func openPage(dek []byte, pageNum uint32, epoch uint32, fileID [16]byte, envelope []byte) ([]byte, error) {
	if len(envelope) < EnvelopeOverhead {
		return nil, ErrIntegrityViolation
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := envelope[:nonceLen]
	ct := envelope[nonceLen:]
	aad := pageAAD(pageNum, epoch, fileID)
	plaintext, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrIntegrityViolation
	}
	return plaintext, nil
}

// pageAAD builds the additional authenticated data for a page: page number and key epoch as
// big-endian uint32s followed by the 16-byte file id (spec 2061 doc 17 §3.3).
func pageAAD(pageNum uint32, epoch uint32, fileID [16]byte) []byte {
	aad := make([]byte, 4+4+16)
	binary.BigEndian.PutUint32(aad[0:4], pageNum)
	binary.BigEndian.PutUint32(aad[4:8], epoch)
	copy(aad[8:], fileID[:])
	return aad
}

// newGCM builds an AES-256-GCM AEAD from a 32-byte key.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, errors.New("crypto: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
