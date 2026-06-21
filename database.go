package doc

import (
	"context"
	"sort"

	"github.com/tamnd/doc/bson"
)

// Database is a handle to one logical namespace inside a .doc file. It is a
// cheap value carrying only the owning DB and a name; creating one performs no
// I/O (spec 2061 doc 14 §3).
type Database struct {
	db   *DB
	name string
}

// Name returns the database name.
func (d *Database) Name() string { return d.name }

// Client returns the owning DB.
func (d *Database) Client() *DB { return d.db }

// Collection returns a handle to the named collection. The namespace is created
// on first write or on an explicit CreateCollection.
func (d *Database) Collection(name string) *Collection {
	return &Collection{db: d.db, dbName: d.name, name: name}
}

// CreateCollection explicitly creates a collection. Calling it is optional, but
// it is the way to set non-default options. The capped, validator, and
// time-series options are accepted and recorded by later milestones; this
// milestone creates the namespace itself.
func (d *Database) CreateCollection(ctx context.Context, name string, opts ...any) error {
	if err := d.db.check(ctx); err != nil {
		return err
	}
	_, err := d.db.eng.CreateCollection(d.name, name)
	return mapEngineErr(err)
}

// Drop removes the database and every collection in it.
func (d *Database) Drop(ctx context.Context) error {
	if err := d.db.check(ctx); err != nil {
		return err
	}
	return mapEngineErr(d.db.eng.DropDatabase(d.name))
}

// ListCollectionNames returns the names of the collections in the database. Only
// the empty filter is honored today.
func (d *Database) ListCollectionNames(ctx context.Context, filter any, opts ...any) ([]string, error) {
	if err := d.db.check(ctx); err != nil {
		return nil, err
	}
	names := d.db.eng.ListCollections(d.name)
	sort.Strings(names)
	return names, nil
}

// ListCollections returns a cursor over one document per collection, each of the
// form {name, type}.
func (d *Database) ListCollections(ctx context.Context, filter any, opts ...any) (*Cursor, error) {
	names, err := d.ListCollectionNames(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	docs := make([]bson.Raw, 0, len(names))
	for _, n := range names {
		docs = append(docs, bson.NewBuilder().
			AppendString("name", n).
			AppendString("type", "collection").
			Build())
	}
	return newCursor(docs), nil
}
