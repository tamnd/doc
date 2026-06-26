package doc

import (
	"context"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/options"
)

// IndexModel describes a secondary index to create: a key specification document
// and optional index options (spec 2061 doc 14 §12.1).
type IndexModel struct {
	Keys    any
	Options *options.IndexOptions
}

// IndexSpecification is the resolved description of a created index, returned by
// ListSpecifications.
type IndexSpecification struct {
	Name               string
	Namespace          string
	KeysDocument       Raw
	Unique             bool
	Sparse             bool
	ExpireAfterSeconds *int32
}

// IndexView is the handle for managing one collection's indexes. Obtain it from
// Collection.Indexes (spec 2061 doc 14 §12).
type IndexView struct {
	coll *Collection
}

// keyPartsFromDoc parses an index key specification (a D, M, or struct) into the
// engine's ordered key parts. A negative numeric direction is descending; a
// string direction such as "text" is treated as ascending order over the field.
func keyPartsFromDoc(keys any) ([]catalog.KeyPart, error) {
	if keys == nil {
		return nil, ErrNilDocument
	}
	raw, err := Marshal(keys)
	if err != nil {
		return nil, err
	}
	elems, err := bson.Raw(raw).Elements()
	if err != nil {
		return nil, err
	}
	parts := make([]catalog.KeyPart, 0, len(elems))
	for _, e := range elems {
		desc := false
		switch e.Value.Type {
		case bson.TypeInt32:
			desc = e.Value.Int32() < 0
		case bson.TypeInt64:
			desc = e.Value.Int64() < 0
		case bson.TypeDouble:
			desc = e.Value.Double() < 0
		}
		parts = append(parts, catalog.KeyPart{Field: e.Key, Desc: desc})
	}
	return parts, nil
}

// toEngineModel converts a public IndexModel into the engine's IndexModel.
func toEngineModel(m IndexModel) (collection.IndexModel, error) {
	parts, err := keyPartsFromDoc(m.Keys)
	if err != nil {
		return collection.IndexModel{}, err
	}
	em := collection.IndexModel{Key: parts}
	if o := m.Options; o != nil {
		if o.Name != nil {
			em.Name = *o.Name
		}
		if o.Unique != nil {
			em.Unique = *o.Unique
		}
		if o.Sparse != nil {
			em.Sparse = *o.Sparse
		}
		if o.ExpireAfterSeconds != nil {
			em.ExpireAfterSeconds = int64(*o.ExpireAfterSeconds)
		}
		if o.PartialFilterExpression != nil {
			pf, perr := Marshal(o.PartialFilterExpression)
			if perr != nil {
				return collection.IndexModel{}, perr
			}
			em.PartialFilter = bson.Raw(pf)
		}
	}
	return em, nil
}

// CreateOne builds one index and returns its name.
func (iv IndexView) CreateOne(ctx context.Context, model IndexModel, opts ...*options.CreateIndexesOptions) (string, error) {
	if err := iv.coll.db.check(ctx); err != nil {
		return "", err
	}
	defer iv.coll.observe("createIndex")(0, 0, 0)
	em, err := toEngineModel(model)
	if err != nil {
		return "", err
	}
	col, err := iv.coll.writable()
	if err != nil {
		return "", err
	}
	name, err := col.CreateIndex(em)
	return name, mapEngineErr(err)
}

// CreateMany builds several indexes and returns their names in order.
func (iv IndexView) CreateMany(ctx context.Context, models []IndexModel, opts ...*options.CreateIndexesOptions) ([]string, error) {
	if err := iv.coll.db.check(ctx); err != nil {
		return nil, err
	}
	defer iv.coll.observe("createIndex")(0, 0, 0)
	ems := make([]collection.IndexModel, len(models))
	for i, m := range models {
		em, err := toEngineModel(m)
		if err != nil {
			return nil, err
		}
		ems[i] = em
	}
	col, err := iv.coll.writable()
	if err != nil {
		return nil, err
	}
	names, err := col.CreateIndexes(ems)
	return names, mapEngineErr(err)
}

// DropOne drops the named index. The returned Raw is reserved for the command
// reply and is currently empty.
func (iv IndexView) DropOne(ctx context.Context, name string, opts ...*options.ListIndexesOptions) (Raw, error) {
	if err := iv.coll.db.check(ctx); err != nil {
		return nil, err
	}
	defer iv.coll.observe("dropIndex")(0, 0, 0)
	col := iv.coll.readable()
	if col == nil {
		return nil, ErrNamespaceNotFound
	}
	return nil, mapEngineErr(col.DropIndex(name))
}

// DropAll drops every secondary index, leaving the _id index in place.
func (iv IndexView) DropAll(ctx context.Context, opts ...*options.ListIndexesOptions) (Raw, error) {
	if err := iv.coll.db.check(ctx); err != nil {
		return nil, err
	}
	defer iv.coll.observe("dropIndex")(0, 0, 0)
	col := iv.coll.readable()
	if col == nil {
		return nil, nil
	}
	return nil, mapEngineErr(col.DropAllIndexes())
}

// indexInfoToSpec converts an engine IndexInfo into a public specification.
func (iv IndexView) indexInfoToSpec(info collection.IndexInfo) *IndexSpecification {
	b := bson.NewBuilder()
	for _, p := range info.Key {
		dir := int32(1)
		if p.Desc {
			dir = -1
		}
		b.AppendInt32(p.Field, dir)
	}
	spec := &IndexSpecification{
		Name:         info.Name,
		Namespace:    iv.coll.dbName + "." + iv.coll.name,
		KeysDocument: Raw(b.Build()),
		Unique:       info.Unique,
		Sparse:       info.Sparse,
	}
	if info.ExpireAfterSeconds > 0 {
		secs := int32(info.ExpireAfterSeconds)
		spec.ExpireAfterSeconds = &secs
	}
	return spec
}

// ListSpecifications returns the resolved specification of every index.
func (iv IndexView) ListSpecifications(ctx context.Context, opts ...*options.ListIndexesOptions) ([]*IndexSpecification, error) {
	if err := iv.coll.db.check(ctx); err != nil {
		return nil, err
	}
	col := iv.coll.readable()
	if col == nil {
		return nil, nil
	}
	infos := col.ListIndexes()
	specs := make([]*IndexSpecification, 0, len(infos))
	for _, info := range infos {
		specs = append(specs, iv.indexInfoToSpec(info))
	}
	return specs, nil
}

// List returns a cursor over one document per index, each carrying the index
// name and key specification.
func (iv IndexView) List(ctx context.Context, opts ...*options.ListIndexesOptions) (*Cursor, error) {
	specs, err := iv.ListSpecifications(ctx, opts...)
	if err != nil {
		return nil, err
	}
	docs := make([]bson.Raw, 0, len(specs))
	for _, s := range specs {
		b := bson.NewBuilder().
			AppendInt32("v", 2).
			AppendDocument("key", bson.Raw(s.KeysDocument)).
			AppendString("name", s.Name)
		if s.Unique {
			b.AppendBoolean("unique", true)
		}
		if s.Sparse {
			b.AppendBoolean("sparse", true)
		}
		docs = append(docs, b.Build())
	}
	return newCursor(docs), nil
}
