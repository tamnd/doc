package collection

import "time"

// IsolationLevel selects the isolation a transaction runs under. SnapshotIsolation
// is the default and is fully serializable on the single-writer path; Serializable
// adds rw-antidependency tracking so write skew is detected and aborted on the
// concurrent-writer path (spec 2061 doc 06 §10). Serializable is implemented in
// M5-b; under M5-a it behaves as snapshot isolation.
type IsolationLevel int

const (
	// SnapshotIsolation is the default: a transaction reads from the snapshot it
	// took at begin and write-write conflicts are detected first-committer-wins.
	SnapshotIsolation IsolationLevel = iota
	// Serializable adds SSI pivot detection on top of snapshot isolation.
	Serializable
)

// ReadConcern is the MongoDB read-concern knob. doc is single-node, so every level
// maps to the same snapshot read: there is one node, so "locally committed" and
// "majority committed" are the same state and committed data is never rolled back
// (spec 2061 doc 06 §9.2). The level is accepted for wire compatibility.
type ReadConcern string

const (
	ReadConcernLocal        ReadConcern = "local"
	ReadConcernMajority     ReadConcern = "majority"
	ReadConcernSnapshot     ReadConcern = "snapshot"
	ReadConcernLinearizable ReadConcern = "linearizable"
)

// WriteConcern is the MongoDB write-concern knob. At a single node w:1 and
// w:"majority" are the same (one node is a majority of one) and the default commit
// already fsyncs the WAL, so j:true is the default behavior. w:0 / j:false request
// the async-durability mode, which is governed by the pager's sync level chosen at
// Open; a per-transaction downgrade below that level is not honored, so a write
// always carries at least the file's configured durability (spec 2061 doc 06 §9.3).
type WriteConcern struct {
	W        int           // acknowledgment level: 1 = acknowledged (default), 0 = unacknowledged
	J        bool          // journal: wait for the WAL fsync (default true)
	WTimeout time.Duration // 0 = no timeout
}

// DefaultWriteConcern is w:1, j:true: the write is acknowledged only after it is
// committed and the WAL is synced.
func DefaultWriteConcern() WriteConcern { return WriteConcern{W: 1, J: true} }

// TransactionOptions configures a multi-document transaction. The zero value is a
// snapshot-isolation transaction with the default concerns and the default retry
// budget, which is what most callers want.
type TransactionOptions struct {
	ReadConcern  ReadConcern
	WriteConcern WriteConcern
	Isolation    IsolationLevel
	// MaxRetries bounds WithTransaction's automatic retry on retriable errors; 0
	// selects defaultMaxRetries.
	MaxRetries int
}

// defaultMaxRetries is WithTransaction's retry budget for retriable errors, matching
// the MongoDB driver convention (spec 2061 doc 06 §8.4).
const defaultMaxRetries = 3
