package oracle

import (
	"errors"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/update"
	"github.com/tamnd/doc/vfs"
)

// DocTarget drives operations against the doc engine: an in-memory collection per
// named collection, reset between cases. It is the subject side of the behavior
// oracle (spec 2061 doc 19 §17). The MongoDB reference target lives in the nested
// conformance module behind a build tag so the engine package never depends on a
// running server.
type DocTarget struct {
	fs    vfs.FS
	gen   sys.IDGenerator
	colls map[string]*collection.Collection
}

// NewDocTarget returns a DocTarget. gen mints _ids for documents inserted without
// one; pass a deterministic generator so auto-minted ids match the reference.
func NewDocTarget(gen sys.IDGenerator) *DocTarget {
	return &DocTarget{
		fs:    vfs.NewMemFS(),
		gen:   gen,
		colls: make(map[string]*collection.Collection),
	}
}

// Name identifies the target in diff output.
func (d *DocTarget) Name() string { return "doc" }

// Reset closes every open collection and starts from a fresh in-memory file
// system, so each case runs against an empty database.
func (d *DocTarget) Reset() error {
	for _, c := range d.colls {
		if err := c.Close(); err != nil {
			return err
		}
	}
	d.colls = make(map[string]*collection.Collection)
	d.fs = vfs.NewMemFS()
	return nil
}

// Close releases all collections.
func (d *DocTarget) Close() error {
	for _, c := range d.colls {
		if err := c.Close(); err != nil {
			return err
		}
	}
	d.colls = nil
	return nil
}

// coll returns the named collection, opening it over the target's file system on
// first use.
func (d *DocTarget) coll(name string) (*collection.Collection, error) {
	if c, ok := d.colls[name]; ok {
		return c, nil
	}
	c, err := collection.Open(d.fs, name+".doc", collection.Options{IDGen: d.gen})
	if err != nil {
		return nil, err
	}
	d.colls[name] = c
	return c, nil
}

// Exec runs op against the doc engine and normalizes the outcome. A modeled
// behavioral error (duplicate key, validation) becomes a Result.ErrCode; an
// unexpected transport-style error is returned so the harness can fail the run.
func (d *DocTarget) Exec(op Op) (Result, error) {
	c, err := d.coll(op.Collection)
	if err != nil {
		return Result{}, err
	}
	switch op.Kind {
	case OpInsertOne:
		if _, err := c.InsertOne(op.Doc); err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{N: 1}, nil

	case OpFindOne:
		// findOne is a find with the projection and sort applied, returning the
		// first result, so it shares the shaping path with limit 1.
		docs, err := c.FindWith(op.Filter, collection.FindOptions{
			Projection: op.Projection,
			Sort:       op.Sort,
			Skip:       op.Skip,
			Limit:      1,
		})
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		if len(docs) == 0 {
			return Result{}, nil
		}
		return Result{Docs: docs[:1]}, nil

	case OpFind:
		docs, err := c.FindWith(op.Filter, collection.FindOptions{
			Projection: op.Projection,
			Sort:       op.Sort,
			Skip:       op.Skip,
			Limit:      op.Limit,
		})
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{Docs: docs}, nil

	case OpDeleteOne:
		n, err := c.DeleteOne(op.Filter)
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{N: n}, nil

	case OpCount:
		n, err := c.CountDocuments(op.Filter)
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{N: n}, nil

	case OpUpdateOne:
		return docUpdateResult(c.UpdateOne(op.Filter, op.Update))

	case OpUpdateMany:
		return docUpdateResult(c.UpdateMany(op.Filter, op.Update))

	case OpReplaceOne:
		return docUpdateResult(c.ReplaceOne(op.Filter, op.Replacement))

	case OpFindOneAndUpdate:
		return docFindModify(c.FindOneAndUpdate(op.Filter, op.Update, docFindModifyOpts(op)))

	case OpFindOneAndReplace:
		return docFindModify(c.FindOneAndReplace(op.Filter, op.Replacement, docFindModifyOpts(op)))

	case OpFindOneAndDelete:
		return docFindModify(c.FindOneAndDelete(op.Filter, docFindModifyOpts(op)))

	case OpCreateIndex:
		if op.Index == nil {
			return Result{}, errUnsupportedOp
		}
		if _, err := c.CreateIndex(collection.IndexModel{
			Key:    op.Index.Key,
			Name:   op.Index.Name,
			Unique: op.Index.Unique,
			Sparse: op.Index.Sparse,
		}); err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{N: 1}, nil

	case OpAggregate:
		docs, err := c.Aggregate(op.Pipeline)
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		return Result{Docs: docs}, nil

	case OpDistinct:
		vals, err := c.Distinct(op.Field, op.Filter)
		if err != nil {
			if code, ok := errCode(err); ok {
				return Result{ErrCode: code}, nil
			}
			return Result{}, err
		}
		docs := make([]bson.Raw, len(vals))
		for i, v := range vals {
			docs[i] = WrapDistinctValue(v)
		}
		return Result{Docs: NormalizeDistinctDocs(docs)}, nil

	default:
		return Result{}, errUnsupportedOp
	}
}

// docUpdateResult normalizes a collection update/replace outcome into a Result
// carrying the matched and modified counts.
func docUpdateResult(res collection.UpdateResult, err error) (Result, error) {
	if err != nil {
		if code, ok := errCode(err); ok {
			return Result{ErrCode: code}, nil
		}
		return Result{}, err
	}
	return Result{Matched: res.Matched, Modified: res.Modified}, nil
}

// docFindModify normalizes a findAndModify outcome: the returned document, or an
// empty Result when nothing matched.
func docFindModify(doc bson.Raw, err error) (Result, error) {
	if err != nil {
		if code, ok := errCode(err); ok {
			return Result{ErrCode: code}, nil
		}
		return Result{}, err
	}
	if doc == nil {
		return Result{}, nil
	}
	return Result{Docs: []bson.Raw{doc}}, nil
}

// docFindModifyOpts maps an Op's shaping fields to the collection's
// findAndModify options.
func docFindModifyOpts(op Op) collection.FindModifyOptions {
	ret := collection.ReturnBefore
	if op.ReturnAfter {
		ret = collection.ReturnAfter
	}
	return collection.FindModifyOptions{
		Sort:       op.Sort,
		Projection: op.Projection,
		Return:     ret,
	}
}

// errUnsupportedOp reports an Op kind the doc target does not implement yet
// (aggregate); those arrive in later milestones.
var errUnsupportedOp = errors.New("oracle: operation kind not supported yet")

// errCode maps a doc engine error to the oracle's normalized error category,
// reporting ok=false for an error that is not a modeled behavioral outcome. The
// categories mirror the MongoDB reference's: a unique-_id collision is the
// E11000 "DuplicateKey", an invalid _id type is "InvalidID".
func errCode(err error) (string, bool) {
	switch {
	case errors.Is(err, storage.ErrDuplicateKey):
		return "DuplicateKey", true
	case errors.Is(err, bson.ErrInvalidIDType):
		return "InvalidID", true
	case errors.Is(err, collection.ErrImmutableField):
		return "ImmutableField", true
	case errors.Is(err, update.ErrConflict):
		return "ConflictingUpdateOperators", true
	case errors.Is(err, update.ErrNotNumeric):
		return "TypeMismatch", true
	case errors.Is(err, update.ErrPathConflict):
		return "PathNotViable", true
	case errors.Is(err, update.ErrBadUpdate):
		return "FailedToParse", true
	default:
		return "", false
	}
}
