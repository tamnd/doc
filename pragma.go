package doc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/pager"
)

// isolation is the database's default transaction isolation, the value behind the
// default_isolation PRAGMA (spec 2061 doc 19 §21.5).
type isolation int

const (
	isoSnapshot isolation = iota
	isoSerializable
)

// level maps the default isolation onto the collection layer's enum.
func (i isolation) level() collection.IsolationLevel {
	if i == isoSerializable {
		return collection.Serializable
	}
	return collection.SnapshotIsolation
}

func (i isolation) String() string {
	if i == isoSerializable {
		return "serializable"
	}
	return "snapshot"
}

// pragmaDesc describes one entry of the PRAGMA surface (spec 2061 doc 19 §21): how
// to read its current value, whether it can be written at runtime, and if so how.
// A read-only entry (create-time geometry, or a knob fixed at open) leaves write
// nil and returns a clear error when a write is attempted.
type pragmaDesc struct {
	read  func(*DB) string
	write func(*DB, string) error // nil = read-only at runtime
	scope string                  // human-readable scope, for error text
}

// pragmas is the registry of every PRAGMA doc backs with real engine state. A name
// absent here is rejected rather than silently accepted, so a knob whose milestone
// has not landed (encryption, telemetry, columnar store) reads as unimplemented
// instead of appearing to take effect.
var pragmas = map[string]pragmaDesc{
	"synchronous": {
		scope: "runtime",
		read:  func(db *DB) string { return syncName(db.eng.SyncLevel()) },
		write: func(db *DB, v string) error {
			l, ok := parseSyncName(v)
			if !ok {
				return fmt.Errorf("doc: synchronous wants off, normal, full, or extra, got %q", v)
			}
			db.eng.SetSyncLevel(l)
			return nil
		},
	},
	"default_isolation": {
		scope: "runtime",
		read:  func(db *DB) string { return db.isolationDefault().String() },
		write: func(db *DB, v string) error {
			switch strings.ToLower(v) {
			case "snapshot":
				db.setIsolationDefault(isoSnapshot)
			case "serializable":
				db.setIsolationDefault(isoSerializable)
			default:
				return fmt.Errorf("doc: default_isolation wants snapshot or serializable, got %q", v)
			}
			return nil
		},
	},
	"page_size": {
		scope: "create-time",
		read:  func(db *DB) string { return strconv.Itoa(db.eng.PageSize()) },
	},
	"journal_mode": {
		scope: "create-time",
		read:  func(*DB) string { return "wal" },
	},
	"cache_size": {
		scope: "open-time",
		read:  func(db *DB) string { return strconv.FormatInt(db.cfg.cacheSize, 10) },
	},
	"busy_timeout_ms": {
		scope: "open-time",
		read:  func(db *DB) string { return strconv.FormatInt(db.cfg.busyTimeout.Milliseconds(), 10) },
	},
	"read_only": {
		scope: "open-time",
		read:  func(db *DB) string { return strconv.FormatBool(db.cfg.readOnly) },
	},
	"max_doc_size": {
		scope: "fixed",
		read:  func(*DB) string { return strconv.Itoa(maxBSONSize) },
	},
	"profile": {
		scope: "runtime",
		read:  func(db *DB) string { return strconv.Itoa(db.prof.Level()) },
		write: func(db *DB, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > 2 {
				return fmt.Errorf("doc: profile wants 0, 1, or 2, got %q", v)
			}
			db.prof.SetLevel(n)
			return nil
		},
	},
	"wal_checkpoint": {
		scope: "runtime",
		read:  func(db *DB) string { return strconv.FormatUint(db.eng.PagerStats().WALSizePages, 10) },
		write: func(db *DB, v string) error {
			mode := strings.TrimSpace(v)
			if mode == "1" || strings.EqualFold(mode, "checkpoint") {
				mode = ""
			}
			return db.Checkpoint(context.Background(), mode)
		},
	},
	"wal_autocheckpoint": {
		scope: "runtime",
		read:  func(db *DB) string { return strconv.Itoa(db.eng.CheckpointThreshold()) },
		write: func(db *DB, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return fmt.Errorf("doc: wal_autocheckpoint wants a non-negative frame count, got %q", v)
			}
			db.eng.SetCheckpointThreshold(n)
			return nil
		},
	},
	"auto_vacuum": {
		scope: "runtime",
		read:  func(db *DB) string { return db.autoVacuumMode() },
		write: func(db *DB, v string) error {
			mode, ok := parseAutoVacuumMode(v)
			if !ok {
				return fmt.Errorf("doc: auto_vacuum wants none, incremental, or full, got %q", v)
			}
			db.setAutoVacuumMode(mode)
			return nil
		},
	},
	"incremental_vacuum": {
		scope: "runtime",
		read:  func(*DB) string { return "0" },
		write: func(db *DB, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return fmt.Errorf("doc: incremental_vacuum wants a non-negative page count, got %q", v)
			}
			_, err = db.IncrementalVacuum(context.Background(), n)
			return err
		},
	},
}

// parseAutoVacuumMode normalizes an auto_vacuum value. SQLite accepts the numeric
// forms 0, 1, 2 as well as the names none, full, incremental; doc accepts both.
func parseAutoVacuumMode(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "none":
		return "none", true
	case "1", "full":
		return "full", true
	case "2", "incremental":
		return "incremental", true
	}
	return "", false
}

// maxBSONSize is the largest document doc accepts, matching MongoDB's 16 MiB limit
// (spec 2061 doc 19 §21, max_doc_size).
const maxBSONSize = 16 * 1024 * 1024

// Pragma reads or writes one engine PRAGMA (spec 2061 doc 19 §21). An empty value
// reads the current setting; a non-empty value writes it. Either way it returns the
// resulting value, so a write echoes what it set. Writing a create-time or open-time
// knob, or naming an unknown PRAGMA, returns an error rather than a silent no-op.
func (db *DB) Pragma(name, value string) (string, error) {
	if db.isClosed() {
		return "", ErrClosed
	}
	key := strings.ToLower(strings.TrimSpace(name))
	d, ok := pragmas[key]
	if !ok {
		return "", fmt.Errorf("doc: unknown PRAGMA %q", name)
	}
	if value == "" {
		return d.read(db), nil
	}
	if d.write == nil {
		return "", fmt.Errorf("doc: PRAGMA %s is %s and cannot be changed at runtime", key, d.scope)
	}
	if err := d.write(db, value); err != nil {
		return "", err
	}
	return d.read(db), nil
}

// PragmaNames returns the catalogued PRAGMA names in sorted order, the set the CLI
// lists when asked for all of them.
func PragmaNames() []string {
	names := make([]string, 0, len(pragmas))
	for n := range pragmas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isolationDefault returns the database's default session isolation under the lock.
func (db *DB) isolationDefault() isolation {
	db.pragMu.Lock()
	defer db.pragMu.Unlock()
	return db.defaultIso
}

// setIsolationDefault records the default session isolation under the lock.
func (db *DB) setIsolationDefault(i isolation) {
	db.pragMu.Lock()
	defer db.pragMu.Unlock()
	db.defaultIso = i
}

// syncName renders a pager sync level as the synchronous PRAGMA spelling.
func syncName(l pager.SyncLevel) string {
	switch l {
	case pager.SyncOff:
		return "off"
	case pager.SyncFull:
		return "full"
	default:
		return "normal"
	}
}

// parseSyncName maps a synchronous PRAGMA value onto a pager sync level. The engine
// has no separate EXTRA barrier, so extra folds onto full (the strongest level it
// implements), matching the CLI's --sync handling.
func parseSyncName(v string) (pager.SyncLevel, bool) {
	switch strings.ToLower(v) {
	case "off", "0":
		return pager.SyncOff, true
	case "normal", "1":
		return pager.SyncNormal, true
	case "full", "2", "extra", "3":
		return pager.SyncFull, true
	default:
		return pager.SyncOff, false
	}
}
