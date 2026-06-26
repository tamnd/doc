package crypto

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// The key hierarchy is two tiers (spec 2061 doc 17 §4): a passphrase (or a raw key) derives
// a Key Encryption Key that lives only in memory; the KEK wraps a random Data Encryption
// Key that is stored in the file header in wrapped form; the DEK encrypts the pages. A
// passphrase change rewraps the DEK and leaves every page untouched (§6.1); a DEK rotation
// re-encrypts the pages (§6.2).

// ErrWrongKey is returned when the supplied passphrase or key does not unwrap the stored
// DEK, which is the clean signal for a wrong password instead of garbage page output (spec
// 2061 doc 17 §4.2).
var ErrWrongKey = errors.New("doc: wrong encryption key")

// KDF ids stored in the descriptor (spec 2061 doc 17 §4.2). Argon2id is unavailable without
// a third-party dependency, so a passphrase is run through PBKDF2-HMAC-SHA256 instead; the
// id records which path produced the KEK.
const (
	kdfPBKDF2 = 0x01 // passphrase stretched with PBKDF2-HMAC-SHA256 (Argon2id substitute)
	kdfRaw    = 0x02 // a raw 32-byte key supplied directly, no stretching
)

// cipherAES256GCM is the only cipher id doc writes (spec 2061 doc 17 §4.2). ChaCha20-Poly1305
// would need a third-party dependency.
const cipherAES256GCM = 0x01

// defaultPBKDF2Iter is the default PBKDF2 work factor. It targets a few hundred milliseconds
// on 2024-class hardware, the same goal the spec sets for its Argon2id defaults.
const defaultPBKDF2Iter = 600000

// KeyOption supplies the secret that protects a database. Exactly one of Passphrase or
// RawKey is set. A passphrase is stretched with PBKDF2; a raw key is used as the KEK
// directly, which is the path a KMS or key file takes.
type KeyOption struct {
	Passphrase []byte // stretched into the KEK with PBKDF2; nil when RawKey is used
	RawKey     []byte // a 32-byte KEK used directly; nil when Passphrase is used
	Iterations int    // PBKDF2 iterations; zero means defaultPBKDF2Iter
}

// Passphrase builds a KeyOption from a passphrase.
func Passphrase(s string) KeyOption { return KeyOption{Passphrase: []byte(s)} }

// RawKey builds a KeyOption from a raw 32-byte key.
func RawKey(key []byte) KeyOption { return KeyOption{RawKey: key} }

// Set reports whether a key was supplied.
func (k KeyOption) Set() bool { return len(k.Passphrase) > 0 || len(k.RawKey) > 0 }

// kdfID returns the descriptor KDF id for this key option.
func (k KeyOption) kdfID() byte {
	if len(k.RawKey) > 0 {
		return kdfRaw
	}
	return kdfPBKDF2
}

// deriveKEK turns the key option into a 32-byte KEK. A raw key must already be 32 bytes; a
// passphrase is stretched with PBKDF2-HMAC-SHA256 over the descriptor salt and iteration
// count.
func (k KeyOption) deriveKEK(salt []byte, iterations int) ([]byte, error) {
	if len(k.RawKey) > 0 {
		if len(k.RawKey) != keyLen {
			return nil, errors.New("crypto: raw key must be 32 bytes")
		}
		out := make([]byte, keyLen)
		copy(out, k.RawKey)
		return out, nil
	}
	if len(k.Passphrase) == 0 {
		return nil, errors.New("crypto: no passphrase or raw key supplied")
	}
	return pbkdf2.Key(sha256.New, string(k.Passphrase), salt, iterations, keyLen)
}

// iterations resolves the PBKDF2 work factor, applying the default.
func (k KeyOption) iterations() int {
	if k.Iterations > 0 {
		return k.Iterations
	}
	return defaultPBKDF2Iter
}

// newDEK generates a fresh random 32-byte data encryption key.
func newDEK() ([]byte, error) {
	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// wrapDEK encrypts the DEK under the KEK for storage in the header. The AAD binds the epoch
// so a wrapped DEK cannot be replayed under a different epoch (spec 2061 doc 17 §4.3). It
// returns the 12-byte wrapping nonce and the 48-byte wrapped blob (32 ciphertext + 16 tag).
func wrapDEK(kek, dek []byte, epoch uint32) (nonce []byte, wrapped []byte, err error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	wrapped = gcm.Seal(nil, nonce, dek, dekWrapAAD(epoch))
	return nonce, wrapped, nil
}

// unwrapDEK recovers the DEK from its wrapped form. A failure means the KEK is wrong, which
// is reported as ErrWrongKey (spec 2061 doc 17 §4.2, §4.3).
func unwrapDEK(kek, nonce, wrapped []byte, epoch uint32) ([]byte, error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	dek, err := gcm.Open(nil, nonce, wrapped, dekWrapAAD(epoch))
	if err != nil {
		return nil, ErrWrongKey
	}
	return dek, nil
}

// dekWrapAAD is the AAD for the DEK-wrap operation: a fixed label plus the epoch.
func dekWrapAAD(epoch uint32) []byte {
	aad := []byte("doc-dek-wrap-v1\x00\x00\x00\x00")
	aad[len(aad)-4] = byte(epoch)
	aad[len(aad)-3] = byte(epoch >> 8)
	aad[len(aad)-2] = byte(epoch >> 16)
	aad[len(aad)-1] = byte(epoch >> 24)
	return aad
}

// sealVerification produces the verification blob: AES-256-GCM of 32 zero bytes under the
// KEK with the verification nonce (spec 2061 doc 17 §4.2). Decrypting it after deriving the
// KEK proves the key is right before any data page is read.
func sealVerification(kek, nonce []byte) ([]byte, error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, make([]byte, keyLen), []byte("doc-verify-v1")), nil
}

// checkVerification decrypts the verification blob and confirms it is the expected zeros. A
// failure means the wrong key was supplied (ErrWrongKey).
func checkVerification(kek, nonce, blob []byte) error {
	gcm, err := newGCM(kek)
	if err != nil {
		return err
	}
	plain, err := gcm.Open(nil, nonce, blob, []byte("doc-verify-v1"))
	if err != nil {
		return ErrWrongKey
	}
	for _, b := range plain {
		if b != 0 {
			return ErrWrongKey
		}
	}
	return nil
}
