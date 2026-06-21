package doc

import (
	"context"
	"errors"
	"reflect"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/options"
)

// Collection is a handle to a bag of BSON documents. Like Database it is a cheap
// value safe for concurrent use; the underlying namespace is created on first
// write (spec 2061 doc 14 §3, §5).
type Collection struct {
	db     *DB
	dbName string
	name   string
}

// Name returns the collection name.
func (c *Collection) Name() string { return c.name }

// Database returns a handle to the collection's database.
func (c *Collection) Database() *Database { return &Database{db: c.db, name: c.dbName} }

// writable resolves the engine collection, creating the namespace if needed.
func (c *Collection) writable() (*collection.Collection, error) {
	col, err := c.db.eng.EnsureCollection(c.dbName, c.name)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return col, nil
}

// readable resolves the engine collection for a read. A missing namespace is not
// an error: it yields a nil collection, which the read paths treat as empty.
func (c *Collection) readable() *collection.Collection {
	return c.db.eng.GetCollection(c.dbName, c.name)
}

// crudExec is the read/write surface shared by an auto-commit engine collection and
// a session transaction's per-collection sub-handle. The public CRUD methods run
// against whichever the context selects: a *collection.Collection (each call its own
// transaction) when no session is bound, or a *collection.Txn that buffers into the
// session's multi-collection transaction when one is (spec 2061 doc 14 §14).
type crudExec interface {
	InsertOne(d bson.Raw) (bson.RawValue, error)
	InsertMany(docs []bson.Raw, ordered bool) (collection.InsertManyResult, error)
	FindWith(filter bson.Raw, opts collection.FindOptions) ([]bson.Raw, error)
	UpdateOneWith(filter, update bson.Raw, opts collection.UpdateOptions) (collection.UpdateResult, error)
	UpdateManyWith(filter, update bson.Raw, opts collection.UpdateOptions) (collection.UpdateResult, error)
	ReplaceOneWith(filter, replacement bson.Raw, opts collection.UpdateOptions) (collection.UpdateResult, error)
	DeleteOne(filter bson.Raw) (int64, error)
	DeleteMany(filter bson.Raw) (int64, error)
	CountDocuments(filter bson.Raw) (int64, error)
	Distinct(field string, filter bson.Raw) ([]bson.RawValue, error)
	Aggregate(pipeline []bson.Raw) ([]bson.Raw, error)
	FindOneAndUpdate(filter, update bson.Raw, opts collection.FindModifyOptions) (bson.Raw, error)
	FindOneAndReplace(filter, replacement bson.Raw, opts collection.FindModifyOptions) (bson.Raw, error)
	FindOneAndDelete(filter bson.Raw, opts collection.FindModifyOptions) (bson.Raw, error)
	BulkWrite(ops []collection.BulkOp, ordered bool) (collection.BulkWriteResult, error)
}

// writeExec resolves the executor for a write. It ensures the namespace exists,
// then routes through the session's transaction when one is bound to ctx.
func (c *Collection) writeExec(ctx context.Context) (crudExec, error) {
	col, err := c.writable()
	if err != nil {
		return nil, err
	}
	if s := sessionFromContext(ctx); s != nil && s.multi != nil {
		return s.multi.For(col), nil
	}
	return col, nil
}

// readExec resolves the executor for a read. A missing namespace yields nil, which
// the read paths treat as empty. When a session is bound, reads run on its
// transaction so they see the session's own buffered writes.
func (c *Collection) readExec(ctx context.Context) crudExec {
	col := c.readable()
	if col == nil {
		return nil
	}
	if s := sessionFromContext(ctx); s != nil && s.multi != nil {
		return s.multi.For(col)
	}
	return col
}

// toFilter marshals a filter value, mapping nil to the match-everything document.
func toFilter(filter any) (bson.Raw, error) {
	if filter == nil {
		return bson.NewBuilder().Build(), nil
	}
	raw, err := Marshal(filter)
	if err != nil {
		return nil, err
	}
	return bson.Raw(raw), nil
}

// toDoc marshals a non-nil document value.
func toDoc(v any) (bson.Raw, error) {
	if v == nil {
		return nil, ErrNilDocument
	}
	raw, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	return bson.Raw(raw), nil
}

// idValue decodes a stored _id RawValue into its natural Go form.
func idValue(rv bson.RawValue) any {
	v, err := decodeNatural(rv)
	if err != nil {
		return nil
	}
	return v
}

// InsertOne inserts one document and returns its _id. A missing _id is generated
// as a fresh ObjectID by the engine before storage.
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*InsertOneResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	defer c.observe("insert")(0, 0, 0)
	raw, err := toDoc(document)
	if err != nil {
		return nil, err
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return nil, err
	}
	id, err := col.InsertOne(raw)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return &InsertOneResult{InsertedID: idValue(id)}, nil
}

// InsertMany inserts a batch of documents. The default is ordered: the first
// failure stops the batch. options.InsertMany().SetOrdered(false) collects all
// errors and keeps successfully inserted documents.
func (c *Collection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*InsertManyResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, ErrEmptySlice
	}
	defer c.observe("insert")(0, 0, 0)
	raws := make([]bson.Raw, len(documents))
	for i, d := range documents {
		raw, err := toDoc(d)
		if err != nil {
			return nil, err
		}
		raws[i] = raw
	}
	ordered := true
	for _, o := range opts {
		if o != nil && o.Ordered != nil {
			ordered = *o.Ordered
		}
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return nil, err
	}
	res, err := col.InsertMany(raws, ordered)
	out := &InsertManyResult{InsertedIDs: make([]any, len(res.InsertedIDs))}
	for i, id := range res.InsertedIDs {
		out.InsertedIDs[i] = idValue(id)
	}
	if err != nil {
		return out, mapEngineErr(err)
	}
	return out, nil
}

// findOptionsToEngine folds the variadic FindOptions and marshals their document
// fields into the engine's FindOptions form.
func (c *Collection) findOptionsToEngine(opts []*options.FindOptions) (collection.FindOptions, error) {
	var fo collection.FindOptions
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.Projection != nil {
			p, err := Marshal(o.Projection)
			if err != nil {
				return fo, err
			}
			fo.Projection = bson.Raw(p)
		}
		if o.Sort != nil {
			s, err := Marshal(o.Sort)
			if err != nil {
				return fo, err
			}
			fo.Sort = bson.Raw(s)
		}
		if o.Skip != nil {
			fo.Skip = *o.Skip
		}
		if o.Limit != nil {
			fo.Limit = *o.Limit
		}
	}
	return fo, nil
}

// Find runs a query and returns a cursor over the matches.
func (c *Collection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*Cursor, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	rec := c.observe("find")
	var returned int64
	defer func() { rec(0, returned, 0) }()
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	col := c.readExec(ctx)
	if col == nil {
		return newCursor(nil), nil
	}
	fo, err := c.findOptionsToEngine(opts)
	if err != nil {
		return nil, err
	}
	docs, err := col.FindWith(f, fo)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	returned = int64(len(docs))
	return newCursor(docs), nil
}

// FindOne returns the first matching document as a SingleResult.
func (c *Collection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *SingleResult {
	if err := c.db.check(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	rec := c.observe("find")
	var returned int64
	defer func() { rec(0, returned, 0) }()
	f, err := toFilter(filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	col := c.readExec(ctx)
	if col == nil {
		return newSingleResult(nil, ErrNoDocuments)
	}
	fo := collection.FindOptions{Limit: 1}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.Projection != nil {
			p, perr := Marshal(o.Projection)
			if perr != nil {
				return newSingleResult(nil, perr)
			}
			fo.Projection = bson.Raw(p)
		}
		if o.Sort != nil {
			s, serr := Marshal(o.Sort)
			if serr != nil {
				return newSingleResult(nil, serr)
			}
			fo.Sort = bson.Raw(s)
		}
		if o.Skip != nil {
			fo.Skip = *o.Skip
		}
	}
	docs, err := col.FindWith(f, fo)
	if err != nil {
		return newSingleResult(nil, mapEngineErr(err))
	}
	if len(docs) == 0 {
		return newSingleResult(nil, ErrNoDocuments)
	}
	returned = 1
	return newSingleResult(docs[0], nil)
}

// updateResult converts an engine UpdateResult into the public form.
func updateResult(r collection.UpdateResult) *UpdateResult {
	out := &UpdateResult{
		MatchedCount:  r.Matched,
		ModifiedCount: r.Modified,
		UpsertedCount: r.Upserted,
	}
	if r.Upserted > 0 {
		out.UpsertedID = idValue(r.UpsertedID)
	}
	return out
}

func upsertFlag(opts ...*options.UpdateOptions) bool {
	upsert := false
	for _, o := range opts {
		if o != nil && o.Upsert != nil {
			upsert = *o.Upsert
		}
	}
	return upsert
}

// UpdateOne applies an update specification to the first matching document.
func (c *Collection) UpdateOne(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	return c.update(ctx, filter, update, false, opts...)
}

// UpdateMany applies an update specification to every matching document.
func (c *Collection) UpdateMany(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	return c.update(ctx, filter, update, true, opts...)
}

func (c *Collection) update(ctx context.Context, filter, update any, many bool, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	defer c.observe("update")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	u, err := toDoc(update)
	if err != nil {
		return nil, err
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return nil, err
	}
	eo := collection.UpdateOptions{Upsert: upsertFlag(opts...)}
	var res collection.UpdateResult
	if many {
		res, err = col.UpdateManyWith(f, u, eo)
	} else {
		res, err = col.UpdateOneWith(f, u, eo)
	}
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return updateResult(res), nil
}

// ReplaceOne replaces the whole document, except _id, with replacement.
func (c *Collection) ReplaceOne(ctx context.Context, filter, replacement any, opts ...*options.ReplaceOptions) (*UpdateResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	defer c.observe("update")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	r, err := toDoc(replacement)
	if err != nil {
		return nil, err
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return nil, err
	}
	upsert := false
	for _, o := range opts {
		if o != nil && o.Upsert != nil {
			upsert = *o.Upsert
		}
	}
	res, err := col.ReplaceOneWith(f, r, collection.UpdateOptions{Upsert: upsert})
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return updateResult(res), nil
}

// DeleteOne removes the first matching document.
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	return c.delete(ctx, filter, false)
}

// DeleteMany removes every matching document.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	return c.delete(ctx, filter, true)
}

func (c *Collection) delete(ctx context.Context, filter any, many bool) (*DeleteResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	defer c.observe("delete")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	col := c.readExec(ctx)
	if col == nil {
		return &DeleteResult{}, nil
	}
	var n int64
	if many {
		n, err = col.DeleteMany(f)
	} else {
		n, err = col.DeleteOne(f)
	}
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return &DeleteResult{DeletedCount: n}, nil
}

// CountDocuments returns the number of documents matching filter.
func (c *Collection) CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error) {
	if err := c.db.check(ctx); err != nil {
		return 0, err
	}
	defer c.observe("count")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return 0, err
	}
	col := c.readExec(ctx)
	if col == nil {
		return 0, nil
	}
	n, err := col.CountDocuments(f)
	return n, mapEngineErr(err)
}

// EstimatedDocumentCount returns a fast estimate of the total document count.
func (c *Collection) EstimatedDocumentCount(ctx context.Context, opts ...*options.EstimatedDocumentCountOptions) (int64, error) {
	if err := c.db.check(ctx); err != nil {
		return 0, err
	}
	defer c.observe("count")(0, 0, 0)
	col := c.readExec(ctx)
	if col == nil {
		return 0, nil
	}
	n, err := col.CountDocuments(bson.NewBuilder().Build())
	return n, mapEngineErr(err)
}

// Distinct returns the distinct values of fieldName among matching documents.
func (c *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]any, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	defer c.observe("distinct")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	col := c.readExec(ctx)
	if col == nil {
		return nil, nil
	}
	vals, err := col.Distinct(fieldName, f)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = idValue(v)
	}
	return out, nil
}

// Aggregate runs an aggregation pipeline and returns a cursor over the result.
func (c *Collection) Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*Cursor, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	rec := c.observe("aggregate")
	var returned int64
	defer func() { rec(0, returned, 0) }()
	stages, err := marshalPipeline(pipeline)
	if err != nil {
		return nil, err
	}
	col := c.readExec(ctx)
	if col == nil {
		return newCursor(nil), nil
	}
	docs, err := col.Aggregate(stages)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	returned = int64(len(docs))
	return newCursor(docs), nil
}

// Drop removes the collection and all its indexes. Dropping a collection that
// does not exist is a no-op, matching MongoDB.
func (c *Collection) Drop(ctx context.Context) error {
	if err := c.db.check(ctx); err != nil {
		return err
	}
	defer c.observe("drop")(0, 0, 0)
	err := mapEngineErr(c.db.eng.DropCollection(c.dbName, c.name))
	if errors.Is(err, ErrNamespaceNotFound) {
		return nil
	}
	return err
}

// Indexes returns the index management view for the collection.
func (c *Collection) Indexes() IndexView {
	return IndexView{coll: c}
}

// findModifyOptions folds the shared findAndModify fields out of the various
// option structs into the engine form.
type findModifyInput struct {
	sort       any
	projection any
	upsert     bool
	ret        options.ReturnDocument
}

func (c *Collection) toFindModify(in findModifyInput) (collection.FindModifyOptions, error) {
	var fo collection.FindModifyOptions
	if in.sort != nil {
		s, err := Marshal(in.sort)
		if err != nil {
			return fo, err
		}
		fo.Sort = bson.Raw(s)
	}
	if in.projection != nil {
		p, err := Marshal(in.projection)
		if err != nil {
			return fo, err
		}
		fo.Projection = bson.Raw(p)
	}
	fo.Upsert = in.upsert
	if in.ret == options.After {
		fo.Return = collection.ReturnAfter
	} else {
		fo.Return = collection.ReturnBefore
	}
	return fo, nil
}

// FindOneAndUpdate atomically updates the first matching document and returns
// either the before or after version per the options.
func (c *Collection) FindOneAndUpdate(ctx context.Context, filter, update any, opts ...*options.FindOneAndUpdateOptions) *SingleResult {
	if err := c.db.check(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	defer c.observe("findAndModify")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	u, err := toDoc(update)
	if err != nil {
		return newSingleResult(nil, err)
	}
	in := findModifyInput{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		in.sort, in.projection = pick(in.sort, o.Sort), pick(in.projection, o.Projection)
		if o.Upsert != nil {
			in.upsert = *o.Upsert
		}
		if o.ReturnDocument != nil {
			in.ret = *o.ReturnDocument
		}
	}
	fo, err := c.toFindModify(in)
	if err != nil {
		return newSingleResult(nil, err)
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return newSingleResult(nil, err)
	}
	res, err := col.FindOneAndUpdate(f, u, fo)
	return newSingleResult(res, mapEngineErr(err))
}

// FindOneAndReplace atomically replaces the first matching document.
func (c *Collection) FindOneAndReplace(ctx context.Context, filter, replacement any, opts ...*options.FindOneAndReplaceOptions) *SingleResult {
	if err := c.db.check(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	defer c.observe("findAndModify")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	r, err := toDoc(replacement)
	if err != nil {
		return newSingleResult(nil, err)
	}
	in := findModifyInput{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		in.sort, in.projection = pick(in.sort, o.Sort), pick(in.projection, o.Projection)
		if o.Upsert != nil {
			in.upsert = *o.Upsert
		}
		if o.ReturnDocument != nil {
			in.ret = *o.ReturnDocument
		}
	}
	fo, err := c.toFindModify(in)
	if err != nil {
		return newSingleResult(nil, err)
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return newSingleResult(nil, err)
	}
	res, err := col.FindOneAndReplace(f, r, fo)
	return newSingleResult(res, mapEngineErr(err))
}

// FindOneAndDelete atomically deletes the first matching document and returns it.
func (c *Collection) FindOneAndDelete(ctx context.Context, filter any, opts ...*options.FindOneAndDeleteOptions) *SingleResult {
	if err := c.db.check(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	defer c.observe("findAndModify")(0, 0, 0)
	f, err := toFilter(filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	in := findModifyInput{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		in.sort, in.projection = pick(in.sort, o.Sort), pick(in.projection, o.Projection)
	}
	fo, err := c.toFindModify(in)
	if err != nil {
		return newSingleResult(nil, err)
	}
	col := c.readExec(ctx)
	if col == nil {
		return newSingleResult(nil, ErrNoDocuments)
	}
	res, err := col.FindOneAndDelete(f, fo)
	return newSingleResult(res, mapEngineErr(err))
}

// pick returns override when it is non-nil, else cur. It lets later option
// values win during the left-to-right merge.
func pick(cur, override any) any {
	if override != nil {
		return override
	}
	return cur
}

// marshalPipeline marshals each stage of an aggregation pipeline to raw BSON.
// The pipeline may be a doc.A, []doc.M, []any, or any slice whose elements
// marshal to documents.
func marshalPipeline(pipeline any) ([]bson.Raw, error) {
	if pipeline == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(pipeline)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, errors.New("doc: aggregation pipeline must be a slice of stages")
	}
	stages := make([]bson.Raw, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		raw, err := Marshal(rv.Index(i).Interface())
		if err != nil {
			return nil, err
		}
		stages = append(stages, bson.Raw(raw))
	}
	return stages, nil
}
