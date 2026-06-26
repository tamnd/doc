package main

import (
	"strings"
	"testing"
)

// TestRunEncryptedOpenPersists writes to an encrypted file with --passphrase, then reopens it
// with the same passphrase and reads the document back.
func TestRunEncryptedOpenPersists(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, "--passphrase", "open-me", f,
		"-e", `db.c.insertOne({"_id":"keep","v":7})`); code != exitOK {
		t.Fatalf("insert exit = %d, stderr = %s", code, stderr)
	}
	out, stderr, code := runCLI(t, "--passphrase", "open-me", f, "-e", `db.c.find({"_id":"keep"})`)
	if code != exitOK {
		t.Fatalf("find exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"v":7`) {
		t.Fatalf("persisted document not found:\n%s", out)
	}
}

// TestRunEncryptedWrongPassphrase confirms the CLI reports an open failure with the wrong
// passphrase rather than returning data.
func TestRunEncryptedWrongPassphrase(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, "--passphrase", "right", f,
		"-e", `db.c.insertOne({"_id":1})`); code != exitOK {
		t.Fatalf("insert exit = %d, stderr = %s", code, stderr)
	}
	_, stderr, code := runCLI(t, "--passphrase", "wrong", f, "-e", `db.c.find({})`)
	if code == exitOK {
		t.Fatal("opening with the wrong passphrase should not succeed")
	}
	if !strings.Contains(stderr, "cannot open") {
		t.Fatalf("stderr = %q, want an open failure", stderr)
	}
}

// TestRunRekeyRotatesPassphrase rotates the passphrase through the CLI, then confirms the new
// passphrase opens the file and the old one does not.
func TestRunRekeyRotatesPassphrase(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, "--passphrase", "old-pass", f,
		"-e", `db.c.insertOne({"_id":"x","v":42})`); code != exitOK {
		t.Fatalf("insert exit = %d, stderr = %s", code, stderr)
	}

	if _, stderr, code := runCLI(t, "--passphrase", "old-pass", f,
		"rekey", "--new-passphrase", "new-pass"); code != exitOK {
		t.Fatalf("rekey exit = %d, stderr = %s", code, stderr)
	}

	out, stderr, code := runCLI(t, "--passphrase", "new-pass", f, "-e", `db.c.find({"_id":"x"})`)
	if code != exitOK {
		t.Fatalf("open with new passphrase exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"v":42`) {
		t.Fatalf("document missing after rekey:\n%s", out)
	}

	if _, _, code := runCLI(t, "--passphrase", "old-pass", f, "-e", `db.c.find({})`); code == exitOK {
		t.Fatal("old passphrase should not open the file after rekey")
	}
}

// TestRunRekeyDataReencryptsPages rotates the data key with --data, which re-encrypts every
// page, and checks the data survives under the new passphrase.
func TestRunRekeyDataReencryptsPages(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, "--passphrase", "data-old", f,
		"-e", `db.c.insertMany([{"_id":1},{"_id":2},{"_id":3}])`); code != exitOK {
		t.Fatalf("insert exit = %d, stderr = %s", code, stderr)
	}

	if _, stderr, code := runCLI(t, "--passphrase", "data-old", f,
		"rekey", "--data", "--new-passphrase", "data-new"); code != exitOK {
		t.Fatalf("rekey --data exit = %d, stderr = %s", code, stderr)
	}

	out, stderr, code := runCLI(t, "--passphrase", "data-new", f, "-e", `db.c.countDocuments({})`)
	if code != exitOK {
		t.Fatalf("open after data rekey exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "3") {
		t.Fatalf("count after data rekey wrong:\n%s", out)
	}
}

// TestRunRekeyNeedsCurrentKey confirms rekey refuses to run without the current key.
func TestRunRekeyNeedsCurrentKey(t *testing.T) {
	f := tmpDoc(t)
	if _, _, code := runCLI(t, f, "-e", `db.c.insertOne({"_id":1})`); code != exitOK {
		t.Fatal("seed insert failed")
	}
	_, stderr, code := runCLI(t, f, "rekey", "--new-passphrase", "whatever")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "current key") {
		t.Fatalf("stderr = %q, want it to mention the current key", stderr)
	}
}
