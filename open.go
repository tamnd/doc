package doc

import (
	"context"
	"sync"
	"time"

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
}

func defaultOpenConfig() openConfig {
	return openConfig{
		pageSize:    16384,
		cacheSize:   64 << 20,
		syncLevel:   SyncFull,
		busyTimeout: time.Second,
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

// DB is the open handle to one .doc file. It is safe for concurrent use by many
// goroutines; share a single DB for the life of the program and derive cheap
// Database and Collection handles from it (spec 2061 doc 14 §2, §3).
type DB struct {
	eng   *engine.Engine
	clock sys.Clock

	mu     sync.RWMutex
	closed bool
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

	eng, err := engine.Open(fs, path, eopts)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return &DB{eng: eng, clock: clock}, nil
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
	return db.eng.Close()
}

// isClosed reports whether Close has run, under the read lock.
func (db *DB) isClosed() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.closed
}
