package main

import (
	"fmt"
	"os"

	"github.com/tamnd/doc"
)

// subRekey rotates the key that protects an encrypted database (spec 2061 doc 17 §6). Two
// modes: the default rewraps the data key under a new passphrase or key without touching a
// data page, an O(1) header rewrite; --data rotates the data key itself, re-encrypting every
// page under fresh material and a new epoch.
//
// The current key comes from the same --passphrase or --key-file used to open the file, so by
// the time this runs the app has already opened the file and proven the old key. The file is
// closed before the rotation rewrites it.
//
//	doc <file> rekey --new-passphrase <p>
//	doc <file> rekey --new-key-file <path>
//	doc <file> rekey --data --new-passphrase <p>
func (a *app) subRekey() int {
	if a.memory {
		return reportTop(usageErr("rekey needs a file database, not in-memory"))
	}
	if !a.cfg.encrypted() {
		return reportTop(usageErr("rekey needs the current key: pass --passphrase or --key-file"))
	}

	fs := parseFlags(a.cfg.subArgs)
	newPass, hasPass := fs.values["new-passphrase"]
	newKeyFile := fs.values["new-key-file"]
	if hasPass && newKeyFile != "" {
		return reportTop(usageErr("set only one of --new-passphrase and --new-key-file"))
	}
	if !hasPass && newKeyFile == "" {
		return reportTop(usageErr("rekey needs --new-passphrase or --new-key-file"))
	}

	oldKey, err := a.cfg.encryptionKey()
	if err != nil {
		return reportTop(cliError{code: exitUsage, msg: err.Error()})
	}
	var newKey doc.EncryptionKey
	if newKeyFile != "" {
		raw, err := readRawKey(newKeyFile)
		if err != nil {
			return reportTop(cliError{code: exitUsage, msg: err.Error()})
		}
		newKey = doc.RawKey(raw)
	} else {
		newKey = doc.PassphraseKey(newPass)
	}

	// The rotation rewrites the file directly, so close the open handle first.
	path := a.cfg.file
	rotateData := fs.bools["data"]
	a.close()

	if rotateData {
		err = doc.Rekey(path, oldKey, newKey)
	} else {
		err = doc.RotatePassphrase(path, oldKey, newKey)
	}
	if err != nil {
		return reportTop(cliError{code: exitIOError, msg: err.Error()})
	}

	if rotateData {
		_, _ = fmt.Fprintf(os.Stderr, "rekeyed %s: data key rotated, all pages re-encrypted\n", displayPath(path))
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "rekeyed %s: protection key rotated\n", displayPath(path))
	}
	return exitOK
}
