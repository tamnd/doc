package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/tamnd/doc"
)

// dotRestore rebuilds a database from a base backup plus archived WAL segments or an
// incremental delta (spec 2061 doc 18 §14). It works straight on the file paths its
// flags name, not the database the CLI opened, so it closes that handle first to keep
// a single writer on any file it touches.
//
// Modes, which compose:
//
//	restore --base <img> --out <file>                 copy a base image to a fresh file
//	restore --base <img> --out <file> --wal-source <dir>   then replay archived WAL
//	restore --base <img> --out <file> --apply-delta <seg>  then replay one delta
//	restore --db <file> --wal-source <dir>            replay WAL onto an existing file
//	restore --db <file> --apply-delta <seg>           replay a delta onto an existing file
//
// A recovery target bounds how far the replay goes:
//
//	--target-version <n>   stop after the last commit at or below version n
//	--target-time <t>      stop after the last commit at or below time t (unix seconds or RFC3339)
func (a *app) dotRestore(args []string) error {
	fs := parseFlags(args)

	// Drop the database handle the CLI opened on startup; restore owns the files now.
	a.releaseDB()

	opts, err := restoreTarget(fs)
	if err != nil {
		return err
	}

	base := fs.values["base"]
	out := fs.values["out"]
	db := fs.values["db"]
	walSource := fs.values["wal-source"]
	delta := fs.values["apply-delta"]

	// Decide which file the replay lands on, building it from a base first if asked.
	target := db
	if base != "" {
		if out == "" {
			return usageErr("restore --base <img> needs --out <file>")
		}
		if err := doc.RestoreBase(base, out); err != nil {
			return classify(err)
		}
		if err := a.rend.writeText("ok: restored base " + base + " to " + out); err != nil {
			return err
		}
		target = out
	}

	if walSource == "" && delta == "" {
		if base == "" {
			return usageErr("restore needs --base/--out, --wal-source, or --apply-delta")
		}
		return nil // a base-only restore is complete
	}
	if target == "" {
		return usageErr("restore --wal-source/--apply-delta needs --db <file> (or --base/--out)")
	}

	var res doc.RestoreResult
	switch {
	case walSource != "":
		sink, serr := doc.NewDirSink(walSource)
		if serr != nil {
			return cliError{code: exitIOError, msg: serr.Error()}
		}
		res, err = doc.ApplyWAL(target, sink, opts)
	case delta != "":
		res, err = doc.ApplyDelta(target, delta, opts)
	}
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(fmt.Sprintf("ok: replayed %d commits (%d frames) to version %d, %d pages",
		res.AppliedCommits, res.AppliedFrames, res.Version, res.DBSizePages))
}

// restoreTarget reads --target-version and --target-time into RestoreOptions. A time
// is either unix seconds or an RFC3339 timestamp.
func restoreTarget(fs flagSet) (doc.RestoreOptions, error) {
	var opts doc.RestoreOptions
	if v := fs.values["target-version"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return opts, usageErr("restore --target-version wants a number")
		}
		opts.TargetVersion = n
	}
	if v := fs.values["target-time"]; v != "" {
		t, err := parseTargetTime(v)
		if err != nil {
			return opts, usageErr("restore --target-time wants unix seconds or RFC3339")
		}
		opts.TargetTime = t
	}
	return opts, nil
}

// parseTargetTime accepts a unix-seconds integer or an RFC3339 timestamp.
func parseTargetTime(v string) (time.Time, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.Parse(time.RFC3339, v)
}

// releaseDB closes the database the CLI opened on startup, if any, so a file operation
// like restore is the only writer. The deferred app.close handles a nil db.
func (a *app) releaseDB() {
	if a.cursor != nil {
		_ = a.cursor.Close(a.ctx())
		a.cursor = nil
	}
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
}
