package doc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// systemProfileName is the capped collection in each database that holds profiler
// events, mirroring MongoDB's system.profile (spec 2061 doc 18 §3.5).
const systemProfileName = "system.profile"

// defaultProfileSizeBytes caps system.profile so old events roll off automatically.
// MongoDB uses 1 MiB; doc follows that default (spec 2061 doc 18 §3.5).
const defaultProfileSizeBytes = 1 << 20

// profiler holds the runtime profiling state: the level (0 off, 1 slow ops, 2 all
// ops) and the slow-op threshold (spec 2061 doc 18 §3.4). It is consulted on every
// finished operation, so the level read is a single atomic load on the hot path.
type profiler struct {
	level      atomic.Int32
	slowThresh time.Duration

	mu      sync.Mutex      // guards lazy creation of system.profile per database
	created map[string]bool // database names whose system.profile exists
}

// newProfiler builds a profiler at the given starting level. A level outside 0..2
// is clamped into range so a bad option cannot leave the profiler in a junk state.
func newProfiler(level int, slow time.Duration) *profiler {
	if level < 0 {
		level = 0
	}
	if level > 2 {
		level = 2
	}
	p := &profiler{slowThresh: slow, created: make(map[string]bool)}
	p.level.Store(int32(level))
	return p
}

// Level returns the current profiler level.
func (p *profiler) Level() int { return int(p.level.Load()) }

// SetLevel changes the profiler level. Values outside 0..2 are clamped.
func (p *profiler) SetLevel(level int) {
	if level < 0 {
		level = 0
	}
	if level > 2 {
		level = 2
	}
	p.level.Store(int32(level))
}

// profileOp is called from the metrics recorder after every finished operation. It
// decides, from the current level and the duration, whether the op is logged to the
// slow-query log and recorded to system.profile (spec 2061 doc 18 §3). It never
// profiles writes to the profile collection itself, which would recurse.
func (db *DB) profileOp(dbName, coll, op string, dur time.Duration, examined, returned, keys int64) {
	if coll == systemProfileName {
		return
	}
	lvl := db.prof.Level()
	if lvl == 0 {
		return
	}
	slow := db.prof.slowThresh > 0 && dur >= db.prof.slowThresh
	if lvl == 1 && !slow {
		return
	}
	ns := dbName + "." + coll
	millis := dur.Milliseconds()

	if slow {
		// The slow-query log is a WARN record carrying the examined-to-returned
		// ratio, the headline index-health signal (spec 2061 doc 18 §3.1, §3.2).
		db.logger(logComponentCommand).LogAttrs(context.Background(), slog.LevelWarn, "slow operation",
			slog.String("ns", ns),
			slog.String("op", op),
			slog.Int64("docsExamined", examined),
			slog.Int64("keysExamined", keys),
			slog.Int64("nreturned", returned),
			slog.Int64("durationMillis", millis),
		)
	}
	db.writeProfileEvent(dbName, op, ns, millis, examined, returned, keys)
}

// writeProfileEvent appends one event to <db>.system.profile, creating the capped
// collection on first use. A failure to write a profile event is swallowed: the
// profiler is a diagnostic aid and must never fail the operation that triggered it.
func (db *DB) writeProfileEvent(dbName, op, ns string, millis, examined, returned, keys int64) {
	if err := db.ensureProfileCollection(dbName); err != nil {
		return
	}
	ev := bson.NewBuilder().
		AppendString("op", op).
		AppendString("ns", ns).
		AppendDateTime("ts", db.clock.Now().UnixMilli()).
		AppendInt64("durationMillis", millis).
		AppendInt64("docsExamined", examined).
		AppendInt64("keysExamined", keys).
		AppendInt64("nreturned", returned).
		Build()
	coll := db.Database(dbName).Collection(systemProfileName)
	_, _ = coll.InsertOne(context.Background(), Raw(ev))
}

// ensureProfileCollection lazily creates the capped system.profile collection for a
// database the first time a profile event is written to it. An "already exists"
// outcome is treated as success so a reopened database does not error.
func (db *DB) ensureProfileCollection(dbName string) error {
	db.prof.mu.Lock()
	defer db.prof.mu.Unlock()
	if db.prof.created[dbName] {
		return nil
	}
	opts := options.CreateCollection().
		SetCapped(true).
		SetSizeInBytes(defaultProfileSizeBytes)
	err := db.Database(dbName).CreateCollection(context.Background(), systemProfileName, opts)
	if err != nil && !errorsIsNamespaceExists(err) {
		return err
	}
	db.prof.created[dbName] = true
	return nil
}

// errorsIsNamespaceExists reports whether err is the catalog's already-exists error,
// which ensureProfileCollection treats as a benign outcome.
func errorsIsNamespaceExists(err error) bool {
	return err == ErrNamespaceExists
}
