package doc

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/wal"
)

// WAL archiving streams every committed transaction to a sink as ordered segment
// files, the foundation for WAL shipping replicas and point-in-time recovery (spec
// 2061 doc 18 §13.5, §14). The primitive is intentionally simple: a commit observer
// captures each commit's page images in memory the moment they are durable, before
// any checkpoint can fold them away, and a background flusher writes batches of
// captured commits to the sink as segments named by their version range.

// WALSink is where archived segments are written and read back. A filesystem sink
// ships in the box (DirSink); an object-store sink is an adapter a caller supplies,
// so the engine keeps its zero-dependency footprint.
type WALSink interface {
	// Put writes a segment under name, overwriting any existing one.
	Put(name string, data []byte) error
	// List returns every segment name, in any order.
	List() ([]string, error)
	// Get reads back the segment written under name.
	Get(name string) ([]byte, error)
}

// WALArchiverOptions configures a WAL archiver.
type WALArchiverOptions struct {
	// Sink is the destination for segments. Required.
	Sink WALSink
	// FlushInterval bounds how long a committed transaction waits in memory before
	// it is written to the sink. Zero defaults to one second.
	FlushInterval time.Duration
	// MaxBatch caps how many commits one segment holds. Zero defaults to 256.
	MaxBatch int
}

// WALArchiverStats reports an archiver's progress.
type WALArchiverStats struct {
	Segments int    // segments written to the sink
	Commits  int    // commits archived
	Frames   int    // page images archived
	Pending  int    // commits captured but not yet flushed
	Buffered int    // bytes of pending page images held in memory
	LastVer  uint64 // highest commit version archived
}

// capturedCommit is one commit held in memory until the next flush.
type capturedCommit struct {
	commitLSN   uint64
	dbSizePages uint32
	frames      []wal.PageImage
	version     uint64
	timeUnix    int64
}

// WALArchiver captures committed frames and ships them to a sink as segments.
type WALArchiver struct {
	db   *DB
	sink WALSink
	opts WALArchiverOptions

	mu       sync.Mutex
	pending  []*capturedCommit
	byLSN    map[uint64]*capturedCommit
	lastVer  uint64 // last version seen, carried forward to unannotated commits
	lastTime int64
	seq      int // next segment sequence number
	bufBytes int

	stats struct {
		segments, commits, frames int
		lastVer                   uint64
	}

	stopCh chan struct{}
	doneCh chan struct{}
	closed bool
}

// ErrArchiveRunning is returned when a second archiver is started on a database that
// already has one.
var ErrArchiveRunning = errors.New("doc: a WAL archiver is already running on this database")

// ArchiveWAL starts streaming committed transactions to opts.Sink. The archiver runs
// until Stop or until the database closes. Only one archiver may run per database.
func (db *DB) ArchiveWAL(opts WALArchiverOptions) (*WALArchiver, error) {
	if opts.Sink == nil {
		return nil, errors.New("doc: WAL archiver needs a sink")
	}
	if db.isClosed() {
		return nil, ErrClosed
	}
	if db.cfg.readOnly {
		return nil, ErrReadOnly
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 256
	}

	a := &WALArchiver{
		db:     db,
		sink:   opts.Sink,
		opts:   opts,
		byLSN:  make(map[uint64]*capturedCommit),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	if !db.archiver.CompareAndSwap(nil, a) {
		return nil, ErrArchiveRunning
	}
	db.eng.SetCommitObserver(a.observeCommit)
	go a.run()
	db.logger(logComponentWAL).Info("WAL archiver started", "flushInterval", opts.FlushInterval.String())
	return a, nil
}

// observeCommit runs on the commit path with the page images one commit made durable.
// It copies them into a pending capture; the flusher writes them to the sink later.
func (a *WALArchiver) observeCommit(ev pager.CommitEvent) {
	frames := make([]wal.PageImage, len(ev.Frames))
	var bytes int
	for i, f := range ev.Frames {
		payload := make([]byte, len(f.Payload))
		copy(payload, f.Payload)
		frames[i] = wal.PageImage{PageID: f.PageID, Payload: payload}
		bytes += len(payload)
	}
	// Stamp the commit time here, on the commit path, so every captured commit has a
	// real cluster time even when it skips the change hook (a catalog or index write).
	// observeVersion fills in the oracle version for the data commits that have one.
	c := &capturedCommit{
		commitLSN:   ev.CommitLSN,
		dbSizePages: ev.DBSizePages,
		frames:      frames,
		timeUnix:    a.db.clock.Now().Unix(),
	}

	a.mu.Lock()
	a.pending = append(a.pending, c)
	a.byLSN[ev.CommitLSN] = c
	a.bufBytes += bytes
	a.mu.Unlock()
}

// observeVersion annotates a captured commit with its oracle version and cluster
// time, correlating by the commit LSN both callbacks share.
func (a *WALArchiver) observeVersion(commitLSN, version uint64, timeUnix int64) {
	a.mu.Lock()
	if c := a.byLSN[commitLSN]; c != nil {
		c.version = version
		if c.timeUnix == 0 {
			c.timeUnix = timeUnix
		}
	}
	a.mu.Unlock()
}

// run is the flush loop: every FlushInterval it writes the pending commits to the
// sink as one segment.
func (a *WALArchiver) run() {
	defer close(a.doneCh)
	t := time.NewTicker(a.opts.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-a.stopCh:
			a.flush()
			return
		case <-t.C:
			a.flush()
		}
	}
}

// flush drains the pending commits into one or more segments and writes them.
func (a *WALArchiver) flush() {
	for {
		a.mu.Lock()
		if len(a.pending) == 0 {
			a.mu.Unlock()
			return
		}
		n := len(a.pending)
		if n > a.opts.MaxBatch {
			n = a.opts.MaxBatch
		}
		batch := a.pending[:n]
		seg := a.buildSegment(batch)
		data := seg.Encode()
		name := fmt.Sprintf("seg-%020d-%020d-%010d.seg", seg.BaseVersion, seg.EndVersion, a.seq)
		a.seq++
		a.mu.Unlock()

		if err := a.sink.Put(name, data); err != nil {
			a.db.logger(logComponentWAL).Error("WAL archive flush failed", "segment", name, "error", err.Error())
			return // leave the batch pending; the next tick retries
		}

		a.mu.Lock()
		// Drop the flushed commits now that they are durable in the sink.
		for _, c := range batch {
			delete(a.byLSN, c.commitLSN)
			for _, f := range c.frames {
				a.bufBytes -= len(f.Payload)
			}
		}
		a.pending = a.pending[n:]
		a.stats.segments++
		a.stats.commits += len(batch)
		for _, c := range batch {
			a.stats.frames += len(c.frames)
		}
		a.stats.lastVer = seg.EndVersion
		more := len(a.pending) > 0
		a.mu.Unlock()
		if !more {
			return
		}
	}
}

// buildSegment turns a batch of captured commits into a segment, carrying the last
// known version and time forward onto any unannotated commit (a DDL commit that did
// not fire the change hook). Call under a.mu.
func (a *WALArchiver) buildSegment(batch []*capturedCommit) *wal.Segment {
	seg := &wal.Segment{
		PageSize:     uint32(a.db.eng.PageSize()),
		BaseVersion:  a.lastVer,
		BaseTimeUnix: a.lastTime,
		Commits:      make([]wal.Commit, 0, len(batch)),
	}
	for _, c := range batch {
		v := c.version
		if v == 0 {
			v = a.lastVer
		}
		ts := c.timeUnix
		if ts == 0 {
			ts = a.lastTime
		}
		a.lastVer, a.lastTime = v, ts
		seg.Commits = append(seg.Commits, wal.Commit{
			Version:     v,
			TimeUnix:    ts,
			DBSizePages: c.dbSizePages,
			Frames:      c.frames,
		})
	}
	seg.EndVersion = a.lastVer
	seg.EndTimeUnix = a.lastTime
	return seg
}

// Flush forces every pending commit out to the sink immediately, rather than waiting
// for the next interval. It is what a test or a clean shutdown calls to make the
// archive current.
func (a *WALArchiver) Flush() { a.flush() }

// Stats returns a snapshot of the archiver's progress.
func (a *WALArchiver) Stats() WALArchiverStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return WALArchiverStats{
		Segments: a.stats.segments,
		Commits:  a.stats.commits,
		Frames:   a.stats.frames,
		Pending:  len(a.pending),
		Buffered: a.bufBytes,
		LastVer:  a.stats.lastVer,
	}
}

// Stop flushes any pending commits, detaches the observer, and stops the flush loop.
func (a *WALArchiver) Stop() {
	a.db.eng.SetCommitObserver(nil)
	a.db.archiver.CompareAndSwap(a, nil)
	a.stop()
}

// stop halts the flush loop and waits for a final flush. Safe to call more than once.
func (a *WALArchiver) stop() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	close(a.stopCh)
	<-a.doneCh
}

// segmentsInOrder reads every segment from a sink and returns them sorted by name,
// which orders them by version range.
func segmentsInOrder(sink WALSink) ([]*wal.Segment, error) {
	names, err := sink.List()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	segs := make([]*wal.Segment, 0, len(names))
	for _, name := range names {
		data, err := sink.Get(name)
		if err != nil {
			return nil, fmt.Errorf("read segment %s: %w", name, err)
		}
		seg, err := wal.DecodeSegment(data)
		if err != nil {
			return nil, fmt.Errorf("decode segment %s: %w", name, err)
		}
		segs = append(segs, seg)
	}
	return segs, nil
}
