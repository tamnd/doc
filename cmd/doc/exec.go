package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
	"github.com/tamnd/doc/options"
)

// runHelper executes a parsed mongosh-style call against the active database and
// renders its result. It is the bridge from typed shell syntax to the library methods
// in spec 2061 doc 14.
func (a *app) runHelper(hc helperCall) error {
	coll := a.collection(hc.coll)
	switch hc.method {
	case "find":
		return a.doFind(coll, hc)
	case "findOne":
		return a.doFindOne(coll, hc)
	case "insertOne":
		return a.doInsertOne(coll, hc)
	case "insertMany":
		return a.doInsertMany(coll, hc)
	case "updateOne":
		return a.doUpdate(coll, hc, false)
	case "updateMany":
		return a.doUpdate(coll, hc, true)
	case "replaceOne":
		return a.doReplace(coll, hc)
	case "deleteOne":
		return a.doDelete(coll, hc, false)
	case "deleteMany":
		return a.doDelete(coll, hc, true)
	case "findAndModify":
		return a.doFindAndModify(coll, hc)
	case "count", "countDocuments":
		return a.doCount(coll, hc)
	case "estimatedDocumentCount":
		return a.doEstimatedCount(coll)
	case "distinct":
		return a.doDistinct(coll, hc)
	case "aggregate":
		return a.doAggregate(coll, hc)
	case "drop":
		return a.doDrop(coll)
	default:
		return queryError("unknown collection method: " + hc.method)
	}
}

// argDoc parses the idx-th argument as a document, returning an empty document when the
// argument is absent so find() and count() can be called with no filter.
func argDoc(args []string, idx int) (bson.Raw, error) {
	if idx >= len(args) || strings.TrimSpace(args[idx]) == "" {
		return bson.NewBuilder().Build(), nil
	}
	raw, err := extjson.Parse([]byte(args[idx]))
	if err != nil {
		return nil, queryError(err.Error())
	}
	return raw, nil
}

// argDocPresent parses the idx-th argument as a document only when it is given.
func argDocPresent(args []string, idx int) (bson.Raw, bool, error) {
	if idx >= len(args) || strings.TrimSpace(args[idx]) == "" {
		return nil, false, nil
	}
	raw, err := extjson.Parse([]byte(args[idx]))
	if err != nil {
		return nil, false, queryError(err.Error())
	}
	return raw, true, nil
}

func (a *app) doFind(coll *doc.Collection, hc helperCall) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	opt := options.Find()
	if proj, ok, err := argDocPresent(hc.args, 1); err != nil {
		return err
	} else if ok {
		opt.SetProjection(proj)
	}
	if err := a.applyChain(opt, hc.chain); err != nil {
		return err
	}
	if a.cfg.limit > 0 {
		opt.SetLimit(a.cfg.limit)
	}
	cur, err := coll.Find(a.ctx(), filter, opt)
	if err != nil {
		return classify(err)
	}
	return a.streamCursor(cur)
}

// applyChain folds .sort/.skip/.limit modifiers into the find options.
func (a *app) applyChain(opt *options.FindOptions, chain []chainCall) error {
	for _, c := range chain {
		switch c.name {
		case "sort":
			sort, err := extjson.Parse([]byte(c.arg))
			if err != nil {
				return queryError("sort: " + err.Error())
			}
			opt.SetSort(sort)
		case "skip":
			n, err := strconv.ParseInt(strings.TrimSpace(c.arg), 10, 64)
			if err != nil {
				return queryError("skip: expected an integer")
			}
			opt.SetSkip(n)
		case "limit":
			n, err := strconv.ParseInt(strings.TrimSpace(c.arg), 10, 64)
			if err != nil {
				return queryError("limit: expected an integer")
			}
			opt.SetLimit(n)
		default:
			return queryError("unknown cursor modifier: ." + c.name)
		}
	}
	return nil
}

func (a *app) doFindOne(coll *doc.Collection, hc helperCall) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	opt := options.FindOne()
	if proj, ok, err := argDocPresent(hc.args, 1); err != nil {
		return err
	} else if ok {
		opt.SetProjection(proj)
	}
	res := coll.FindOne(a.ctx(), filter, opt)
	raw, err := res.Raw()
	if err != nil {
		if errors.Is(err, doc.ErrNoDocuments) {
			return a.rend.writeText("null")
		}
		return classify(err)
	}
	return a.rend.renderDoc(raw)
}

func (a *app) doInsertOne(coll *doc.Collection, hc helperCall) error {
	d, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	res, err := coll.InsertOne(a.ctx(), d)
	if err != nil {
		return classify(err)
	}
	b := ackBuilder()
	appendAny(b, "insertedId", res.InsertedID)
	return a.rend.renderDoc(b.Build())
}

func (a *app) doInsertMany(coll *doc.Collection, hc helperCall) error {
	if len(hc.args) == 0 {
		return usageErr("insertMany([doc, ...])")
	}
	docs, err := extjson.ParseArray([]byte(hc.args[0]))
	if err != nil {
		return queryError(err.Error())
	}
	opt := options.InsertMany()
	if o, ok, err := argDocPresent(hc.args, 1); err != nil {
		return err
	} else if ok {
		if v, found := o.Lookup("ordered"); found && v.Type == bson.TypeBoolean {
			opt.SetOrdered(v.Boolean())
		}
	}
	anyDocs := make([]any, len(docs))
	for i := range docs {
		anyDocs[i] = docs[i]
	}
	res, err := coll.InsertMany(a.ctx(), anyDocs, opt)
	if err != nil {
		return classify(err)
	}
	b := ackBuilder()
	b.AppendInt64("insertedCount", int64(len(res.InsertedIDs)))
	idsArr := bson.NewBuilder()
	for i, id := range res.InsertedIDs {
		appendAny(idsArr, strconv.Itoa(i), id)
	}
	b.AppendArray("insertedIds", idsArr.Build())
	return a.rend.renderDoc(b.Build())
}

func (a *app) doUpdate(coll *doc.Collection, hc helperCall, many bool) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	update, err := argDoc(hc.args, 1)
	if err != nil {
		return err
	}
	opt := options.Update()
	if o, ok, err := argDocPresent(hc.args, 2); err != nil {
		return err
	} else if ok {
		if v, found := o.Lookup("upsert"); found && v.Type == bson.TypeBoolean {
			opt.SetUpsert(v.Boolean())
		}
	}
	var res *doc.UpdateResult
	if many {
		res, err = coll.UpdateMany(a.ctx(), filter, update, opt)
	} else {
		res, err = coll.UpdateOne(a.ctx(), filter, update, opt)
	}
	if err != nil {
		return classify(err)
	}
	return a.rend.renderDoc(updateAck(res))
}

func (a *app) doReplace(coll *doc.Collection, hc helperCall) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	repl, err := argDoc(hc.args, 1)
	if err != nil {
		return err
	}
	opt := options.Replace()
	if o, ok, err := argDocPresent(hc.args, 2); err != nil {
		return err
	} else if ok {
		if v, found := o.Lookup("upsert"); found && v.Type == bson.TypeBoolean {
			opt.SetUpsert(v.Boolean())
		}
	}
	res, err := coll.ReplaceOne(a.ctx(), filter, repl, opt)
	if err != nil {
		return classify(err)
	}
	return a.rend.renderDoc(updateAck(res))
}

// updateAck builds the update reply document, including upsertedId only when an upsert
// actually inserted a document.
func updateAck(res *doc.UpdateResult) bson.Raw {
	b := ackBuilder()
	b.AppendInt64("matchedCount", res.MatchedCount)
	b.AppendInt64("modifiedCount", res.ModifiedCount)
	if res.UpsertedCount > 0 {
		appendAny(b, "upsertedId", res.UpsertedID)
	}
	return b.Build()
}

func (a *app) doDelete(coll *doc.Collection, hc helperCall, many bool) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	var res *doc.DeleteResult
	if many {
		res, err = coll.DeleteMany(a.ctx(), filter)
	} else {
		res, err = coll.DeleteOne(a.ctx(), filter)
	}
	if err != nil {
		return classify(err)
	}
	b := ackBuilder()
	b.AppendInt64("deletedCount", res.DeletedCount)
	return a.rend.renderDoc(b.Build())
}

func (a *app) doFindAndModify(coll *doc.Collection, hc helperCall) error {
	spec, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	query, _ := lookupDoc(spec, "query")
	sort, hasSort := lookupDoc(spec, "sort")
	fields, hasFields := lookupDoc(spec, "fields")
	returnNew := lookupBool(spec, "new")
	upsert := lookupBool(spec, "upsert")
	remove := lookupBool(spec, "remove")

	var res *doc.SingleResult
	if remove {
		opt := options.FindOneAndDelete()
		if hasSort {
			opt.SetSort(sort)
		}
		if hasFields {
			opt.SetProjection(fields)
		}
		res = coll.FindOneAndDelete(a.ctx(), query, opt)
	} else {
		update, ok := lookupDoc(spec, "update")
		if !ok {
			return usageErr("findAndModify needs update or remove:true")
		}
		opt := options.FindOneAndUpdate()
		if hasSort {
			opt.SetSort(sort)
		}
		if hasFields {
			opt.SetProjection(fields)
		}
		if upsert {
			opt.SetUpsert(true)
		}
		if returnNew {
			opt.SetReturnDocument(options.After)
		}
		res = coll.FindOneAndUpdate(a.ctx(), query, update, opt)
	}
	raw, err := res.Raw()
	if err != nil {
		if errors.Is(err, doc.ErrNoDocuments) {
			return a.rend.writeText("null")
		}
		return classify(err)
	}
	return a.rend.renderDoc(raw)
}

func (a *app) doCount(coll *doc.Collection, hc helperCall) error {
	filter, err := argDoc(hc.args, 0)
	if err != nil {
		return err
	}
	n, err := coll.CountDocuments(a.ctx(), filter)
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(strconv.FormatInt(n, 10))
}

func (a *app) doEstimatedCount(coll *doc.Collection) error {
	n, err := coll.EstimatedDocumentCount(a.ctx())
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(strconv.FormatInt(n, 10))
}

func (a *app) doDistinct(coll *doc.Collection, hc helperCall) error {
	if len(hc.args) == 0 {
		return usageErr("distinct(field, filter)")
	}
	field, err := extjson.ParseValue([]byte(hc.args[0]))
	if err != nil || field.Type != bson.TypeString {
		return queryError("distinct: first argument must be a field name string")
	}
	filter, err := argDoc(hc.args, 1)
	if err != nil {
		return err
	}
	vals, err := coll.Distinct(a.ctx(), field.StringValue(), filter)
	if err != nil {
		return classify(err)
	}
	arr := bson.NewBuilder()
	for i, v := range vals {
		appendAny(arr, strconv.Itoa(i), v)
	}
	// Wrap the array so the renderer prints a top-level value document.
	wrapped := bson.NewBuilder().AppendArray("values", arr.Build()).Build()
	return a.rend.renderDoc(wrapped)
}

func (a *app) doAggregate(coll *doc.Collection, hc helperCall) error {
	if len(hc.args) == 0 {
		return usageErr("aggregate([stage, ...])")
	}
	stages, err := extjson.ParseArray([]byte(hc.args[0]))
	if err != nil {
		return queryError(err.Error())
	}
	pipeline := make([]any, len(stages))
	for i := range stages {
		pipeline[i] = stages[i]
	}
	cur, err := coll.Aggregate(a.ctx(), pipeline)
	if err != nil {
		return classify(err)
	}
	return a.streamCursor(cur)
}

func (a *app) doDrop(coll *doc.Collection) error {
	if err := coll.Drop(a.ctx()); err != nil {
		return classify(err)
	}
	return a.rend.writeText(`{ "ok": 1 }`)
}

// lookupDoc reads a nested document field from a spec document.
func lookupDoc(spec bson.Raw, key string) (bson.Raw, bool) {
	v, ok := spec.Lookup(key)
	if !ok || v.Type != bson.TypeDocument {
		return nil, false
	}
	return v.Document(), true
}

func lookupBool(spec bson.Raw, key string) bool {
	v, ok := spec.Lookup(key)
	return ok && v.Type == bson.TypeBoolean && v.Boolean()
}

func usageErr(form string) error {
	return cliError{code: exitUsage, msg: "usage: " + form}
}

// classify maps an engine error onto a cliError with the right exit code so a
// non-interactive run exits with a meaningful number (spec §17).
func classify(err error) error {
	if err == nil {
		return nil
	}
	var ce cliError
	if errors.As(err, &ce) {
		return ce
	}
	switch {
	case errors.Is(err, doc.ErrDocumentValidation):
		return cliError{code: exitSchemaViolation, msg: err.Error()}
	case isDuplicateKey(err):
		return cliError{code: exitQueryError, msg: err.Error()}
	default:
		return cliError{code: exitQueryError, msg: err.Error()}
	}
}

// isDuplicateKey reports a unique-index violation by its MongoDB code 11000, which the
// write exception carries.
func isDuplicateKey(err error) bool {
	var we doc.WriteException
	if errors.As(err, &we) {
		for _, e := range we.WriteErrors {
			if e.Code == 11000 {
				return true
			}
		}
	}
	return fmt.Sprintf("%v", err) == "duplicate key"
}
