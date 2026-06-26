package main

import (
	"fmt"
	"os"

	"github.com/tamnd/doc"
)

// dotBackup streams a consistent physical backup of the open database to a file
// (spec 2061 doc 18 §10). The destination comes from --out or the first positional
// argument; "-" writes the raw image to stdout. --verify re-checks each page's
// checksum as it is copied. The database stays open and writable throughout.
func (a *app) dotBackup(args []string) error {
	fs := parseFlags(args)
	out := fs.values["out"]
	if out == "" && len(fs.positional) > 0 {
		out = fs.positional[0]
	}
	if out == "" {
		return usageErr(".backup --out <file> [--verify]")
	}

	w := os.Stdout
	if out != "-" {
		f, err := os.Create(out)
		if err != nil {
			return cliError{code: exitIOError, msg: err.Error()}
		}
		defer func() { _ = f.Close() }()
		w = f
	}

	res, err := a.db.Backup(a.ctx(), w, doc.BackupOptions{Verify: fs.bools["verify"]})
	if err != nil {
		return classify(err)
	}
	if out == "-" {
		return nil // a raw image on stdout; no trailing text to corrupt it
	}
	return a.rend.writeText(fmt.Sprintf("ok: backed up %d pages (%d bytes) at version %d to %s",
		res.Pages, res.Bytes, res.Version, out))
}
