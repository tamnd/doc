package doc

import (
	"context"
	"sort"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/engine"
	"github.com/tamnd/doc/options"
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

// CreateCollection explicitly creates a collection. Calling it is optional for a
// regular collection (a write creates the namespace implicitly); it is the way to
// attach a validator or make the collection capped (spec 2061 doc 09 §8.2).
func (d *Database) CreateCollection(ctx context.Context, name string, opts ...*options.CreateCollectionOptions) error {
	if err := d.db.check(ctx); err != nil {
		return err
	}
	spec, err := createSpec(opts)
	if err != nil {
		return err
	}
	_, err = d.db.eng.CreateCollectionWith(d.name, name, spec)
	return mapEngineErr(err)
}

// createSpec folds the CreateCollection options into the engine's create spec,
// marshaling the validator document and mapping the validation level and action
// strings onto the catalog enums (spec 2061 doc 09 §10.3, §10.4).
func createSpec(opts []*options.CreateCollectionOptions) (engine.CreateSpec, error) {
	var spec engine.CreateSpec
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.Capped != nil {
			spec.Capped = *o.Capped
		}
		if o.SizeInBytes != nil {
			spec.SizeBytes = *o.SizeInBytes
		}
		if o.MaxDocuments != nil {
			spec.MaxDocs = *o.MaxDocuments
		}
		if o.Validator != nil {
			v, err := toDoc(o.Validator)
			if err != nil {
				return engine.CreateSpec{}, err
			}
			spec.Validator = v
		}
		if o.ValidationLevel != nil {
			spec.ValidationLevel = validationLevel(*o.ValidationLevel)
		}
		if o.ValidationAction != nil {
			spec.ValidationAction = validationAction(*o.ValidationAction)
		}
	}
	return spec, nil
}

// validationLevel maps the MongoDB level string onto the catalog enum, defaulting to
// strict for any unrecognized value (MongoDB's own default).
func validationLevel(s string) catalog.ValidationLevel {
	switch s {
	case "off":
		return catalog.ValidationOff
	case "moderate":
		return catalog.ValidationModerate
	default:
		return catalog.ValidationStrict
	}
}

// validationAction maps the MongoDB action string onto the catalog enum, defaulting
// to error.
func validationAction(s string) catalog.ValidationAction {
	if s == "warn" {
		return catalog.ValidationWarn
	}
	return catalog.ValidationError
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
