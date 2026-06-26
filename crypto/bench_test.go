package crypto

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/vfs"
)

// BenchmarkSealPage measures per-page AEAD sealing throughput.
func BenchmarkSealPage(b *testing.B) {
	dek := make([]byte, keyLen)
	var fileID [16]byte
	plain := bytes.Repeat([]byte{0x5A}, 16384)
	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sealPage(dek, uint32(i), 1, fileID, plain); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenPage measures per-page AEAD opening throughput.
func BenchmarkOpenPage(b *testing.B) {
	dek := make([]byte, keyLen)
	var fileID [16]byte
	plain := bytes.Repeat([]byte{0x5A}, 16384)
	env, err := sealPage(dek, 0, 1, fileID, plain)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := openPage(dek, 0, 1, fileID, env); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncryptedWriteAt measures a page write through the encrypting VFS, the full path
// the pager takes: seal plus underlying write.
func BenchmarkEncryptedWriteAt(b *testing.B) {
	const pageSize = 16384
	mem := vfs.NewMemFS()
	key := KeyOption{RawKey: make([]byte, keyLen)}
	f, err := NewFS(mem, "b.doc", key, pageSize).Open("b.doc", vfs.OpenCreate)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	page := bytes.Repeat([]byte{0x33}, pageSize)
	b.SetBytes(pageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(page, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlainWriteAt is the cleartext baseline for the same page write, so the encryption
// overhead is visible as the ratio between the two.
func BenchmarkPlainWriteAt(b *testing.B) {
	const pageSize = 16384
	mem := vfs.NewMemFS()
	f, err := mem.Open("p.doc", vfs.OpenCreate)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	page := bytes.Repeat([]byte{0x33}, pageSize)
	b.SetBytes(pageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteAt(page, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeriveKEK measures the PBKDF2 stretch at the default work factor, the one-time
// cost paid when a database is opened with a passphrase.
func BenchmarkDeriveKEK(b *testing.B) {
	key := Passphrase("benchmark passphrase")
	salt := make([]byte, 32)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := key.deriveKEK(salt, defaultPBKDF2Iter); err != nil {
			b.Fatal(err)
		}
	}
}
