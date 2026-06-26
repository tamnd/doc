package doc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// readFile returns the raw bytes of a file, failing the test on error.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestOpenEncryptedRoundTrip creates an encrypted database, writes documents, closes it,
// reopens with the same passphrase, and reads them back. It also confirms a known plaintext
// marker does not appear in the raw file on disk.
func TestOpenEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "enc.doc")
	const marker = "supersecret-marker-value"

	db, err := Open(path, WithPassphrase("open sesame"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("vault").Collection("secrets")
	for i := 0; i < 50; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "note": marker}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The marker must not be readable in the raw .doc image.
	if bytes.Contains(readFile(t, path), []byte(marker)) {
		t.Fatal("plaintext marker leaked into the encrypted file")
	}

	db2, err := Open(path, WithPassphrase("open sesame"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	n, err := db2.Database("vault").Collection("secrets").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 50 {
		t.Fatalf("document count = %d, want 50", n)
	}
	if err := db2.Database("vault").Collection("secrets").FindOne(ctx, M{"_id": 7}).Err(); err != nil {
		t.Fatalf("lookup after reopen: %v", err)
	}
}

// TestOpenEncryptedWrongPassphrase confirms reopening with the wrong passphrase fails with
// ErrWrongKey instead of corrupt data.
func TestOpenEncryptedWrongPassphrase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wrong.doc")

	db, err := Open(path, WithPassphrase("correct"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Database("d").Collection("c").InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path, WithPassphrase("incorrect")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("reopen with wrong passphrase: err = %v, want ErrWrongKey", err)
	}
	// Opening with no key at all also fails: the cleartext header is not a valid format header.
	if _, err := Open(path); err == nil {
		t.Fatal("opening an encrypted file with no key should fail")
	}
}

// TestRotatePassphraseEndToEnd is the milestone exit criterion (spec 2061 doc 19 §22 M8):
// encrypt with key A, rotate to key B, verify the database is readable with key B and not
// with key A.
func TestRotatePassphraseEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rotate.doc")

	db, err := Open(path, WithPassphrase("key-A"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("app").Collection("data")
	for i := 0; i < 100; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "v": i * i}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := RotatePassphrase(path, PassphraseKey("key-A"), PassphraseKey("key-B")); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Readable with B, with the data intact.
	dbB, err := Open(path, WithPassphrase("key-B"))
	if err != nil {
		t.Fatalf("open with B: %v", err)
	}
	n, err := dbB.Database("app").Collection("data").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count with B: %v", err)
	}
	if n != 100 {
		t.Fatalf("document count after rotation = %d, want 100", n)
	}
	if err := dbB.Close(); err != nil {
		t.Fatalf("close B: %v", err)
	}

	// Not readable with A.
	if _, err := Open(path, WithPassphrase("key-A")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with A after rotation: err = %v, want ErrWrongKey", err)
	}
}

// TestRekeyEndToEnd rotates the data key by re-encrypting every page, then checks the data
// reads back under the new key and the old key no longer opens the file.
func TestRekeyEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rekey.doc")

	db, err := Open(path, WithPassphrase("data-A"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("app").Collection("data")
	for i := 0; i < 200; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := Rekey(path, PassphraseKey("data-A"), PassphraseKey("data-B")); err != nil {
		t.Fatalf("rekey: %v", err)
	}

	dbB, err := Open(path, WithPassphrase("data-B"))
	if err != nil {
		t.Fatalf("open after rekey: %v", err)
	}
	n, err := dbB.Database("app").Collection("data").CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count after rekey: %v", err)
	}
	if n != 200 {
		t.Fatalf("document count after rekey = %d, want 200", n)
	}
	if err := dbB.Close(); err != nil {
		t.Fatalf("close B: %v", err)
	}

	if _, err := Open(path, WithPassphrase("data-A")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with old key after rekey: err = %v, want ErrWrongKey", err)
	}
}

// TestOpenEncryptedRawKey exercises the raw-key path (WithEncryptionKey) end to end.
func TestOpenEncryptedRawKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "raw.doc")
	key := bytes.Repeat([]byte{0x2A}, 32)

	db, err := Open(path, WithEncryptionKey(key))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Database("d").Collection("c").InsertOne(ctx, M{"_id": "x"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path, WithEncryptionKey(key))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if err := db2.Database("d").Collection("c").FindOne(ctx, M{"_id": "x"}).Err(); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}
