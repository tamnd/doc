package doc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/wal"
)

// Restore rebuilds a database from a base backup plus archived WAL segments, the
// path point-in-time recovery takes (spec 2061 doc 18 §14). The base supplies the
// pages as of the backup version; the segments carry every commit after it as full
// page images, which replay over the base by overwriting each page to its committed
// state. Replaying commits in version order up to a target yields the exact database
// state at that target.

// CurrentVersion returns the latest committed MVCC version, the value a point-in-time
// restore targets with --target-version (spec 2061 doc 18 §14.1).
func (db *DB) CurrentVersion() uint64 { return db.eng.CommitVersion() }

// RestoreOptions bounds how far a WAL replay goes.
type RestoreOptions struct {
	// TargetVersion stops the replay after the last commit whose version is at or
	// below it. Zero means replay every commit available.
	TargetVersion uint64
	// TargetTime stops the replay after the last commit whose cluster time is at or
	// below it. The zero time means no time bound.
	TargetTime time.Time
}

// RestoreResult reports what a replay applied.
type RestoreResult struct {
	Version        uint64 // version of the last commit applied
	AppliedCommits int
	AppliedFrames  int
	DBSizePages    uint32 // database size the file was truncated to
}

// RestoreBase copies a base backup image to outPath, the first step of a restore. It
// fails if outPath already exists, so a restore never silently overwrites a database.
// The copied file is validated by opening it.
func RestoreBase(basePath, outPath string) error {
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Errorf("doc: restore target %s already exists", outPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(basePath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// A copied base must reopen and check clean before we call it a restore base.
	db, err := Open(outPath, WithReadOnly(true))
	if err != nil {
		return fmt.Errorf("doc: restored base does not open: %w", err)
	}
	return db.Close()
}

// ApplyWAL replays archived segments from sink onto the database file at dbPath,
// stopping at the recovery target, then truncates and syncs. The file must already
// hold a base image (from RestoreBase). The result is a complete, self-contained
// database at the target with no WAL to replay.
func ApplyWAL(dbPath string, sink WALSink, opts RestoreOptions) (RestoreResult, error) {
	segs, err := segmentsInOrder(sink)
	if err != nil {
		return RestoreResult{}, err
	}
	commits := make([]wal.Commit, 0, 64)
	for _, s := range segs {
		commits = append(commits, s.Commits...)
	}
	return applyCommits(dbPath, commits, opts)
}

// ApplyDelta replays a single delta segment file onto the database at dbPath. It is
// the incremental sibling of ApplyWAL: a delta is one segment produced by an
// incremental backup (spec 2061 doc 18 §10.3).
func ApplyDelta(dbPath, deltaPath string, opts RestoreOptions) (RestoreResult, error) {
	data, err := os.ReadFile(deltaPath)
	if err != nil {
		return RestoreResult{}, err
	}
	seg, err := wal.DecodeSegment(data)
	if err != nil {
		return RestoreResult{}, err
	}
	return applyCommits(dbPath, seg.Commits, opts)
}

// applyCommits writes the page images of each commit to the database file in order,
// stopping at the target, then truncates to the last applied commit's size. It works
// straight on the file the same way crash recovery replays the WAL: full page images
// written at their page offset, idempotent and order-independent within a commit.
func applyCommits(dbPath string, commits []wal.Commit, opts RestoreOptions) (RestoreResult, error) {
	f, err := os.OpenFile(dbPath, os.O_RDWR, 0o644)
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = f.Close() }()

	hdrBuf := make([]byte, format.HeaderSize)
	if _, err := f.ReadAt(hdrBuf, 0); err != nil {
		return RestoreResult{}, fmt.Errorf("read header: %w", err)
	}
	hdr, err := format.DecodeHeader(hdrBuf)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("decode header: %w", err)
	}
	pageSize := int64(hdr.PageSize)

	var targetUnix int64 = -1
	if !opts.TargetTime.IsZero() {
		targetUnix = opts.TargetTime.Unix()
	}

	var res RestoreResult
	res.DBSizePages = hdr.PageCount
	var effectiveVer uint64
	for _, c := range commits {
		v := c.Version
		if v == 0 {
			v = effectiveVer // an unannotated commit carries the running version forward
		}
		if opts.TargetVersion > 0 && v > opts.TargetVersion {
			break
		}
		if targetUnix >= 0 && c.TimeUnix > targetUnix {
			break
		}
		for _, fr := range c.Frames {
			if _, err := f.WriteAt(fr.Payload, int64(fr.PageID)*pageSize); err != nil {
				return res, fmt.Errorf("apply page %d: %w", fr.PageID, err)
			}
			res.AppliedFrames++
		}
		effectiveVer = v
		res.Version = v
		res.DBSizePages = c.DBSizePages
		res.AppliedCommits++
	}

	if err := f.Truncate(int64(res.DBSizePages) * pageSize); err != nil {
		return res, fmt.Errorf("truncate: %w", err)
	}
	if err := f.Sync(); err != nil {
		return res, fmt.Errorf("sync: %w", err)
	}
	return res, nil
}
