package doc

import (
	"context"
	"log/slog"
)

// Log component names tag every record with the subsystem that produced it (spec
// 2061 doc 18 §6.1). They are stable strings so a log pipeline can route or filter
// on them.
const (
	logComponentCommand    = "COMMAND"
	logComponentStorage    = "STORAGE"
	logComponentWAL        = "WAL"
	logComponentRecovery   = "RECOVERY"
	logComponentChangefeed = "CHANGEFEED"
	logComponentBackup     = "BACKUP"
	logComponentCatalog    = "CATALOG"
	logComponentSecurity   = "SECURITY"
)

// WithLogger sets the slog.Logger doc routes all of its structured logging through
// (spec 2061 doc 18 §6.1). When unset, doc uses slog.Default(), so a process that
// has already configured the standard logger gets doc's records for free. Pass a
// logger backed by a discard handler to silence doc entirely.
func WithLogger(l *slog.Logger) Option {
	return func(c *openConfig) { c.logger = l }
}

// logger returns the database logger tagged with the given component. It never
// returns nil: an unset logger falls back to slog.Default() so callers can log
// unconditionally.
func (db *DB) logger(component string) *slog.Logger {
	base := db.log
	if base == nil {
		base = slog.Default()
	}
	return base.With(slog.String("component", component))
}

// logStartup emits the single INFO record that records the state of the database at
// open (spec 2061 doc 18 §6.3). The numbers come straight from the pager and the
// catalog so the line is a faithful snapshot of what was opened.
func (db *DB) logStartup() {
	g := db.eng.Gauges()
	ps := db.eng.PagerStats()
	var docs int64
	for _, c := range g.PerCollection {
		docs += c.DocumentCount
	}
	db.logger(logComponentStorage).LogAttrs(context.Background(), slog.LevelInfo, "database opened",
		slog.String("path", db.path),
		slog.Int("formatVersion", formatVersion),
		slog.Int("pageSize", db.eng.PageSize()),
		slog.Int64("walSizePages", int64(ps.WALSizePages)),
		slog.Int64("documentCount", docs),
		slog.Int64("collectionCount", g.Collections),
		slog.Int64("indexCount", g.Indexes),
		slog.Int64("fileSizeBytes", g.FileSizeBytes),
		slog.Bool("encryptionEnabled", len(db.cfg.encryptionKey) > 0),
		slog.Bool("readOnly", db.cfg.readOnly),
	)
}

// formatVersion is the on-disk format version doc reports in the startup log and
// build info. It tracks the file header version the pager writes (spec 2061 doc 03).
const formatVersion = 1
