package crypto

import (
	"crypto/rand"
	"io"

	"github.com/tamnd/doc/vfs"
)

// Key rotation comes in two costs (spec 2061 doc 17 §6). RotateKey changes the passphrase or
// key that protects the file without touching a single data page: it rederives the KEK and
// rewraps the existing DEK, an O(1) header write. Rekey rotates the DEK itself, which
// re-encrypts every page under fresh key material and a new epoch.
//
// Both operate on a closed file: open the path, rewrite, close. doc exposes them as
// db-level helpers that the caller runs while the database is not otherwise open.

// RotateKey changes the key that protects path from oldKey to newKey without re-encrypting
// data pages. The file is opened, its header is read and the DEK unwrapped under oldKey,
// then the DEK is rewrapped under a KEK derived from newKey with a fresh salt, and the new
// header is written and synced. Reading afterward requires newKey; oldKey no longer works.
func RotateKey(inner vfs.FS, path string, oldKey, newKey KeyOption) error {
	f, err := inner.Open(path, vfs.OpenRead)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h, dek, err := readHeaderAndDEK(f, oldKey)
	if err != nil {
		return err
	}
	defer zero(dek)

	// Fresh salt and KDF parameters for the new key, then rewrap the same DEK under the new
	// KEK. The epoch and file id are unchanged because the data pages are unchanged.
	if _, err := rand.Read(h.salt[:]); err != nil {
		return err
	}
	h.kdfID = newKey.kdfID()
	h.iterations = newKey.iterations()
	newKEK, err := newKey.deriveKEK(h.salt[:], h.iterations)
	if err != nil {
		return err
	}
	if err := h.sealKeys(newKEK, dek); err != nil {
		return err
	}
	if _, err := f.WriteAt(h.encode(), 0); err != nil {
		return err
	}
	return f.Sync(vfs.SyncFull)
}

// Rekey rotates the data encryption key: it allocates a fresh DEK for the next epoch,
// re-encrypts every page under it, and rewrites the header. The key that protects the file
// can change at the same time (pass a different newKey) or stay the same (pass the same
// option). This is the eager, offline rotation behind `doc rekey` (spec 2061 doc 17 §6.2).
func Rekey(inner vfs.FS, path string, oldKey, newKey KeyOption) error {
	f, err := inner.Open(path, vfs.OpenRead)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h, oldDEK, err := readHeaderAndDEK(f, oldKey)
	if err != nil {
		return err
	}
	defer zero(oldDEK)

	newDEKBytes, err := newDEK()
	if err != nil {
		return err
	}
	defer zero(newDEKBytes)
	newEpoch := h.epoch + 1

	pageSize := int(h.pageSize)
	slotSize := pageSize + EnvelopeOverhead
	size, err := f.Size()
	if err != nil {
		return err
	}
	pages := (size - SuperHeaderSize) / int64(slotSize)

	// Re-encrypt each page: decrypt under the old DEK and epoch, re-seal under the new DEK
	// and epoch, and write the slot back in place.
	env := make([]byte, slotSize)
	for page := int64(0); page < pages; page++ {
		off := SuperHeaderSize + page*int64(slotSize)
		if _, err := f.ReadAt(env, off); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		plain, err := openPage(oldDEK, uint32(page), h.epoch, h.fileID, env)
		if err != nil {
			return err
		}
		resealed, err := sealPage(newDEKBytes, uint32(page), newEpoch, h.fileID, plain)
		zero(plain)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(resealed, off); err != nil {
			return err
		}
	}

	// Wrap the new DEK under a KEK from newKey with a fresh salt, then write the updated
	// header carrying the new epoch.
	if _, err := rand.Read(h.salt[:]); err != nil {
		return err
	}
	h.epoch = newEpoch
	h.kdfID = newKey.kdfID()
	h.iterations = newKey.iterations()
	newKEK, err := newKey.deriveKEK(h.salt[:], h.iterations)
	if err != nil {
		return err
	}
	if err := h.sealKeys(newKEK, newDEKBytes); err != nil {
		return err
	}
	if _, err := f.WriteAt(h.encode(), 0); err != nil {
		return err
	}
	return f.Sync(vfs.SyncFull)
}

// readHeaderAndDEK reads the super-header from a raw (unwrapped) file and unwraps the DEK
// under key. It is the shared front half of both rotation operations. The returned file
// offsets are physical, so the caller works with raw slots.
func readHeaderAndDEK(f vfs.File, key KeyOption) (*header, []byte, error) {
	hb := make([]byte, SuperHeaderSize)
	if _, err := f.ReadAt(hb, 0); err != nil {
		return nil, nil, err
	}
	h, err := decodeHeader(hb)
	if err != nil {
		return nil, nil, err
	}
	dek, err := h.openKeys(key)
	if err != nil {
		return nil, nil, err
	}
	return h, dek, nil
}

// zero wipes a key buffer.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
