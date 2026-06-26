package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/tamnd/doc"
)

// dotBackup streams a consistent physical backup of the open database to a file
// (spec 2061 doc 18 §10). The destination comes from --out or the first positional
// argument; "-" writes the raw image to stdout. --verify re-checks each page's
// checksum as it is copied. The database stays open and writable throughout.
//
// With --since-version <n> --archive <dir> it writes an incremental delta instead: a
// single segment carrying only the commits archived after version n, replayable over a
// base at that version with restore --apply-delta (spec 2061 doc 18 §10.3).
func (a *app) dotBackup(args []string) error {
	fs := parseFlags(args)
	out := fs.values["out"]
	if out == "" && len(fs.positional) > 0 {
		out = fs.positional[0]
	}
	if out == "" {
		return usageErr(".backup --out <file> [--verify] [--since-version <n> --archive <dir>]")
	}

	opts := doc.BackupOptions{Verify: fs.bools["verify"]}
	if v := fs.values["since-version"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return usageErr(".backup --since-version wants a number")
		}
		archive := fs.values["archive"]
		if archive == "" {
			return usageErr(".backup --since-version needs --archive <dir>")
		}
		sink, err := doc.NewDirSink(archive)
		if err != nil {
			return cliError{code: exitIOError, msg: err.Error()}
		}
		opts.SinceVersion = n
		opts.ArchiveSource = sink
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

	res, err := a.db.Backup(a.ctx(), w, opts)
	if err != nil {
		return classify(err)
	}
	if out == "-" {
		return nil // a raw image on stdout; no trailing text to corrupt it
	}
	if opts.SinceVersion > 0 {
		return a.rend.writeText(fmt.Sprintf("ok: wrote incremental delta (%d frames) up to version %d to %s",
			res.WALFrames, res.Version, out))
	}
	return a.rend.writeText(fmt.Sprintf("ok: backed up %d pages (%d bytes) at version %d to %s",
		res.Pages, res.Bytes, res.Version, out))
}
