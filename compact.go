package doc

import (
	"context"
	"fmt"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/engine"
	"github.com/tamnd/doc/vfs"
)

// Compact rewrites the database into a fresh, hole-free file and swaps it in
// place, reclaiming the space held by dead slots, superseded cells, forwarding
// tombstones, and the freelist (spec 2061 doc 18 §15.2). It is an offline,
// exclusive operation: it takes the write lock for its whole duration, so no other
// operation runs against the database while it proceeds, and the caller must not
// hold any cursor or session open across the call.
//
// The work is a logical dump and reload: every live document and index is read
// out, written into a sibling temp file through the bulk-load path, and the temp
// file's bytes replace the original. A document's _id and the collection's options,
// validators, and secondary indexes are preserved exactly; only the physical
// layout changes. After Compact, db.Check reports a file with no fragmentation.
func (db *DB) Compact(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.cfg.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Pause the TTL sweeper: it reads db.eng without the lock, and Compact swaps
	// db.eng underneath it.
	sweeping := db.ttlStop != nil
	if sweeping {
		close(db.ttlStop)
		<-db.ttlDone
		db.ttlStop = nil
	}

	if err := db.compactLocked(); err != nil {
		// The original engine is still open and untouched on any pre-swap failure;
		// just restart the sweeper and surface the error.
		if sweeping {
			db.startTTLSweeper(db.cfg.ttlInterval)
		}
		return err
	}

	if sweeping {
		db.startTTLSweeper(db.cfg.ttlInterval)
	}
	return nil
}

// compactLocked performs the dump, rebuild, swap, and reopen. The caller holds
// db.mu and has paused the sweeper.
func (db *DB) compactLocked() error {
	snap, err := db.eng.Export()
	if err != nil {
		return fmt.Errorf("compact: reading the database failed: %w", err)
	}

	tmp := db.path + ".compact"
	if err := db.removeFile(tmp); err != nil {
		return fmt.Errorf("compact: clearing a stale temp file failed: %w", err)
	}

	if err := db.buildTempFile(tmp, snap); err != nil {
		_ = db.removeFile(tmp)
		return err
	}

	// Close the live engine, overwrite the original file with the compact bytes,
	// drop any stale WAL so reopen reads the new image, and reopen.
	if err := db.eng.Close(); err != nil {
		_ = db.removeFile(tmp)
		return fmt.Errorf("compact: closing the database failed: %w", err)
	}
	if err := copyFileBytes(db.fs, tmp, db.path); err != nil {
		return fmt.Errorf("compact: installing the compact file failed: %w", err)
	}
	_ = db.removeFile(db.path + "-wal")
	_ = db.removeFile(tmp)

	eng, err := engine.Open(db.fs, db.path, db.cfg.engineOptions(db.clock))
	if err != nil {
		return fmt.Errorf("compact: reopening the database failed: %w", mapEngineErr(err))
	}
	db.eng = eng
	db.rewireFeed()
	return nil
}

// buildTempFile creates a fresh engine on path, loads the snapshot into it, and
// closes it so its file is a clean, checkpointed image.
func (db *DB) buildTempFile(path string, snap *engine.Snapshot) error {
	eng, err := engine.Open(db.fs, path, db.cfg.engineOptions(db.clock))
	if err != nil {
		return fmt.Errorf("compact: creating the temp file failed: %w", mapEngineErr(err))
	}
	if err := eng.Import(snap); err != nil {
		_ = eng.Close()
		return fmt.Errorf("compact: writing the compact file failed: %w", err)
	}
	if err := eng.Close(); err != nil {
		return fmt.Errorf("compact: finishing the compact file failed: %w", err)
	}
	return nil
}

// rewireFeed points the freshly opened engine's change hook back at the database's
// existing change feed, so a watcher that was open before Compact keeps receiving
// events afterward.
func (db *DB) rewireFeed() {
	feed := db.feed
	db.eng.SetChangeHook(func(dbName, coll string, recs []collection.ChangeRecord, cv uint64) {
		feed.publish(dbName, coll, recs, cv)
	})
}

// removeFile deletes path if it exists, tolerating a missing file.
func (db *DB) removeFile(path string) error {
	exists, err := db.fs.Exists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return db.fs.Delete(path, true)
}

// copyFileBytes replaces dst's contents with src's, truncating dst to the source
// length and syncing the result. Both paths live on the same filesystem.
func copyFileBytes(fs vfs.FS, src, dst string) error {
	in, err := fs.Open(src, vfs.OpenReadOnly)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	size, err := in.Size()
	if err != nil {
		return err
	}
	out, err := fs.Open(dst, vfs.OpenCreate)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if err := out.Truncate(size); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	var off int64
	for off < size {
		n, rerr := in.ReadAt(buf, off)
		if n > 0 {
			if _, werr := out.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
		}
		if rerr != nil {
			if off >= size {
				break
			}
			return rerr
		}
	}
	return out.Sync(vfs.SyncFull)
}
