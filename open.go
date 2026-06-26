package doc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/engine"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// SyncLevel controls how aggressively writes are flushed to stable storage. It
// maps directly onto the pager's durability barrier (spec 2061 doc 14 §18.2).
type SyncLevel int

const (
	// SyncOff issues no fsync. Fastest, but a crash can lose recent commits.
	SyncOff SyncLevel = iota
	// SyncNormal fsyncs at checkpoint. Safe in combination with the WAL.
	SyncNormal
	// SyncFull fsyncs on every WAL commit. Safest, and the default.
	SyncFull
)

// Codec is the pluggable on-disk document encoding. Only the default BSON codec
// is wired today; the seam exists so an alternate encoding can be slotted in
// without an API change.
type Codec interface {
	Name() string
}

// memoryPath is the reserved path that selects a private in-memory database.
const memoryPath = ":memory:"

// openConfig accumulates the functional options before they are folded into the
// engine options at Open time.
type openConfig struct {
	pageSize      int
	cacheSize     int64
	syncLevel     SyncLevel
	codec         Codec
	encryptionKey []byte
	busyTimeout   time.Duration
	readOnly      bool
	ttlInterval   time.Duration
	slowOpThresh  time.Duration
	logger        *slog.Logger
	profileLevel  int
}

// engineOptions folds the resolved open config into the engine options. It is
// shared by OpenContext and by Compact, which reopens the file with the same
// geometry after rewriting it.
func (cfg openConfig) engineOptions(clock sys.Clock) engine.Options {
	eopts := engine.Options{
		Pager: pager.Options{
			PageSize: uint32(cfg.pageSize),
			Sync:     pager.SyncLevel(cfg.syncLevel),
			ReadOnly: cfg.readOnly,
		},
		Clock: clock,
		IDGen: sys.NewObjectIDGenerator(clock),
	}
	if cfg.pageSize > 0 && cfg.cacheSize > 0 {
		eopts.Pager.PoolPages = int(cfg.cacheSize / int64(cfg.pageSize))
	}
	return eopts
}

func defaultOpenConfig() openConfig {
	return openConfig{
		pageSize:     16384,
		cacheSize:    64 << 20,
		syncLevel:    SyncFull,
		busyTimeout:  time.Second,
		ttlInterval:  60 * time.Second,
		slowOpThresh: 100 * time.Millisecond,
	}
}

// Option configures Open. Options are applied left to right.
type Option func(*openConfig)

// WithPageSize sets the page size for a newly created file. It is a create-time
// option: opening an existing file with a different page size is an error.
func WithPageSize(n int) Option { return func(c *openConfig) { c.pageSize = n } }

// WithCacheSize sets the buffer pool size in bytes.
func WithCacheSize(n int64) Option { return func(c *openConfig) { c.cacheSize = n } }

// WithSyncLevel sets the durability level.
func WithSyncLevel(l SyncLevel) Option { return func(c *openConfig) { c.syncLevel = l } }

// WithCodec sets the on-disk document codec.
func WithCodec(codec Codec) Option { return func(c *openConfig) { c.codec = codec } }

// WithEncryptionKey sets a 32-byte AES-256-GCM key. Create-time only.
func WithEncryptionKey(key []byte) Option { return func(c *openConfig) { c.encryptionKey = key } }

// WithBusyTimeout sets how long Open waits for the write lock before returning
// ErrBusy.
func WithBusyTimeout(d time.Duration) Option { return func(c *openConfig) { c.busyTimeout = d } }

// WithReadOnly opens the file read-only: writes return ErrReadOnly.
func WithReadOnly(b bool) Option { return func(c *openConfig) { c.readOnly = b } }

// WithTTLInterval sets how often the background TTL sweeper runs (spec 2061 doc 04
// §11.4). The default is 60 seconds. A non-positive interval disables the background
// sweeper; expiry can still be driven on demand.
func WithTTLInterval(d time.Duration) Option { return func(c *openConfig) { c.ttlInterval = d } }

// WithSlowOpThreshold sets the duration above which an operation is counted as
// slow and, at profiler level 1, logged and recorded to system.profile (spec 2061
// doc 18 §3). The default is 100 ms. A non-positive value disables slow-op
// accounting.
func WithSlowOpThreshold(d time.Duration) Option {
	return func(c *openConfig) { c.slowOpThresh = d }
}

// WithProfileLevel sets the initial profiler level (spec 2061 doc 18 §3.4): 0 off,
// 1 log operations slower than the slow-op threshold, 2 log every operation. The
// level can be changed at runtime with the profile pragma or the profile command.
func WithProfileLevel(level int) Option {
	return func(c *openConfig) { c.profileLevel = level }
}

// DB is the open handle to one .doc file. It is safe for concurrent use by many
// goroutines; share a single DB for the life of the program and derive cheap
// Database and Collection handles from it (spec 2061 doc 14 §2, §3).
type DB struct {
	eng   *engine.Engine
	clock sys.Clock
	cfg   openConfig // the resolved open options, read by the PRAGMA surface
	fs    vfs.FS     // the filesystem the file lives on, reused by Compact
	path  string     // the file path, reused by Compact to rebuild in place

	mu     sync.RWMutex
	closed bool

	pragMu     sync.Mutex // guards the session-runtime PRAGMA state below
	defaultIso isolation  // default isolation for sessions started on this DB
	autoVacuum string     // auto_vacuum mode: none, incremental, or full

	ttlStop chan struct{} // closed to stop the background sweeper
	ttlDone chan struct{} // closed when the sweeper goroutine has returned

	feed *changeFeed // in-memory change broadcaster, fed by the engine commit path
	met  *dbMetrics  // the metric registry, always live (spec 2061 doc 18 §2)
	log  *slog.Logger
	prof *profiler // slow-op log and system.profile capture (spec 2061 doc 18 §3)

	archiver atomic.Pointer[WALArchiver] // set while a WAL archiver is running
}

// Open opens an existing .doc file or creates a new one. The special path
// ":memory:" creates a private in-memory database with no file backing.
func Open(path string, opts ...Option) (*DB, error) {
	return OpenContext(context.Background(), path, opts...)
}

// OpenContext is Open with a context that bounds the open sequence (recovery,
// WAL replay, lock acquisition). If ctx is cancelled before the file is ready,
// it returns ctx.Err() and creates no file.
func OpenContext(ctx context.Context, path string, opts ...Option) (*DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfg := defaultOpenConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	var fs vfs.FS
	if path == memoryPath {
		fs = vfs.NewMemFS()
	} else {
		fs = vfs.NewOSFS()
	}

	clock := sys.SystemClock{}
	eopts := cfg.engineOptions(clock)

	eng, err := engine.Open(fs, path, eopts)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	db := &DB{eng: eng, clock: clock, cfg: cfg, fs: fs, path: path, defaultIso: isoSnapshot}
	db.log = cfg.logger
	db.met = newDBMetrics()
	db.prof = newProfiler(cfg.profileLevel, cfg.slowOpThresh)
	db.feed = newChangeFeed()
	eng.SetChangeHook(func(dbName, coll string, recs []collection.ChangeRecord, cv uint64) {
		for _, r := range recs {
			db.met.changefeedEvt.With(r.Op).Inc()
		}
		db.feed.publish(dbName, coll, recs, cv)
		if a := db.archiver.Load(); a != nil {
			a.observeVersion(db.eng.DurableLSN(), cv, db.clock.Now().Unix())
		}
	})
	if !cfg.readOnly && cfg.ttlInterval > 0 {
		db.startTTLSweeper(cfg.ttlInterval)
	}
	db.logStartup()
	return db, nil
}

// startTTLSweeper launches the background goroutine that runs a TTL expiry pass on
// each tick until the database closes (spec 2061 doc 04 §11.4).
func (db *DB) startTTLSweeper(interval time.Duration) {
	db.ttlStop = make(chan struct{})
	db.ttlDone = make(chan struct{})
	go func() {
		defer close(db.ttlDone)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-db.ttlStop:
				return
			case <-t.C:
				_, _ = db.eng.SweepTTL(db.clock.Now())
			}
		}
	}()
}

// SweepExpired runs one TTL expiry pass immediately and returns the number of
// documents deleted. It is the on-demand counterpart to the background sweeper,
// useful when a caller wants deterministic expiry rather than waiting for a tick.
func (db *DB) SweepExpired(ctx context.Context) (int, error) {
	if err := db.check(ctx); err != nil {
		return 0, err
	}
	return db.eng.SweepTTL(db.clock.Now())
}

// Close flushes dirty pages, checkpoints the WAL, releases the file lock, and
// closes the file. Handles derived from the DB return ErrClosed afterward.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	if a := db.archiver.Load(); a != nil {
		// Drain any buffered commits to the sink before the engine shuts down.
		a.stop()
		db.archiver.Store(nil)
	}
	if db.ttlStop != nil {
		close(db.ttlStop)
		<-db.ttlDone
	}
	return db.eng.Close()
}

// isClosed reports whether Close has run, under the read lock.
func (db *DB) isClosed() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.closed
}
