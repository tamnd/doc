package doc

import (
	"github.com/tamnd/doc/crypto"
	"github.com/tamnd/doc/vfs"
)

// Encryption at rest (spec 2061 doc 17). A database opened with WithPassphrase or
// WithEncryptionKey stores every page as an AES-256-GCM envelope; the key never touches the
// disk in cleartext. The errors and rotation helpers below are the public surface around that
// machinery.

// ErrWrongKey is returned by Open when the supplied passphrase or key does not match an
// encrypted file. It surfaces before any page is read, so a wrong password is a clean error
// rather than garbage output.
var ErrWrongKey = crypto.ErrWrongKey

// ErrIntegrityViolation is returned when an encrypted page fails its authentication tag,
// meaning the ciphertext was altered or corrupted. The read fails closed rather than
// returning data that cannot be trusted.
var ErrIntegrityViolation = crypto.ErrIntegrityViolation

// EncryptionKey is the secret that protects an encrypted database, either a passphrase or a
// raw 32-byte key. Build one with PassphraseKey or RawKey for the rotation helpers.
type EncryptionKey = crypto.KeyOption

// PassphraseKey builds an EncryptionKey from a passphrase.
func PassphraseKey(passphrase string) EncryptionKey { return crypto.Passphrase(passphrase) }

// RawKey builds an EncryptionKey from a raw 32-byte key.
func RawKey(key []byte) EncryptionKey { return crypto.RawKey(key) }

// RotatePassphrase changes the key that protects the database at path from oldKey to newKey
// without re-encrypting any data page (spec 2061 doc 17 §6.1). It rederives the key-encryption
// key and rewraps the stored data-encryption key, an O(1) header rewrite. The database must be
// closed while this runs. Afterward the file opens with newKey and no longer with oldKey.
func RotatePassphrase(path string, oldKey, newKey EncryptionKey) error {
	return crypto.RotateKey(vfs.NewOSFS(), path, oldKey, newKey)
}

// Rekey rotates the data-encryption key of the database at path, re-encrypting every page
// under fresh key material and a new epoch (spec 2061 doc 17 §6.2). It is the eager, offline
// rotation behind `doc rekey`: more expensive than RotatePassphrase because it rewrites all
// pages, and the right tool when the data key itself must change. The database must be closed
// while this runs. The key that protects the file can change at the same time by passing a
// different newKey, or stay the same by passing the same value.
func Rekey(path string, oldKey, newKey EncryptionKey) error {
	return crypto.Rekey(vfs.NewOSFS(), path, oldKey, newKey)
}
