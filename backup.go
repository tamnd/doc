package doc

import (
	"context"
	"errors"
	"io"
	"time"
)

// BackupOptions tunes an online physical backup (spec 2061 doc 18 §10.2).
type BackupOptions struct {
	// SinceVersion, if > 0, asks for an incremental backup carrying only the WAL
	// frames committed after that version. Incremental backup ships with WAL
	// archiving in M7-d; until then a non-zero value returns ErrUnsupported.
	SinceVersion uint64

	// Verify, if true, re-checks every page's checksum as it is streamed, catching a
	// read error or a bit flip during the copy at the cost of the extra hashing.
	Verify bool

	// Progress, if non-nil, is called after each page with the running byte count
	// and the total, so a caller can render a progress bar.
	Progress func(written, total int64)
}

// BackupResult reports what a backup copied (spec 2061 doc 18 §10.2).
type BackupResult struct {
	Version   uint64        // committed MVCC version at the time of the snapshot
	Pages     int64         // pages written to the backup
	WALFrames int64         // WAL frames folded into the image before the copy
	Bytes     int64         // total bytes written
	Duration  time.Duration // wall-clock time the backup took
}

// ErrUnsupported is returned for a backup option whose milestone has not landed yet,
// such as an incremental backup before WAL archiving is available.
var ErrUnsupported = errors.New("doc: operation not supported yet")

// Backup streams a consistent physical image of the database to w without closing it
// (spec 2061 doc 18 §10). The backup is taken as of a single MVCC version: it folds
// the committed WAL into the main file, then copies that image while the checkpointer
// and dirty-page stealing are frozen, so concurrent writers proceed against the WAL
// and never tear the bytes being copied. The result is a finished .doc file that
// opens with no WAL replay; verify it by opening it read-only and running Check.
func (db *DB) Backup(ctx context.Context, w io.Writer, opts BackupOptions) (BackupResult, error) {
	if err := db.check(ctx); err != nil {
		return BackupResult{}, err
	}
	if opts.SinceVersion > 0 {
		return BackupResult{}, ErrUnsupported
	}
	if w == nil {
		return BackupResult{}, errors.New("doc: backup writer is nil")
	}
	start := time.Now()
	framesBefore := int64(db.eng.PagerStats().WALSizePages)
	version := db.eng.CommitVersion()
	info, err := db.eng.Backup(w, opts.Verify, opts.Progress)
	if err != nil {
		return BackupResult{}, mapEngineErr(err)
	}
	res := BackupResult{
		Version:   version,
		Pages:     info.Pages,
		WALFrames: framesBefore,
		Bytes:     info.Bytes,
		Duration:  time.Since(start),
	}
	db.logger(logComponentBackup).Info("backup completed",
		"version", res.Version,
		"pages", res.Pages,
		"bytes", res.Bytes,
		"verify", opts.Verify)
	return res, nil
}
