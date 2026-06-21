package collection

import (
	"errors"
	"fmt"

	"github.com/tamnd/doc/mvcc"
	"github.com/tamnd/doc/storage"
)

// Session-lifecycle errors. A session drives at most one transaction at a time,
// mirroring the MongoDB ClientSession contract (spec 2061 doc 06 §7.2).
var (
	// ErrTxnInProgress reports StartTransaction while a transaction is already
	// open on the session.
	ErrTxnInProgress = errors.New("collection: a transaction is already in progress on this session")
	// ErrNoTxn reports CommitTransaction or AbortTransaction with no open
	// transaction.
	ErrNoTxn = errors.New("collection: no transaction in progress on this session")
	// ErrMaxRetriesExceeded reports that WithTransaction exhausted its retry budget
	// on retriable errors without committing.
	ErrMaxRetriesExceeded = errors.New("collection: transaction retry budget exhausted")
)

// WriteConflictError reports a first-committer-wins write-write conflict surfaced to
// a session caller: a concurrent transaction committed a write to one of this
// transaction's documents after its snapshot began. It is retriable (it unwraps to
// storage.ErrConflict), so WithTransaction retries it automatically. This is
// distinct from a DuplicateKeyError, which is not retriable because a retry would
// hit the same existing key (spec 2061 doc 06 §8.4, §8.6).
type WriteConflictError struct {
	// ConflictVer is the commit version of the transaction that won the race.
	ConflictVer uint64
}

func (e *WriteConflictError) Error() string {
	return fmt.Sprintf("collection: write conflict with commit version %d", e.ConflictVer)
}

// Unwrap lets errors.Is(err, storage.ErrConflict) match a WriteConflictError so the
// generic retriable check recognizes it.
func (e *WriteConflictError) Unwrap() error { return storage.ErrConflict }

// IsRetriable reports whether an error from a transaction body or commit is a
// transient conflict that a fresh-snapshot retry may resolve: a write-write
// conflict or a serialization failure (both unwrap to storage.ErrConflict). A
// duplicate-key violation is not retriable and returns false (spec 2061 doc 06
// §8.4, §10.5).
func IsRetriable(err error) bool {
	return err != nil && errors.Is(err, storage.ErrConflict)
}

// Session is the unit that drives multi-document transactions, matching the
// MongoDB ClientSession surface (spec 2061 doc 06 §7.2). It holds at most one
// active transaction; operations run against that transaction's snapshot with
// read-your-writes, and the writes become visible atomically at commit. M5 is one
// collection per file, so a session transacts over a single collection; the
// multi-collection database handle that lets one transaction span collections
// arrives with the catalog in M6 (spec 2061 doc 19 §22 M6).
type Session struct {
	c    *Collection
	txn  *Txn
	opts TransactionOptions
}

// StartSession opens a session over the collection. The default transaction options
// (snapshot isolation, w:1/j:true) apply unless overridden per transaction.
func (c *Collection) StartSession() *Session {
	return &Session{c: c, opts: TransactionOptions{WriteConcern: DefaultWriteConcern()}}
}

// resolveOpts merges a per-call options override (if any) over the session default.
func (s *Session) resolveOpts(opts []TransactionOptions) TransactionOptions {
	o := s.opts
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = defaultMaxRetries
	}
	return o
}

// StartTransaction opens a transaction on the session. Operations are issued through
// Transaction(); CommitTransaction or AbortTransaction ends it.
func (s *Session) StartTransaction(opts ...TransactionOptions) error {
	if s.txn != nil {
		return ErrTxnInProgress
	}
	s.txn = s.c.BeginTx(s.resolveOpts(opts))
	return nil
}

// Transaction returns the session's active transaction, or nil when none is open.
// The returned handle exposes the full read and write surface (InsertOne, FindOne,
// UpdateOne, and the rest), all reading and writing the transaction's snapshot.
func (s *Session) Transaction() *Txn { return s.txn }

// CommitTransaction commits the active transaction, making its writes durable and
// visible. A write-write conflict or serialization failure returns a retriable
// error and leaves nothing committed.
func (s *Session) CommitTransaction() error {
	if s.txn == nil {
		return ErrNoTxn
	}
	err := mapCommitErr(s.txn.Commit())
	s.txn = nil
	return err
}

// AbortTransaction discards the active transaction's buffered writes. Abort is free:
// nothing durable was written before commit (spec 2061 doc 06 §7.6).
func (s *Session) AbortTransaction() error {
	if s.txn == nil {
		return ErrNoTxn
	}
	err := s.txn.Rollback()
	s.txn = nil
	return err
}

// EndSession releases the session, aborting any transaction still open. It is safe
// to call more than once and is the deferred-cleanup counterpart to StartSession.
func (s *Session) EndSession() {
	if s.txn != nil {
		_ = s.txn.Rollback()
		s.txn = nil
	}
}

// WithTransaction runs fn inside a transaction, committing when fn returns nil and
// retrying automatically on a retriable error (a write conflict or serialization
// failure) up to the options' retry budget, following the MongoDB driver contract
// (spec 2061 doc 06 §7.2, §8.4). fn must be idempotent across retries: it may run
// more than once, each time against a fresh snapshot that includes the writes that
// caused the prior conflict. A non-retriable error from fn or commit is returned
// immediately with the transaction aborted.
func (s *Session) WithTransaction(fn func(*Txn) error, opts ...TransactionOptions) error {
	o := s.resolveOpts(opts)
	var lastErr error
	for attempt := 0; attempt < o.MaxRetries; attempt++ {
		t := s.c.BeginTx(o)
		s.txn = t
		if err := fn(t); err != nil {
			_ = t.Rollback()
			s.txn = nil
			if IsRetriable(err) {
				lastErr = err
				continue
			}
			return err
		}
		cErr := mapCommitErr(t.Commit())
		s.txn = nil
		if cErr == nil {
			return nil
		}
		if !IsRetriable(cErr) {
			return cErr
		}
		lastErr = cErr
	}
	return fmt.Errorf("%w: %w", ErrMaxRetriesExceeded, lastErr)
}

// WithTransaction is the one-shot convenience over a fresh session: it starts a
// session, runs fn in a retrying transaction, and ends the session. It is the path
// most callers take when they do not need to reuse a session across transactions.
func (c *Collection) WithTransaction(fn func(*Txn) error, opts ...TransactionOptions) error {
	s := c.StartSession()
	defer s.EndSession()
	return s.WithTransaction(fn, opts...)
}

// mapCommitErr translates a low-level commit error into the session-facing error
// shape: a first-committer-wins conflict from the oracle becomes a WriteConflictError
// carrying the winning commit version. Other errors pass through unchanged.
func mapCommitErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *mvcc.ConflictError
	if errors.As(err, &ce) {
		return &WriteConflictError{ConflictVer: ce.ConflictVer}
	}
	return err
}
