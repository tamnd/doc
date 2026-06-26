package doc

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/tamnd/doc/wal"
)

// BackupOptions tunes an online physical backup (spec 2061 doc 18 §10.2).
type BackupOptions struct {
	// SinceVersion, if > 0, asks for an incremental backup: a delta carrying only the
	// commits archived after that version, drawn from ArchiveSource. The delta is one
	// WAL segment that doc restore --apply-delta replays over a base at SinceVersion.
	SinceVersion uint64

	// ArchiveSource is where an incremental backup reads archived segments from. It is
	// required when SinceVersion > 0 and ignored otherwise.
	ArchiveSource WALSink

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
	if w == nil {
		return BackupResult{}, errors.New("doc: backup writer is nil")
	}
	if opts.SinceVersion > 0 {
		return db.incrementalBackup(w, opts)
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

// incrementalBackup builds a delta segment from ArchiveSource holding every archived
// commit with version greater than SinceVersion, and writes it to w. The delta is the
// daily-incremental half of the weekly-full + daily-incremental + continuous-archive
// rotation (spec 2061 doc 18 §10.3, §14.3).
func (db *DB) incrementalBackup(w io.Writer, opts BackupOptions) (BackupResult, error) {
	if opts.ArchiveSource == nil {
		return BackupResult{}, errors.New("doc: incremental backup needs an ArchiveSource to read archived segments from")
	}
	start := time.Now()
	segs, err := segmentsInOrder(opts.ArchiveSource)
	if err != nil {
		return BackupResult{}, err
	}
	delta := &wal.Segment{BaseVersion: opts.SinceVersion}
	var frames int64
	for _, s := range segs {
		if delta.PageSize == 0 {
			delta.PageSize = s.PageSize
		}
		for _, c := range s.Commits {
			if c.Version <= opts.SinceVersion {
				continue
			}
			delta.Commits = append(delta.Commits, c)
			delta.EndVersion = c.Version
			delta.EndTimeUnix = c.TimeUnix
			frames += int64(len(c.Frames))
		}
	}
	if delta.PageSize == 0 {
		delta.PageSize = uint32(db.eng.PageSize())
	}
	data := delta.Encode()
	n, err := w.Write(data)
	if err != nil {
		return BackupResult{}, err
	}
	res := BackupResult{
		Version:   delta.EndVersion,
		WALFrames: frames,
		Bytes:     int64(n),
		Duration:  time.Since(start),
	}
	db.logger(logComponentBackup).Info("incremental backup completed",
		"sinceVersion", opts.SinceVersion,
		"endVersion", res.Version,
		"commits", len(delta.Commits),
		"frames", res.WALFrames,
		"bytes", res.Bytes)
	return res, nil
}
