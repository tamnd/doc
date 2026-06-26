package wire

import (
	"context"
	"errors"
	"strconv"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// findBatchDefault is the first-batch size a find uses when the command does not set
// batchSize, matching MongoDB's 101 (spec 2061 doc 16 §7.2).
const findBatchDefault = 101

// maxBatchBytes caps a single cursor batch at 16 MiB so one getMore reply stays within
// the document size ceiling (spec 2061 doc 16 §7.2). At least one document always goes
// into a batch even if it alone is larger.
const maxBatchBytes = 16 * 1024 * 1024

// dispatchData handles the data-path commands that need cursor state or document
// sequences: find, getMore, killCursors, insert, update, delete, findAndModify, and
// aggregate. Everything else (count, distinct, listCollections, DDL, ...) is left to
// RunCommand, so dispatchData reports not-handled for those (spec 2061 doc 16 §6).
func (c *conn) dispatchData(ctx context.Context, dbName, name string, in *opMsgIn) (bson.Raw, bool) {
	switch name {
	case "find":
		return c.handleFind(ctx, dbName, in.body), true
	case "getmore":
		return c.handleGetMore(ctx, dbName, in.body), true
	case "killcursors":
		return c.handleKillCursors(ctx, in.body), true
	case "insert":
		return c.handleInsert(ctx, dbName, in), true
	case "update":
		return c.handleUpdate(ctx, dbName, in), true
	case "delete":
		return c.handleDelete(ctx, dbName, in), true
	case "findandmodify":
		return c.handleFindAndModify(ctx, dbName, in.body), true
	case "aggregate":
		raw, handled := c.handleAggregate(ctx, dbName, in.body)
		return raw, handled
	default:
		return nil, false
	}
}

// collectionOf reads the collection name a command targets from its first field, which
// carries the value by convention (find: <coll>, insert: <coll>, and so on).
func collectionOf(body bson.Raw) string {
	if v, ok := body.Lookup(firstKey(body)); ok && v.Type == bson.TypeString {
		return v.StringValue()
	}
	return ""
}

// namespace renders the db.collection string a cursor reply echoes.
func namespace(db, coll string) string { return db + "." + coll }

// handleFind runs a query and builds the first cursor batch. When the result set
// outlasts the first batch the cursor is registered and its id returned, pinned to this
// connection so only this connection's getMore can advance it (spec 2061 doc 16 §6.4).
func (c *conn) handleFind(ctx context.Context, dbName string, body bson.Raw) bson.Raw {
	coll := collectionOf(body)
	ns := namespace(dbName, coll)

	filter := documentField(body, "filter")
	opt := options.Find()
	if v := documentField(body, "projection"); v != nil {
		opt.SetProjection(v)
	}
	if v := documentField(body, "sort"); v != nil {
		opt.SetSort(v)
	}
	if n, ok := lookupInt64(body, "skip"); ok {
		opt.SetSkip(n)
	}
	if n, ok := lookupInt64(body, "limit"); ok && n > 0 {
		opt.SetLimit(n)
	}

	cur, err := c.collection(dbName, coll).Find(ctx, filter, opt)
	if err != nil {
		return errorReplyFrom(err)
	}

	batchSize := findBatchDefault
	if n, ok := lookupInt64(body, "batchSize"); ok && n >= 0 {
		batchSize = int(n)
	}
	singleBatch := lookupBool(body, "singleBatch")
	return c.firstBatch(ctx, ns, cur, batchSize, singleBatch)
}

// handleGetMore advances an open cursor. A missing cursor, or one owned by another
// connection, returns CursorNotFound (code 43); when the cursor drains it is removed and
// its id reported as 0 (spec 2061 doc 16 §6.4, §7.4).
func (c *conn) handleGetMore(ctx context.Context, dbName string, body bson.Raw) bson.Raw {
	id, _ := lookupInt64(body, "getMore")
	coll := lookupString(body, "collection")
	ns := namespace(dbName, coll)

	sc, ok := c.srv.cursors.get(c.id, c.id, id)
	if !ok {
		return errorDoc(43, "CursorNotFound", "cursor id "+strconv.FormatInt(id, 10)+" not found")
	}

	batchSize := 0 // 0 means drain whatever remains
	if n, ok := lookupInt64(body, "batchSize"); ok && n > 0 {
		batchSize = int(n)
	}
	docs, hasMore := collectBatch(ctx, sc.cur, batchSize)
	if err := sc.cur.Err(); err != nil {
		c.dropCursor(ctx, id)
		return errorReplyFrom(err)
	}
	replyID := id
	if !hasMore {
		c.dropCursor(ctx, id)
		replyID = 0
	}
	return cursorBatchReply(replyID, ns, "nextBatch", docs)
}

// handleKillCursors closes the named cursors and reports which were found and which were
// not, the shape a driver expects so it stops tracking them (spec 2061 doc 16 §6.4).
func (c *conn) handleKillCursors(ctx context.Context, body bson.Raw) bson.Raw {
	var killed, notFound []int64
	if v, ok := body.Lookup("cursors"); ok && v.Type == bson.TypeArray {
		for _, e := range arrayElements(v) {
			id, ok := rawValueInt64(e)
			if !ok {
				continue
			}
			if sc, ok := c.srv.cursors.remove(c.id, id); ok {
				_ = sc.cur.Close(ctx)
				killed = append(killed, id)
			} else {
				notFound = append(notFound, id)
			}
		}
	}

	return bson.NewBuilder().
		AppendArray("cursorsKilled", int64Array(killed)).
		AppendArray("cursorsNotFound", int64Array(notFound)).
		AppendArray("cursorsAlive", bson.BuildArray()).
		AppendArray("cursorsUnknown", bson.BuildArray()).
		AppendDouble("ok", 1).
		Build()
}

// handleInsert inserts the batch carried in the body "documents" array plus any kind-1
// "documents" sequence. The reply reports the inserted count and any per-document write
// errors (spec 2061 doc 16 §6.5).
func (c *conn) handleInsert(ctx context.Context, dbName string, in *opMsgIn) bson.Raw {
	if reply := c.readOnlyGuard(); reply != nil {
		return reply
	}
	coll := collectionOf(in.body)
	docs := mergeDocs(in, "documents")
	if len(docs) == 0 {
		return writeReply(0, nil)
	}
	ordered := orderedFlag(in.body)

	models := make([]any, len(docs))
	for i, d := range docs {
		models[i] = d
	}
	res, err := c.collection(dbName, coll).InsertMany(ctx, models, options.InsertMany().SetOrdered(ordered))
	n := 0
	if res != nil {
		n = len(res.InsertedIDs)
	}
	return writeReply(n, writeErrorsFrom(err))
}

// handleUpdate applies the updates carried in the body "updates" array plus any kind-1
// "updates" sequence through a bulk write. The reply reports matched and modified counts
// and any upserted ids (spec 2061 doc 16 §6.5).
func (c *conn) handleUpdate(ctx context.Context, dbName string, in *opMsgIn) bson.Raw {
	if reply := c.readOnlyGuard(); reply != nil {
		return reply
	}
	coll := collectionOf(in.body)
	statements := mergeDocs(in, "updates")
	ordered := orderedFlag(in.body)

	models := make([]doc.WriteModel, 0, len(statements))
	for _, s := range statements {
		filter := documentField(s, "q")
		update := updateField(s, "u")
		if update == nil {
			continue
		}
		upsert := lookupBool(s, "upsert")
		if lookupBool(s, "multi") {
			m := doc.NewUpdateManyModel().SetFilter(filter).SetUpdate(update)
			if upsert {
				m.SetUpsert(true)
			}
			models = append(models, m)
			continue
		}
		// A replacement (no update operators) routes through ReplaceOne so the engine
		// treats it as a whole-document swap rather than an operator update.
		if isReplacement(update) {
			m := doc.NewReplaceOneModel().SetFilter(filter).SetReplacement(update)
			if upsert {
				m.SetUpsert(true)
			}
			models = append(models, m)
			continue
		}
		m := doc.NewUpdateOneModel().SetFilter(filter).SetUpdate(update)
		if upsert {
			m.SetUpsert(true)
		}
		models = append(models, m)
	}

	res, err := c.collection(dbName, coll).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(ordered))
	b := bson.NewBuilder()
	if res != nil {
		b.AppendInt32("n", int32(res.MatchedCount+res.UpsertedCount))
		b.AppendInt32("nModified", int32(res.ModifiedCount))
		if len(res.UpsertedIDs) > 0 {
			b.AppendArray("upserted", upsertedArray(res.UpsertedIDs))
		}
	} else {
		b.AppendInt32("n", 0)
		b.AppendInt32("nModified", 0)
	}
	if we := writeErrorsFrom(err); we != nil {
		b.AppendArray("writeErrors", we)
	}
	b.AppendDouble("ok", 1)
	return b.Build()
}

// handleDelete applies the deletes carried in the body "deletes" array plus any kind-1
// "deletes" sequence. A statement limit of 0 deletes all matches, 1 deletes the first
// match (spec 2061 doc 16 §6.5).
func (c *conn) handleDelete(ctx context.Context, dbName string, in *opMsgIn) bson.Raw {
	if reply := c.readOnlyGuard(); reply != nil {
		return reply
	}
	coll := collectionOf(in.body)
	statements := mergeDocs(in, "deletes")
	ordered := orderedFlag(in.body)

	models := make([]doc.WriteModel, 0, len(statements))
	for _, s := range statements {
		filter := documentField(s, "q")
		limit, _ := lookupInt64(s, "limit")
		if limit == 0 {
			models = append(models, doc.NewDeleteManyModel().SetFilter(filter))
		} else {
			models = append(models, doc.NewDeleteOneModel().SetFilter(filter))
		}
	}

	res, err := c.collection(dbName, coll).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(ordered))
	n := 0
	if res != nil {
		n = int(res.DeletedCount)
	}
	return writeReply(n, writeErrorsFrom(err))
}

// handleFindAndModify atomically updates, replaces, or removes one document and returns
// the before or after image with a lastErrorObject (spec 2061 doc 16 §6.5).
func (c *conn) handleFindAndModify(ctx context.Context, dbName string, body bson.Raw) bson.Raw {
	if reply := c.readOnlyGuard(); reply != nil {
		return reply
	}
	coll := collectionOf(body)
	filter := documentField(body, "query")
	col := c.collection(dbName, coll)

	var res *doc.SingleResult
	remove := lookupBool(body, "remove")
	switch {
	case remove:
		opt := options.FindOneAndDelete()
		if v := documentField(body, "sort"); v != nil {
			opt.SetSort(v)
		}
		if v := documentField(body, "fields"); v != nil {
			opt.SetProjection(v)
		}
		res = col.FindOneAndDelete(ctx, filter, opt)
	default:
		update := updateField(body, "update")
		after := lookupBool(body, "new")
		upsert := lookupBool(body, "upsert")
		if isReplacement(update) {
			opt := options.FindOneAndReplace().SetUpsert(upsert)
			if v := documentField(body, "sort"); v != nil {
				opt.SetSort(v)
			}
			if v := documentField(body, "fields"); v != nil {
				opt.SetProjection(v)
			}
			if after {
				opt.SetReturnDocument(options.After)
			}
			res = col.FindOneAndReplace(ctx, filter, update, opt)
		} else {
			opt := options.FindOneAndUpdate().SetUpsert(upsert)
			if v := documentField(body, "sort"); v != nil {
				opt.SetSort(v)
			}
			if v := documentField(body, "fields"); v != nil {
				opt.SetProjection(v)
			}
			if after {
				opt.SetReturnDocument(options.After)
			}
			res = col.FindOneAndUpdate(ctx, filter, update, opt)
		}
	}

	value, err := res.Raw()
	if err != nil && !errors.Is(err, doc.ErrNoDocuments) {
		return errorReplyFrom(err)
	}
	leN := int32(0)
	if value != nil {
		leN = 1
	}
	lastError := bson.NewBuilder().
		AppendInt32("n", leN).
		AppendBoolean("updatedExisting", value != nil && !remove).
		Build()

	b := bson.NewBuilder().AppendDocument("lastErrorObject", lastError)
	if value != nil {
		b.AppendDocument("value", value)
	} else {
		b.AppendNull("value")
	}
	b.AppendDouble("ok", 1)
	return b.Build()
}

// handleAggregate runs a pipeline and returns its first cursor batch. A db-level
// aggregate (numeric target) is left to RunCommand by reporting not-handled.
func (c *conn) handleAggregate(ctx context.Context, dbName string, body bson.Raw) (bson.Raw, bool) {
	v, ok := body.Lookup("aggregate")
	if !ok || v.Type != bson.TypeString {
		return nil, false
	}
	coll := v.StringValue()
	ns := namespace(dbName, coll)

	var pipeline []bson.Raw
	if p, ok := body.Lookup("pipeline"); ok && p.Type == bson.TypeArray {
		for _, e := range arrayElements(p) {
			if e.Type == bson.TypeDocument {
				pipeline = append(pipeline, e.Document())
			}
		}
	}

	cur, err := c.collection(dbName, coll).Aggregate(ctx, pipelineDocs(pipeline))
	if err != nil {
		return errorReplyFrom(err), true
	}
	batchSize := findBatchDefault
	if n, ok := lookupInt64(body, "batchSize"); ok && n >= 0 {
		batchSize = int(n)
	}
	return c.firstBatch(ctx, ns, cur, batchSize, false), true
}

// firstBatch pulls the first batch off a fresh cursor and frames the find/aggregate
// cursor reply, registering the cursor when more documents remain.
func (c *conn) firstBatch(ctx context.Context, ns string, cur *doc.Cursor, batchSize int, singleBatch bool) bson.Raw {
	docs, hasMore := collectBatch(ctx, cur, batchSize)
	if err := cur.Err(); err != nil {
		_ = cur.Close(ctx)
		return errorReplyFrom(err)
	}
	var id int64
	if hasMore && !singleBatch {
		id = c.srv.cursors.register(c.id, ns, cur)
	} else {
		_ = cur.Close(ctx)
	}
	return cursorBatchReply(id, ns, "firstBatch", docs)
}

// collectBatch pulls up to batchSize documents off a cursor (0 means drain it) while
// keeping the batch under the byte cap, and reports whether more remain.
func collectBatch(ctx context.Context, cur *doc.Cursor, batchSize int) (docs []bson.Raw, hasMore bool) {
	bytes := 0
	for batchSize == 0 || len(docs) < batchSize {
		if !cur.Next(ctx) {
			return docs, false
		}
		d := cloneRaw(bson.Raw(cur.Current()))
		docs = append(docs, d)
		bytes += len(d)
		// Stop once the batch reaches the byte cap, but only after at least one document
		// so a single oversize document still makes progress.
		if bytes >= maxBatchBytes {
			return docs, cur.RemainingBatchLength() > 0
		}
	}
	return docs, cur.RemainingBatchLength() > 0
}

// dropCursor removes and closes a cursor by id.
func (c *conn) dropCursor(ctx context.Context, id int64) {
	if sc, ok := c.srv.cursors.remove(c.id, id); ok {
		_ = sc.cur.Close(ctx)
	}
}

// collection resolves a db.collection handle on the server's database.
func (c *conn) collection(dbName, coll string) *doc.Collection {
	return c.srv.db.Database(dbName).Collection(coll)
}

// readOnlyGuard returns an error reply when the server runs read-only and a write was
// attempted, else nil.
func (c *conn) readOnlyGuard() bson.Raw {
	if c.srv.opts.ReadOnly {
		return errorDoc(166, "IllegalOperation", "server is read-only")
	}
	return nil
}

// cursorBatchReply frames a {cursor: {id, ns, <field>: [...]}, ok: 1} reply.
func cursorBatchReply(id int64, ns, field string, docs []bson.Raw) bson.Raw {
	cursor := bson.NewBuilder().
		AppendInt64("id", id).
		AppendString("ns", ns).
		AppendArray(field, docArray(docs)).
		Build()
	return bson.NewBuilder().
		AppendDocument("cursor", cursor).
		AppendDouble("ok", 1).
		Build()
}

// writeReply frames the common insert/delete reply: {n, writeErrors?, ok: 1}.
func writeReply(n int, writeErrors bson.Raw) bson.Raw {
	b := bson.NewBuilder().AppendInt32("n", int32(n))
	if writeErrors != nil {
		b.AppendArray("writeErrors", writeErrors)
	}
	b.AppendDouble("ok", 1)
	return b.Build()
}

// writeErrorsFrom turns a WriteException into the writeErrors array a driver parses, or
// nil when err is not a write exception.
func writeErrorsFrom(err error) bson.Raw {
	if err == nil {
		return nil
	}
	var we doc.WriteException
	if !errors.As(err, &we) || len(we.WriteErrors) == 0 {
		return nil
	}
	b := bson.NewBuilder()
	for i, w := range we.WriteErrors {
		entry := bson.NewBuilder().
			AppendInt32("index", int32(w.Index)).
			AppendInt32("code", int32(w.Code)).
			AppendString("errmsg", w.Message).
			Build()
		b.AppendDocument(strconv.Itoa(i), entry)
	}
	return b.Build()
}

// upsertedArray renders the upserted ids of a bulk update as the [{index, _id}] array a
// driver expects, ordered by model index.
func upsertedArray(ids map[int64]any) bson.Raw {
	b := bson.NewBuilder()
	out := 0
	// Emit in ascending index order so the array is stable.
	for i := int64(0); out < len(ids); i++ {
		val, ok := ids[i]
		if !ok {
			continue
		}
		t, data, err := doc.MarshalValue(val)
		if err != nil {
			out++
			continue
		}
		entry := bson.NewBuilder().
			AppendInt64("index", i).
			AppendValue("_id", bson.RawValue{Type: t, Data: data}).
			Build()
		b.AppendDocument(strconv.Itoa(out), entry)
		out++
	}
	return b.Build()
}

// docArray builds a BSON array payload from a slice of documents, keyed by position.
func docArray(docs []bson.Raw) bson.Raw {
	b := bson.NewBuilder()
	for i, d := range docs {
		b.AppendDocument(strconv.Itoa(i), d)
	}
	return b.Build()
}

// int64Array builds a BSON array payload from a slice of int64 cursor ids.
func int64Array(vals []int64) bson.Raw {
	b := bson.NewBuilder()
	for i, v := range vals {
		b.AppendInt64(strconv.Itoa(i), v)
	}
	return b.Build()
}

// mergeDocs gathers a write batch from both the inline body array and the kind-1
// document sequence under the same identifier (spec 2061 doc 16 §6.5).
func mergeDocs(in *opMsgIn, field string) []bson.Raw {
	var out []bson.Raw
	if v, ok := in.body.Lookup(field); ok && v.Type == bson.TypeArray {
		for _, e := range arrayElements(v) {
			if e.Type == bson.TypeDocument {
				out = append(out, e.Document())
			}
		}
	}
	out = append(out, in.sequences[field]...)
	return out
}

// orderedFlag reads the ordered flag, defaulting to true as MongoDB does.
func orderedFlag(body bson.Raw) bool {
	if v, ok := body.Lookup("ordered"); ok && v.Type == bson.TypeBoolean {
		return v.Boolean()
	}
	return true
}

// documentField returns a sub-document field as a true nil interface when absent, so a
// missing filter reaches the library as untyped nil (match-all) rather than a typed-nil
// bson.Raw, which would marshal as an invalid empty document.
func documentField(d bson.Raw, key string) any {
	if v, ok := d.Lookup(key); ok && v.Type == bson.TypeDocument {
		return v.Document()
	}
	return nil
}

// updateField returns an update spec field, which may be an update document or an
// aggregation pipeline array. Both pass straight to the library, which accepts either.
func updateField(d bson.Raw, key string) bson.Raw {
	if v, ok := d.Lookup(key); ok && (v.Type == bson.TypeDocument || v.Type == bson.TypeArray) {
		return v.Document()
	}
	return nil
}

// isReplacement reports whether an update spec is a whole-document replacement rather
// than an operator update or pipeline. A replacement has no leading $ operator key.
func isReplacement(update bson.Raw) bool {
	if update == nil {
		return false
	}
	elems, err := update.Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	for _, e := range elems {
		if len(e.Key) > 0 && e.Key[0] == '$' {
			return false
		}
	}
	return true
}

// pipelineDocs adapts a slice of stage documents to the any the library Aggregate takes.
func pipelineDocs(stages []bson.Raw) any {
	out := make([]any, len(stages))
	for i, s := range stages {
		out[i] = s
	}
	return out
}

// arrayElements returns the values of a BSON array (a document keyed by position).
func arrayElements(v bson.RawValue) []bson.RawValue {
	if v.Type != bson.TypeArray && v.Type != bson.TypeDocument {
		return nil
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil
	}
	out := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		out[i] = e.Value
	}
	return out
}

// rawValueInt64 coerces a numeric BSON value to int64.
func rawValueInt64(v bson.RawValue) (int64, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return int64(v.Int32()), true
	case bson.TypeInt64:
		return v.Int64(), true
	case bson.TypeDouble:
		return int64(v.Double()), true
	default:
		return 0, false
	}
}

// lookupInt64 reads a numeric field as int64, accepting int32, int64, or double.
func lookupInt64(d bson.Raw, key string) (int64, bool) {
	if v, ok := d.Lookup(key); ok {
		return rawValueInt64(v)
	}
	return 0, false
}

// lookupBool reads a boolean field, defaulting to false.
func lookupBool(d bson.Raw, key string) bool {
	if v, ok := d.Lookup(key); ok && v.Type == bson.TypeBoolean {
		return v.Boolean()
	}
	return false
}

// cloneRaw copies a Raw so a batch keeps its bytes after the cursor advances.
func cloneRaw(r bson.Raw) bson.Raw {
	out := make([]byte, len(r))
	copy(out, r)
	return bson.Raw(out)
}
