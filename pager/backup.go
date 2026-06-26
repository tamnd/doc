package pager

import (
	"io"

	"github.com/tamnd/doc/format"
)

// BackupInfo reports what an online backup copied.
type BackupInfo struct {
	Pages int64 // pages written to the backup
	Bytes int64 // total bytes written
}

// Backup streams a consistent physical image of the database to w without closing
// it (spec 2061 doc 18 §10). It first checkpoints, folding every committed WAL
// frame into the main file so the on-disk image is complete, then freezes the
// checkpointer for the duration of the copy. With the checkpointer frozen, no path
// rewrites the main file: new commits only append WAL frames, so the bytes being
// streamed are a stable snapshot. The result needs no WAL replay; it is a finished
// .doc file at the snapshot version.
//
// verify, when true, re-checks every page's checksum as it is streamed, catching a
// read error or a bit flip during the copy at the cost of the extra hash.
//
// progress, when non-nil, is called after each page with the running byte count and
// the total, so a CLI can render a bar.
func (p *Pager) Backup(w io.Writer, verify bool, progress func(written, total int64)) (BackupInfo, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return BackupInfo{}, ErrClosed
	}
	if !p.readOnly {
		if err := p.checkpointLocked(); err != nil {
			p.mu.Unlock()
			return BackupInfo{}, err
		}
	}
	pageCount := int64(p.hdr.PageCount)
	pageSize := int64(p.pageSize)
	// Freeze the checkpointer and dirty-page stealing so nothing rewrites the main
	// file while it is copied. The first backup flips the freeze on; concurrent
	// backups share it and the last one out clears it.
	p.backupActive++
	p.pool.freezeDirty = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.backupActive--
		if p.backupActive == 0 {
			p.pool.freezeDirty = false
		}
		p.mu.Unlock()
	}()

	// The on-disk main file is frozen for the copy: the checkpointer is held off and
	// no dirty page can be stolen back to it. Read the pages straight from the file
	// so concurrent writers, which touch only the pool and the WAL, never block on
	// the backup.
	total := pageCount * pageSize
	buf := make([]byte, pageSize)
	var written int64
	for id := int64(0); id < pageCount; id++ {
		if _, err := p.main.ReadAt(buf, id*pageSize); err != nil {
			return BackupInfo{Pages: id, Bytes: written}, err
		}
		if verify && id != 0 && !isZeroPage(buf) {
			if err := format.VerifyPageChecksum(buf, p.checksum); err != nil {
				return BackupInfo{Pages: id, Bytes: written}, err
			}
		}
		n, err := w.Write(buf)
		written += int64(n)
		if err != nil {
			return BackupInfo{Pages: id, Bytes: written}, err
		}
		if progress != nil {
			progress(written, total)
		}
	}
	return BackupInfo{Pages: pageCount, Bytes: written}, nil
}
