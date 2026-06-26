package doc

import "context"

// WithSnapshot runs fn against a single consistent read snapshot of the database
// (spec 2061 doc 19 §20). Every read fn issues on the context it receives observes
// the same point in time, so a sequence of finds across collections cannot tear
// even while other writers commit concurrently. It is the read-side companion to
// WithTransaction: there is no conflict retry loop, because a snapshot read never
// conflicts.
//
// fn is expected to read. Writes it issues on the snapshot context do participate
// in the underlying transaction and are committed when fn returns nil, so a snapshot
// used for a read-modify-write behaves like a single non-retried transaction rather
// than silently dropping the write. If fn returns an error the snapshot is rolled
// back and the error is returned unchanged.
func (db *DB) WithSnapshot(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := db.check(ctx); err != nil {
		return err
	}
	sess, err := db.StartSession()
	if err != nil {
		return err
	}
	defer sess.EndSession(ctx)
	if err := sess.StartTransaction(); err != nil {
		return err
	}
	sctx := NewSessionContext(ctx, sess)
	if err := fn(sctx); err != nil {
		_ = sess.AbortTransaction(ctx)
		return err
	}
	return sess.CommitTransaction(ctx)
}
