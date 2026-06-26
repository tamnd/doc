package crypto

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tamnd/doc/vfs"
)

// TestEnvelopeRoundTrip seals and opens a page and checks the plaintext survives.
func TestEnvelopeRoundTrip(t *testing.T) {
	dek := make([]byte, keyLen)
	for i := range dek {
		dek[i] = byte(i)
	}
	var fileID [16]byte
	copy(fileID[:], "0123456789abcdef")
	plain := bytes.Repeat([]byte("page data "), 100)

	env, err := sealPage(dek, 7, 1, fileID, plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(env) != len(plain)+EnvelopeOverhead {
		t.Fatalf("envelope len = %d, want %d", len(env), len(plain)+EnvelopeOverhead)
	}
	got, err := openPage(dek, 7, 1, fileID, env)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("round-trip plaintext mismatch")
	}
}

// TestEnvelopeTamperDetected flips a ciphertext byte and expects an integrity violation.
func TestEnvelopeTamperDetected(t *testing.T) {
	dek := make([]byte, keyLen)
	var fileID [16]byte
	env, _ := sealPage(dek, 1, 1, fileID, []byte("secret page contents here"))
	env[nonceLen+3] ^= 0x01

	if _, err := openPage(dek, 1, 1, fileID, env); !errors.Is(err, ErrIntegrityViolation) {
		t.Fatalf("tampered open err = %v, want ErrIntegrityViolation", err)
	}
}

// TestEnvelopeAADBinding confirms a page decrypted under a different page number, epoch, or
// file id fails authentication, which is the cut-and-paste protection.
func TestEnvelopeAADBinding(t *testing.T) {
	dek := make([]byte, keyLen)
	var fileID [16]byte
	copy(fileID[:], "fileFILEfileFILE")
	env, _ := sealPage(dek, 5, 2, fileID, []byte("bound to its coordinates"))

	var otherID [16]byte
	cases := []struct {
		name        string
		page, epoch uint32
		id          [16]byte
	}{
		{"wrong page", 6, 2, fileID},
		{"wrong epoch", 5, 3, fileID},
		{"wrong file", 5, 2, otherID},
	}
	for _, tc := range cases {
		if _, err := openPage(dek, tc.page, tc.epoch, tc.id, env); !errors.Is(err, ErrIntegrityViolation) {
			t.Fatalf("%s: err = %v, want ErrIntegrityViolation", tc.name, err)
		}
	}
}

// TestKeyWrapRoundTrip wraps and unwraps a DEK and checks a wrong KEK is rejected.
func TestKeyWrapRoundTrip(t *testing.T) {
	kek, _ := Passphrase("correct horse").deriveKEK(make([]byte, 32), 1000)
	dek, _ := newDEK()
	nonce, wrapped, err := wrapDEK(kek, dek, 1)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := unwrapDEK(kek, nonce, wrapped, 1)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unwrapped DEK mismatch")
	}
	wrongKEK, _ := Passphrase("battery staple").deriveKEK(make([]byte, 32), 1000)
	if _, err := unwrapDEK(wrongKEK, nonce, wrapped, 1); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong KEK err = %v, want ErrWrongKey", err)
	}
}

// TestHeaderRoundTrip builds a header, encodes and decodes it, and unwraps the DEK with the
// right and wrong passphrase.
func TestHeaderRoundTrip(t *testing.T) {
	key := KeyOption{Passphrase: []byte("open sesame"), Iterations: 2000}
	h, dek, err := newHeader(8192, key)
	if err != nil {
		t.Fatalf("newHeader: %v", err)
	}
	dec, err := decodeHeader(h.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.pageSize != 8192 || dec.epoch != 1 {
		t.Fatalf("decoded pageSize=%d epoch=%d", dec.pageSize, dec.epoch)
	}
	got, err := dec.openKeys(key)
	if err != nil {
		t.Fatalf("openKeys: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("header DEK mismatch")
	}
	if _, err := dec.openKeys(KeyOption{Passphrase: []byte("wrong"), Iterations: 2000}); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong passphrase err = %v, want ErrWrongKey", err)
	}
}

// TestEncryptingFileRoundTrip writes pages through the encrypting file, reopens it, and
// reads them back, then confirms the raw bytes on disk are not the plaintext.
func TestEncryptingFileRoundTrip(t *testing.T) {
	const pageSize = 4096
	mem := vfs.NewMemFS()
	key := KeyOption{Passphrase: []byte("hunter2"), Iterations: 1000}
	fs := NewFS(mem, "main.doc", key, pageSize)

	page0 := bytes.Repeat([]byte{0xAB}, pageSize)
	page1 := bytes.Repeat([]byte("plaintext-marker"), pageSize/16)

	f, err := fs.Open("main.doc", vfs.OpenCreate)
	if err != nil {
		t.Fatalf("open create: %v", err)
	}
	if _, err := f.WriteAt(page0, 0); err != nil {
		t.Fatalf("write page0: %v", err)
	}
	if _, err := f.WriteAt(page1, pageSize); err != nil {
		t.Fatalf("write page1: %v", err)
	}
	_ = f.Sync(vfs.SyncFull)
	if sz, _ := f.Size(); sz != 2*pageSize {
		t.Fatalf("logical size = %d, want %d", sz, 2*pageSize)
	}
	_ = f.Close()

	// The raw underlying file must not contain the plaintext marker anywhere.
	raw, _ := mem.Open("main.doc", vfs.OpenReadOnly)
	rsize, _ := raw.Size()
	rbuf := make([]byte, rsize)
	_, _ = raw.ReadAt(rbuf, 0)
	_ = raw.Close()
	if bytes.Contains(rbuf, []byte("plaintext-marker")) {
		t.Fatal("plaintext leaked into the encrypted file")
	}

	// Reopen and read both pages back.
	g, err := fs.Open("main.doc", vfs.OpenRead)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = g.Close() }()
	got0 := make([]byte, pageSize)
	if _, err := g.ReadAt(got0, 0); err != nil {
		t.Fatalf("read page0: %v", err)
	}
	got1 := make([]byte, pageSize)
	if _, err := g.ReadAt(got1, pageSize); err != nil {
		t.Fatalf("read page1: %v", err)
	}
	if !bytes.Equal(got0, page0) || !bytes.Equal(got1, page1) {
		t.Fatal("page round-trip mismatch")
	}

	// A sub-page read at offset 0 returns the page prefix, the pattern the pager uses to
	// read the format header.
	prefix := make([]byte, 100)
	if _, err := g.ReadAt(prefix, 0); err != nil {
		t.Fatalf("prefix read: %v", err)
	}
	if !bytes.Equal(prefix, page0[:100]) {
		t.Fatal("prefix read mismatch")
	}
}

// TestEncryptingFileWrongKey confirms reopening with the wrong passphrase fails with
// ErrWrongKey rather than returning garbage.
func TestEncryptingFileWrongKey(t *testing.T) {
	const pageSize = 4096
	mem := vfs.NewMemFS()
	good := KeyOption{Passphrase: []byte("right"), Iterations: 1000}
	f, err := NewFS(mem, "x.doc", good, pageSize).Open("x.doc", vfs.OpenCreate)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = f.WriteAt(bytes.Repeat([]byte{1}, pageSize), 0)
	_ = f.Close()

	bad := KeyOption{Passphrase: []byte("wrong"), Iterations: 1000}
	if _, err := NewFS(mem, "x.doc", bad, pageSize).Open("x.doc", vfs.OpenRead); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key open err = %v, want ErrWrongKey", err)
	}
}

// TestRotateKey rotates the passphrase and checks the new one opens the file while the old
// one no longer does, with the page data intact. This is the key-rotation exit criterion
// (spec 2061 doc 17 §6.1, doc 19 §22): encrypt with A, rotate to B, readable with B not A.
func TestRotateKey(t *testing.T) {
	const pageSize = 4096
	mem := vfs.NewMemFS()
	keyA := KeyOption{Passphrase: []byte("passphrase-A"), Iterations: 1000}
	keyB := KeyOption{Passphrase: []byte("passphrase-B"), Iterations: 1000}
	page := bytes.Repeat([]byte("rotate-me"), pageSize)[:pageSize]

	f, _ := NewFS(mem, "r.doc", keyA, pageSize).Open("r.doc", vfs.OpenCreate)
	if _, err := f.WriteAt(page, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	if err := RotateKey(mem, "r.doc", keyA, keyB); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// B reads the same data.
	g, err := NewFS(mem, "r.doc", keyB, pageSize).Open("r.doc", vfs.OpenRead)
	if err != nil {
		t.Fatalf("open with B: %v", err)
	}
	got := make([]byte, pageSize)
	if _, err := g.ReadAt(got, 0); err != nil {
		t.Fatalf("read with B: %v", err)
	}
	_ = g.Close()
	if !bytes.Equal(got, page) {
		t.Fatal("data not preserved across rotation")
	}

	// A no longer opens the file.
	if _, err := NewFS(mem, "r.doc", keyA, pageSize).Open("r.doc", vfs.OpenRead); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("old key err = %v, want ErrWrongKey", err)
	}
}

// TestRekey rotates the DEK by re-encrypting every page, then checks the data reads back and
// the epoch advanced so the old ciphertext no longer authenticates.
func TestRekey(t *testing.T) {
	const pageSize = 4096
	mem := vfs.NewMemFS()
	keyA := KeyOption{Passphrase: []byte("data-key-A"), Iterations: 1000}
	keyB := KeyOption{Passphrase: []byte("data-key-B"), Iterations: 1000}
	pages := [][]byte{
		bytes.Repeat([]byte{0x11}, pageSize),
		bytes.Repeat([]byte{0x22}, pageSize),
		bytes.Repeat([]byte{0x33}, pageSize),
	}

	f, _ := NewFS(mem, "k.doc", keyA, pageSize).Open("k.doc", vfs.OpenCreate)
	for i, p := range pages {
		_, _ = f.WriteAt(p, int64(i)*pageSize)
	}
	_ = f.Close()

	if err := Rekey(mem, "k.doc", keyA, keyB); err != nil {
		t.Fatalf("rekey: %v", err)
	}

	g, err := NewFS(mem, "k.doc", keyB, pageSize).Open("k.doc", vfs.OpenRead)
	if err != nil {
		t.Fatalf("open after rekey: %v", err)
	}
	defer func() { _ = g.Close() }()
	for i, want := range pages {
		got := make([]byte, pageSize)
		if _, err := g.ReadAt(got, int64(i)*pageSize); err != nil {
			t.Fatalf("read page %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("page %d mismatch after rekey", i)
		}
	}
	if _, err := NewFS(mem, "k.doc", keyA, pageSize).Open("k.doc", vfs.OpenRead); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("old key after rekey err = %v, want ErrWrongKey", err)
	}
}
