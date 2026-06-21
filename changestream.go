package doc

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strconv"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
	"github.com/tamnd/doc/query"
)

// ErrChangeStreamResume is returned by ChangeStream.Next when the stream fell so far
// behind the in-memory feed that the events it needed were evicted. The caller has to
// open a fresh stream; the resume token it last held no longer points into the buffer
// (spec 2061 doc 14 §15).
var ErrChangeStreamResume = errors.New("doc: change stream cannot resume, events evicted")

// ErrChangeStreamInvalidated is the error a stream reports after an invalidate event,
// once the watched collection has been dropped and no further events can arrive.
var ErrChangeStreamInvalidated = errors.New("doc: change stream invalidated")

// Operation types reported by ChangeEvent.OperationType (spec 2061 doc 14 §15.2).
const (
	OperationInsert     = "insert"
	OperationUpdate     = "update"
	OperationReplace    = "replace"
	OperationDelete     = "delete"
	OperationDrop       = "drop"
	OperationInvalidate = "invalidate"
)

// Namespace is the database and collection an event came from (spec 2061 doc 14 §15.2).
type Namespace struct {
	DB         string `bson:"db"`
	Collection string `bson:"coll"`
}

// UpdateDescription carries the field-level diff of an update event: the top-level
// fields that changed value and the names of those that were removed (spec 2061 doc 14
// §15.2). It is nil on non-update events.
type UpdateDescription struct {
	UpdatedFields   M        `bson:"updatedFields"`
	RemovedFields   []string `bson:"removedFields"`
	TruncatedArrays []M      `bson:"truncatedArrays,omitempty"`
}

// ChangeEvent is one change delivered by a ChangeStream, the document a stream decodes
// into. Current returns it already populated; Decode unmarshals it into a caller value
// (spec 2061 doc 14 §15.3).
type ChangeEvent struct {
	ID                M                  `bson:"_id"` // resume token, {_data: "..."}
	OperationType     string             `bson:"operationType"`
	FullDocument      *M                 `bson:"fullDocument,omitempty"`
	Ns                Namespace          `bson:"ns"`
	DocumentKey       M                  `bson:"documentKey,omitempty"`
	UpdateDescription *UpdateDescription `bson:"updateDescription,omitempty"`
	ClusterTime       Timestamp          `bson:"clusterTime"`
	TxnNumber         *int64             `bson:"txnNumber,omitempty"`
}

// ChangeStream delivers change events from a watched scope. It is not safe for
// concurrent use; drive it from one goroutine with Next/Decode, the usual cursor
// shape. Close unregisters it from the feed (spec 2061 doc 14 §15.3).
type ChangeStream struct {
	db    *DB
	feed  *changeFeed
	sub   *feedSub
	scope Namespace // empty DB watches everything, empty Collection watches a database
	match *query.Matcher
	mode  options.FullDocument

	next    uint64   // last delivered sequence; the stream wants events with seq greater
	curRaw  bson.Raw // the current event as a BSON document, for Decode and $match
	cur     *ChangeEvent
	err     error
	done    bool
	invalid bool // a drop on the watched collection is pending its invalidate
}

// Watch opens a change stream over this collection. The pipeline may contain a single
// $match stage, applied to each change event document; other stages are ignored at
// this milestone. Events from before the call are not replayed unless a resume token
// is supplied (spec 2061 doc 14 §15.1).
func (c *Collection) Watch(ctx context.Context, pipeline any, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	return c.db.watch(ctx, Namespace{DB: c.dbName, Collection: c.name}, pipeline, opts)
}

// Watch opens a change stream over every collection in the database.
func (d *Database) Watch(ctx context.Context, pipeline any, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	return d.db.watch(ctx, Namespace{DB: d.name}, pipeline, opts)
}

// Watch opens a change stream over the whole database file, every collection in every
// database.
func (db *DB) Watch(ctx context.Context, pipeline any, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	return db.watch(ctx, Namespace{}, pipeline, opts)
}

// watch is the shared constructor behind the three Watch surfaces. It compiles the
// optional $match, resolves the start position from a resume token or "now", and
// registers the stream with the database feed.
func (db *DB) watch(ctx context.Context, scope Namespace, pipeline any, opts []*options.ChangeStreamOptions) (*ChangeStream, error) {
	if err := db.check(ctx); err != nil {
		return nil, err
	}
	m, err := matchFromPipeline(pipeline)
	if err != nil {
		return nil, err
	}
	o := mergeChangeStreamOptions(opts)
	cs := &ChangeStream{
		db:    db,
		feed:  db.feed,
		scope: scope,
		match: m,
		mode:  options.FullDocumentDefault,
	}
	if o.FullDocument != nil {
		cs.mode = *o.FullDocument
	}
	sub, head := db.feed.register()
	cs.sub = sub
	cs.next = head
	if tok := resumeTokenOf(o); len(tok) > 0 {
		seq, ok := decodeResumeToken(tok)
		if !ok {
			db.feed.unregister(sub)
			return nil, errors.New("doc: invalid resume token")
		}
		cs.next = seq
	}
	return cs, nil
}

// Next advances the stream to the next event in scope and reports whether one is
// ready. It blocks until an event arrives, the context is cancelled, or the stream is
// invalidated. After Next returns false, inspect Err.
func (cs *ChangeStream) Next(ctx context.Context) bool {
	if cs.done || cs.err != nil {
		return false
	}
	if cs.invalid {
		cs.setInvalidate()
		return true
	}
	for {
		if err := ctx.Err(); err != nil {
			cs.err = err
			return false
		}
		if cs.scan() {
			return true
		}
		if cs.err != nil {
			return false
		}
		select {
		case <-ctx.Done():
			cs.err = ctx.Err()
			return false
		case <-cs.sub.wake:
		}
	}
}

// TryNext is Next without blocking: it returns true if an event is already buffered,
// false otherwise, and never waits. A false return with a nil Err means no event is
// ready yet, not end of stream.
func (cs *ChangeStream) TryNext(ctx context.Context) bool {
	if cs.done || cs.err != nil {
		return false
	}
	if cs.invalid {
		cs.setInvalidate()
		return true
	}
	if err := ctx.Err(); err != nil {
		cs.err = err
		return false
	}
	return cs.scan()
}

// scan consumes ring events at or after the stream position until one matches the
// scope and pipeline, setting it current. It returns false when no matching event is
// ready (or the stream missed events, in which case it sets err).
func (cs *ChangeStream) scan() bool {
	evs, missed := cs.feed.since(cs.next)
	if missed {
		cs.err = ErrChangeStreamResume
		return false
	}
	for _, ev := range evs {
		cs.next = ev.seq
		if !cs.inScope(ev) {
			continue
		}
		raw := cs.render(ev)
		if cs.match != nil && !cs.match.Match(raw) {
			continue
		}
		cs.curRaw = raw
		cs.cur = nil
		if ev.op == OperationDrop && cs.scope.Collection != "" {
			cs.invalid = true
		}
		return true
	}
	return false
}

// setInvalidate makes the terminal invalidate event current and closes the stream.
func (cs *ChangeStream) setInvalidate() {
	cs.curRaw = cs.renderInvalidate()
	cs.cur = nil
	cs.invalid = false
	cs.done = true
}

// Current returns the event the last Next or TryNext delivered, decoded into a
// ChangeEvent. It decodes lazily and caches the result.
func (cs *ChangeStream) Current() *ChangeEvent {
	if cs.cur != nil {
		return cs.cur
	}
	if len(cs.curRaw) == 0 {
		return nil
	}
	var e ChangeEvent
	if err := Unmarshal(cs.curRaw, &e); err != nil {
		return nil
	}
	cs.cur = &e
	return cs.cur
}

// Decode unmarshals the current event document into out, the whole event, not just its
// full document. It returns ErrNoDocuments before the first event.
func (cs *ChangeStream) Decode(out any) error {
	if len(cs.curRaw) == 0 {
		return ErrNoDocuments
	}
	return Unmarshal(cs.curRaw, out)
}

// ResumeToken returns the token for the last delivered event, suitable for
// SetResumeAfter on a later Watch. It is nil before the first event.
func (cs *ChangeStream) ResumeToken() bson.Raw {
	if len(cs.curRaw) == 0 {
		return nil
	}
	tv, ok := cs.curRaw.Lookup("_id")
	if !ok || tv.Type != bson.TypeDocument {
		return nil
	}
	return cloneRawValueDoc(tv)
}

// Err returns the first error that ended the stream, if any. A stream closed by an
// invalidate reports ErrChangeStreamInvalidated.
func (cs *ChangeStream) Err() error {
	if cs.err == nil && cs.done {
		return ErrChangeStreamInvalidated
	}
	return cs.err
}

// Close unregisters the stream from the feed. It is safe to call more than once.
func (cs *ChangeStream) Close(context.Context) error {
	if cs.sub != nil {
		cs.feed.unregister(cs.sub)
		cs.sub = nil
	}
	cs.done = true
	return nil
}

// inScope reports whether an event belongs to this stream's watched scope.
func (cs *ChangeStream) inScope(ev feedEvent) bool {
	if cs.scope.DB != "" && ev.db != cs.scope.DB {
		return false
	}
	if cs.scope.Collection != "" && ev.coll != cs.scope.Collection {
		return false
	}
	return true
}

// render builds the BSON change-event document for a feed event, applying the
// fullDocument mode and computing the update diff. The document is both what Decode
// returns and what a pipeline $match runs against (spec 2061 doc 14 §15.2).
func (cs *ChangeStream) render(ev feedEvent) bson.Raw {
	b := bson.NewBuilder()
	b.AppendDocument("_id", encodeResumeToken(ev.seq))
	b.AppendString("operationType", ev.op)
	b.AppendDocument("ns", bson.NewBuilder().
		AppendString("db", ev.db).
		AppendString("coll", ev.coll).
		Build())
	if ev.id.Type != 0 {
		b.AppendDocument("documentKey", bson.NewBuilder().AppendValue("_id", ev.id).Build())
	}
	switch ev.op {
	case OperationInsert, OperationReplace:
		if len(ev.doc) > 0 {
			b.AppendDocument("fullDocument", ev.doc)
		}
	case OperationUpdate:
		b.AppendDocument("updateDescription", updateDescriptionDoc(ev.before, ev.doc))
		if cs.mode != options.FullDocumentDefault && len(ev.doc) > 0 {
			b.AppendDocument("fullDocument", ev.doc)
		}
	}
	b.AppendTimestamp("clusterTime", Timestamp{T: uint32(ev.cv)}.uint64())
	return b.Build()
}

// renderInvalidate builds the terminal invalidate event document.
func (cs *ChangeStream) renderInvalidate() bson.Raw {
	b := bson.NewBuilder()
	b.AppendDocument("_id", encodeResumeToken(cs.next))
	b.AppendString("operationType", OperationInvalidate)
	b.AppendDocument("ns", bson.NewBuilder().
		AppendString("db", cs.scope.DB).
		AppendString("coll", cs.scope.Collection).
		Build())
	return b.Build()
}

// updateDescriptionDoc computes the field-level update description document from the
// pre- and post-images by comparing top-level fields. A field present after with a
// different value than before (or absent before) is an updated field; a field present
// before and gone after is a removed field. Nested changes surface as a whole-field
// replacement, faithful for the common case and never wrong, only coarse.
func updateDescriptionDoc(before, after bson.Raw) bson.Raw {
	beforeEls, _ := before.Elements()
	afterEls, _ := after.Elements()
	beforeBy := make(map[string]bson.RawValue, len(beforeEls))
	for _, el := range beforeEls {
		beforeBy[el.Key] = el.Value
	}
	afterKeys := make(map[string]struct{}, len(afterEls))
	upd := bson.NewBuilder()
	for _, el := range afterEls {
		afterKeys[el.Key] = struct{}{}
		prev, ok := beforeBy[el.Key]
		if !ok || !bson.Equal(prev, el.Value) {
			upd.AppendValue(el.Key, el.Value)
		}
	}
	var removed []string
	for _, el := range beforeEls {
		if _, ok := afterKeys[el.Key]; !ok {
			removed = append(removed, el.Key)
		}
	}
	return bson.NewBuilder().
		AppendDocument("updatedFields", upd.Build()).
		AppendArray("removedFields", stringArray(removed)).
		Build()
}

// stringArray frames a slice of strings as a BSON array body.
func stringArray(ss []string) bson.Raw {
	b := bson.NewBuilder()
	for i, s := range ss {
		b.AppendString(strconv.Itoa(i), s)
	}
	return b.Build()
}

// cloneRawValueDoc copies a document-typed RawValue into a standalone Raw.
func cloneRawValueDoc(v bson.RawValue) bson.Raw {
	return bson.Raw(v.Data).Clone()
}

// matchFromPipeline extracts a $match filter from a watch pipeline. It accepts nil, an
// empty pipeline, or a slice whose first stage is {$match: {...}}. Any other shape is
// passed through as no filter, since later stages are not modelled at this milestone.
func matchFromPipeline(pipeline any) (*query.Matcher, error) {
	stages, err := marshalPipeline(pipeline)
	if err != nil {
		return nil, err
	}
	for _, stage := range stages {
		mv, ok := stage.Lookup("$match")
		if !ok {
			continue
		}
		if mv.Type != bson.TypeDocument {
			return nil, errors.New("doc: $match in a change stream pipeline must be a document")
		}
		return query.Compile(mv.Document())
	}
	return nil, nil
}

// mergeChangeStreamOptions folds a slice of option pointers into one, last write wins,
// nils skipped.
func mergeChangeStreamOptions(opts []*options.ChangeStreamOptions) *options.ChangeStreamOptions {
	out := &options.ChangeStreamOptions{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.FullDocument != nil {
			out.FullDocument = o.FullDocument
		}
		if o.BatchSize != nil {
			out.BatchSize = o.BatchSize
		}
		if o.ResumeAfter != nil {
			out.ResumeAfter = o.ResumeAfter
		}
		if o.StartAfter != nil {
			out.StartAfter = o.StartAfter
		}
	}
	return out
}

// resumeTokenOf returns the resume token bytes from ResumeAfter or StartAfter, in that
// order of precedence. The token may be a bson.Raw or anything that marshals to one.
func resumeTokenOf(o *options.ChangeStreamOptions) bson.Raw {
	for _, v := range []any{o.ResumeAfter, o.StartAfter} {
		if v == nil {
			continue
		}
		if raw, ok := v.(bson.Raw); ok {
			return raw
		}
		if raw, err := Marshal(v); err == nil {
			return raw
		}
	}
	return nil
}

// encodeResumeToken renders a sequence position as a resume token document,
// {_data: base64url(8-byte big-endian seq)} (spec 2061 doc 18 §9).
func encodeResumeToken(seq uint64) bson.Raw {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	data := base64.RawURLEncoding.EncodeToString(b[:])
	return bson.NewBuilder().AppendString("_data", data).Build()
}

// decodeResumeToken reverses encodeResumeToken, returning the sequence position the
// stream should resume after.
func decodeResumeToken(tok bson.Raw) (uint64, bool) {
	dv, ok := tok.Lookup("_data")
	if !ok || dv.Type != bson.TypeString {
		return 0, false
	}
	b, err := base64.RawURLEncoding.DecodeString(dv.StringValue())
	if err != nil || len(b) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}
