package doc

import (
	"context"
	"errors"
	"sort"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/engine"
	"github.com/tamnd/doc/pager"
)

// mapEngineErr translates the lower-layer sentinels into the public error
// taxonomy so callers match on doc's names regardless of which internal package
// raised the failure (spec 2061 doc 14 §17).
func mapEngineErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrNamespaceExists):
		return ErrNamespaceExists
	case errors.Is(err, engine.ErrNamespaceNotFound):
		return ErrNamespaceNotFound
	case errors.Is(err, pager.ErrReadOnly):
		return ErrReadOnly
	case errors.Is(err, collection.ErrDuplicateKey):
		return duplicateKeyException(0, err)
	default:
		return err
	}
}

// Database returns a handle to the named logical database. The call performs no
// I/O; the namespace is created on first write or on explicit CreateCollection.
func (db *DB) Database(name string) *Database {
	return &Database{db: db, name: name}
}

// ListDatabaseNames returns the names of all databases, filtered by the given
// document. Only the empty filter is honored today; a non-empty filter returns
// every name and is reserved for future predicate support.
func (db *DB) ListDatabaseNames(ctx context.Context, filter any, opts ...any) ([]string, error) {
	if err := db.check(ctx); err != nil {
		return nil, err
	}
	names := db.eng.ListDatabases()
	sort.Strings(names)
	return names, nil
}

// ListDatabases returns structured information about each database.
func (db *DB) ListDatabases(ctx context.Context, filter any, opts ...any) (ListDatabasesResult, error) {
	names, err := db.ListDatabaseNames(ctx, filter, opts...)
	if err != nil {
		return ListDatabasesResult{}, err
	}
	res := ListDatabasesResult{Databases: make([]DatabaseSpecification, 0, len(names))}
	for _, n := range names {
		empty := len(db.eng.ListCollections(n)) == 0
		res.Databases = append(res.Databases, DatabaseSpecification{Name: n, Empty: empty})
	}
	return res, nil
}

// check reports the first applicable error before an operation runs: a cancelled
// context or a closed database.
func (db *DB) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if db.isClosed() {
		return ErrClosed
	}
	return nil
}
