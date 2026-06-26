package doc

import (
	"context"
	"io"
	"time"

	"github.com/tamnd/doc/metrics"
)

// dbMetrics is the per-database metric catalogue (spec 2061 doc 18 §2.3). It owns
// one registry and a typed handle to each metric vector so the hot paths increment
// without a map lookup. The storage and catalog gauges are not tracked
// incrementally: refresh pulls them from the pager and the catalog just before a
// scrape, so they are always consistent with the file even after recovery.
type dbMetrics struct {
	reg *metrics.Registry

	opsTotal      *metrics.CounterVec   // op, collection
	opDuration    *metrics.HistogramVec // op, collection
	docsExamined  *metrics.CounterVec   // op, collection
	docsReturned  *metrics.CounterVec   // op, collection
	keysExamined  *metrics.CounterVec   // op, collection
	slowQuery     *metrics.CounterVec   // op, collection
	changefeedEvt *metrics.CounterVec   // op_type

	bytesRead      *metrics.CounterVec
	bytesWritten   *metrics.CounterVec
	pageReads      *metrics.CounterVec
	pageWrites     *metrics.CounterVec
	cacheEvictions *metrics.CounterVec
	walFrames      *metrics.CounterVec
	checkpoints    *metrics.CounterVec
	fsyncs         *metrics.CounterVec
	fsyncErrors    *metrics.CounterVec

	walSizePages  *metrics.GaugeVec
	bufferPool    *metrics.GaugeVec
	fileSize      *metrics.GaugeVec
	freelistPages *metrics.GaugeVec
	fragmentation *metrics.GaugeVec
	collectionCnt *metrics.GaugeVec
	indexCnt      *metrics.GaugeVec
	documentCnt   *metrics.GaugeVec // collection
	indexSize     *metrics.GaugeVec // collection, index
	activeCursors *metrics.GaugeVec

	totalOps  metrics.Counter // every op, for the snapshot headline
	totalSlow metrics.Counter // every slow op, for the snapshot headline
}

// newDBMetrics registers the whole catalogue against a fresh registry. Every
// metric the spec marks normative is created here so it appears in the exposition
// even before the first observation.
func newDBMetrics() *dbMetrics {
	r := metrics.New()
	m := &dbMetrics{reg: r}

	m.opsTotal = r.Counter("doc_ops_total", "Operations completed, by type and collection.", "op", "collection")
	m.opDuration = r.Histogram("doc_op_duration_seconds", "End-to-end wall-clock duration per operation.", nil, "op", "collection")
	m.docsExamined = r.Counter("doc_docs_examined_total", "Documents fetched from the record store during execution.", "op", "collection")
	m.docsReturned = r.Counter("doc_docs_returned_total", "Documents returned to the caller.", "op", "collection")
	m.keysExamined = r.Counter("doc_keys_examined_total", "Index keys scanned during execution.", "op", "collection")
	m.slowQuery = r.Counter("doc_slow_query_total", "Operations that exceeded the slow-query threshold.", "op", "collection")
	m.changefeedEvt = r.Counter("doc_changefeed_events_total", "Change-feed events emitted.", "op_type")

	m.bytesRead = r.Counter("doc_bytes_read_total", "Bytes read from storage (pager page reads).")
	m.bytesWritten = r.Counter("doc_bytes_written_total", "Bytes written to storage (WAL appends plus checkpointed pages).")
	m.pageReads = r.Counter("doc_page_reads_total", "Pages read from disk into the buffer pool.")
	m.pageWrites = r.Counter("doc_page_writes_total", "Pages written out (checkpoint plus WAL).")
	m.cacheEvictions = r.Counter("doc_cache_evictions_total", "Pages evicted from the buffer pool.")
	m.walFrames = r.Counter("doc_wal_frames_total", "WAL frames appended since open.")
	m.checkpoints = r.Counter("doc_checkpoint_total", "Checkpoints completed.", "mode")
	m.fsyncs = r.Counter("doc_fsync_total", "fsync calls issued.")
	m.fsyncErrors = r.Counter("doc_fsync_errors_total", "fsync calls that returned an error.")

	m.walSizePages = r.Gauge("doc_wal_size_pages", "Current WAL size in pages.")
	m.bufferPool = r.Gauge("doc_buffer_pool_bytes", "Total buffer pool size in bytes.")
	m.fileSize = r.Gauge("doc_file_size_bytes", "Total size of the .doc file.")
	m.freelistPages = r.Gauge("doc_freelist_pages", "Pages on the freelist (reclaimed but not yet vacuumed).")
	m.fragmentation = r.Gauge("doc_fragmentation_ratio", "Live bytes over total file bytes (lower is more fragmented).")
	m.collectionCnt = r.Gauge("doc_collection_count", "Number of collections in the catalog.")
	m.indexCnt = r.Gauge("doc_index_count", "Total number of indexes across all collections.")
	m.documentCnt = r.Gauge("doc_document_count", "Estimated document count per collection.", "collection")
	m.indexSize = r.Gauge("doc_index_size_bytes", "On-disk size of an index B-tree.", "collection", "index")
	m.activeCursors = r.Gauge("doc_active_cursors", "Open cursors (document iterators).")

	return m
}

// recordOp folds one finished operation into the op counters: the op total, the
// latency histogram, the examined/returned/keys counters, and the slow-query
// counter when the duration crossed the threshold. coll is the full namespace.
func (m *dbMetrics) recordOp(op, coll string, dur time.Duration, examined, returned, keys int64, slowThreshold time.Duration) {
	m.opsTotal.With(op, coll).Inc()
	m.opDuration.With(op, coll).Observe(dur.Seconds())
	m.totalOps.Inc()
	if examined > 0 {
		m.docsExamined.With(op, coll).Add(examined)
	}
	if returned > 0 {
		m.docsReturned.With(op, coll).Add(returned)
	}
	if keys > 0 {
		m.keysExamined.With(op, coll).Add(keys)
	}
	if slowThreshold > 0 && dur >= slowThreshold {
		m.slowQuery.With(op, coll).Inc()
		m.totalSlow.Inc()
	}
}

// observe starts timing one operation and returns a recorder. Call the recorder
// (normally via defer) with the execution stats the op gathered; an op that has no
// cheap stats passes zeros and only the op count and latency are recorded, which
// are the two headline metrics the spec names (doc 18 §2.3). The namespace label is
// resolved once so the deferred call stays allocation-free.
func (c *Collection) observe(op string) func(examined, returned, keys int64) {
	start := time.Now()
	ns := c.dbName + "." + c.name
	return func(examined, returned, keys int64) {
		dur := time.Since(start)
		c.db.met.recordOp(op, ns, dur, examined, returned, keys, c.db.cfg.slowOpThresh)
		c.db.profileOp(c.dbName, c.name, op, dur, examined, returned, keys)
	}
}

// refresh pulls the live storage and catalog numbers into the gauges and folds the
// pager's cumulative counters into the counter metrics. It runs just before any
// read of the registry (a scrape, a Metrics() snapshot, or serverStatus) so the
// pull-based metrics are current without a background ticker.
func (db *DB) refreshMetrics() {
	m := db.met
	ps := db.eng.PagerStats()
	g := db.eng.Gauges()

	setCounter(m.bytesRead.With(), int64(ps.BytesRead))
	setCounter(m.bytesWritten.With(), int64(ps.BytesWritten))
	setCounter(m.pageReads.With(), int64(ps.PageReads))
	setCounter(m.pageWrites.With(), int64(ps.PageWrites))
	setCounter(m.cacheEvictions.With(), int64(ps.CacheEvictions))
	setCounter(m.walFrames.With(), int64(ps.WALFramesTotal))
	setCounter(m.checkpoints.With("full"), int64(ps.Checkpoints))
	setCounter(m.fsyncs.With(), int64(ps.Fsyncs))
	setCounter(m.fsyncErrors.With(), int64(ps.FsyncErrors))

	m.walSizePages.With().Set(int64(ps.WALSizePages))
	m.bufferPool.With().Set(db.cfg.cacheSize)
	m.fileSize.With().Set(g.FileSizeBytes)
	m.freelistPages.With().Set(g.FreelistPages)
	m.collectionCnt.With().Set(g.Collections)
	m.indexCnt.With().Set(g.Indexes)

	var live int64
	for _, c := range g.PerCollection {
		m.documentCnt.With(c.Namespace).Set(c.DocumentCount)
		for _, ix := range c.Indexes {
			m.indexSize.With(ix.Namespace, ix.Name).Set(ix.SizeBytes)
		}
	}
	if g.FileSizeBytes > 0 {
		// live bytes are approximated by the heap plus index footprint; without a
		// per-collection live-byte read we use the gauge total the pager reports.
		used := g.FileSizeBytes - g.FreelistPages*int64(ps.PageSize)
		live = used
		m.fragmentation.With().Set(ratioPermille(live, g.FileSizeBytes))
	}
}

// setCounter forces a counter to an absolute value. The pager's counters are
// cumulative since open, so the metric mirrors them rather than being incremented
// on every I/O.
func setCounter(c *metrics.Counter, abs int64) {
	c.Add(abs - c.Value())
}

// ratioPermille expresses num/den as a per-mille integer so the gauge stays an
// integer while still carrying three digits of the live-bytes ratio.
func ratioPermille(num, den int64) int64 {
	if den <= 0 {
		return 0
	}
	return num * 1000 / den
}

// MetricsSnapshot is the headline view of the metric registry, the embedded-mode
// counterpart to scraping /metrics (spec 2061 doc 18 §2.4). It carries the numbers
// the serverStatus command reports and a runbook reads first.
type MetricsSnapshot struct {
	OpsTotal       int64
	SlowQueryTotal int64
	PageReads      int64
	PageWrites     int64
	BytesRead      int64
	BytesWritten   int64
	CacheHits      int64
	CacheMisses    int64
	CacheEvictions int64
	WALFramesTotal int64
	WALSizePages   int64
	Checkpoints    int64
	Fsyncs         int64
	FsyncErrors    int64
	FileSizeBytes  int64
	FreelistPages  int64
	Collections    int64
	Indexes        int64
	DocumentCount  int64
}

// Metrics returns the headline metric snapshot. It pulls the live storage and
// catalog numbers so the result is current at the moment of the call.
func (db *DB) Metrics(ctx context.Context) (*MetricsSnapshot, error) {
	if err := db.check(ctx); err != nil {
		return nil, err
	}
	db.refreshMetrics()
	ps := db.eng.PagerStats()
	g := db.eng.Gauges()
	var docs int64
	for _, c := range g.PerCollection {
		docs += c.DocumentCount
	}
	return &MetricsSnapshot{
		OpsTotal:       db.met.totalOps.Value(),
		SlowQueryTotal: db.met.totalSlow.Value(),
		PageReads:      int64(ps.PageReads),
		PageWrites:     int64(ps.PageWrites),
		BytesRead:      int64(ps.BytesRead),
		BytesWritten:   int64(ps.BytesWritten),
		CacheHits:      int64(ps.CacheHits),
		CacheMisses:    int64(ps.CacheMisses),
		CacheEvictions: int64(ps.CacheEvictions),
		WALFramesTotal: int64(ps.WALFramesTotal),
		WALSizePages:   int64(ps.WALSizePages),
		Checkpoints:    int64(ps.Checkpoints),
		Fsyncs:         int64(ps.Fsyncs),
		FsyncErrors:    int64(ps.FsyncErrors),
		FileSizeBytes:  g.FileSizeBytes,
		FreelistPages:  g.FreelistPages,
		Collections:    g.Collections,
		Indexes:        g.Indexes,
		DocumentCount:  docs,
	}, nil
}

// WritePrometheus writes the database's metrics in the Prometheus text exposition
// format (spec 2061 doc 18 §2.4). It refreshes the pull-based gauges first, so a
// single call is a complete, current scrape. The doc serve metrics endpoint calls
// it directly.
func (db *DB) WritePrometheus(ctx context.Context, w io.Writer) error {
	if err := db.check(ctx); err != nil {
		return err
	}
	db.refreshMetrics()
	return db.met.reg.WritePrometheus(w)
}

// MetricsRegistry exposes the underlying registry so an embedding application can
// fold doc's metrics into its own scrape. Call RefreshMetrics first to update the
// pull-based gauges and counters.
func (db *DB) MetricsRegistry() *metrics.Registry { return db.met.reg }

// RefreshMetrics updates the pull-based gauges and counters from the current
// storage and catalog state. Metrics and WritePrometheus call it automatically.
func (db *DB) RefreshMetrics() {
	if db.isClosed() {
		return
	}
	db.refreshMetrics()
}
