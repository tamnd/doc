package doc

import (
	"context"
	"errors"
	"fmt"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/options"
)

// Session drives multi-document, multi-collection transactions, mirroring the
// MongoDB ClientSession surface (spec 2061 doc 14 §14). A session holds at most one
// active transaction. Operations join the transaction when they run on a context
// carrying the session, obtained with NewSessionContext or supplied by
// WithTransaction. Always call EndSession when done.
type Session interface {
	// StartTransaction opens a transaction on the session.
	StartTransaction(opts ...*options.TransactionOptions) error
	// CommitTransaction commits the active transaction, making its writes durable
	// and visible across every collection it touched.
	CommitTransaction(ctx context.Context) error
	// AbortTransaction discards the active transaction's buffered writes.
	AbortTransaction(ctx context.Context) error
	// WithTransaction runs fn inside a transaction, committing on success and
	// retrying on a transient conflict. fn must be idempotent.
	WithTransaction(ctx context.Context, fn func(ctx context.Context) (any, error), opts ...*options.TransactionOptions) (any, error)
	// EndSession releases the session, aborting any transaction still open.
	EndSession(ctx context.Context)
	// ID returns the session's logical identifier.
	ID() M
}

// session is the concrete Session. It binds the public layer to one
// engine-coordinated MultiTxn at a time.
type session struct {
	db    *DB
	multi *collection.MultiTxn
	id    ObjectID
	ended bool
}

// sessionRetries bounds WithTransaction's automatic retries on transient conflicts,
// matching the collection layer's default budget.
const sessionRetries = 3

// StartSession opens a session over the database. The returned session starts with
// no active transaction; call StartTransaction or WithTransaction to begin one.
func (db *DB) StartSession(opts ...*options.SessionOptions) (Session, error) {
	if db.isClosed() {
		return nil, ErrClosed
	}
	return &session{db: db, id: NewObjectID()}, nil
}

// ID returns the session's logical session id document, shaped like MongoDB's lsid.
func (s *session) ID() M { return M{"id": s.id} }

// StartTransaction opens a transaction on the session. It is an error to start a
// second transaction while one is already in progress.
func (s *session) StartTransaction(opts ...*options.TransactionOptions) error {
	if s.ended {
		return ErrClientDisconnected
	}
	if s.multi != nil {
		return errors.New("doc: a transaction is already in progress on this session")
	}
	if s.db.isClosed() {
		return ErrClosed
	}
	s.multi = s.db.eng.Begin(s.db.isolationDefault().level())
	return nil
}

// CommitTransaction commits the active transaction. A transient conflict returns a
// retriable error and leaves nothing committed.
func (s *session) CommitTransaction(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.multi == nil {
		return errors.New("doc: no transaction in progress on this session")
	}
	err := s.multi.Commit()
	s.multi = nil
	return mapTxnErr(err)
}

// AbortTransaction discards the active transaction's buffered writes.
func (s *session) AbortTransaction(ctx context.Context) error {
	if s.multi == nil {
		return errors.New("doc: no transaction in progress on this session")
	}
	err := s.multi.Rollback()
	s.multi = nil
	return err
}

// WithTransaction runs fn inside a transaction and retries it on a transient
// conflict up to the session retry budget. The closure receives a context bound to
// the session, so every operation it issues on that context participates in the
// transaction. Because the closure may run more than once, it must be idempotent.
func (s *session) WithTransaction(ctx context.Context, fn func(ctx context.Context) (any, error), opts ...*options.TransactionOptions) (any, error) {
	if s.ended {
		return nil, ErrClientDisconnected
	}
	var lastErr error
	for range sessionRetries {
		if err := s.StartTransaction(opts...); err != nil {
			return nil, err
		}
		sctx := NewSessionContext(ctx, s)
		res, err := fn(sctx)
		if err != nil {
			_ = s.AbortTransaction(ctx)
			if collection.IsRetriable(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		cErr := s.CommitTransaction(ctx)
		if cErr == nil {
			return res, nil
		}
		if !collection.IsRetriable(cErr) {
			return nil, cErr
		}
		lastErr = cErr
	}
	return nil, fmt.Errorf("doc: transaction retry budget exhausted: %w", lastErr)
}

// EndSession releases the session, aborting any transaction still open. It is safe
// to call more than once.
func (s *session) EndSession(ctx context.Context) {
	if s.multi != nil {
		_ = s.multi.Rollback()
		s.multi = nil
	}
	s.ended = true
}

// sessionKey is the context key under which a bound session travels.
type sessionKey struct{}

// NewSessionContext returns a context that carries sess, so operations run on it
// join the session's active transaction (spec 2061 doc 14 §14.2).
func NewSessionContext(ctx context.Context, sess Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// sessionFromContext returns the session bound to ctx, or nil when none is.
func sessionFromContext(ctx context.Context) *session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sessionKey{}).(*session)
	return s
}

// mapTxnErr translates a collection-layer commit error into the public form. A
// write conflict becomes a CommandError tagged TransientTransactionError so callers
// running their own retry loop can recognize it (spec 2061 doc 14 §14.5).
func mapTxnErr(err error) error {
	if err == nil {
		return nil
	}
	if collection.IsRetriable(err) {
		return CommandError{
			Code:    codeWriteConflict,
			Message: err.Error(),
			Name:    "WriteConflict",
			Labels:  []string{"TransientTransactionError"},
		}
	}
	return mapEngineErr(err)
}
