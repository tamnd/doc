package collection

import (
	"fmt"
	"strings"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// InsertManyResult reports the ids of the documents an insertMany stored, in the
// order they were supplied (spec 2061 doc 13 §3.3).
type InsertManyResult struct {
	InsertedIDs []bson.RawValue
}

// BulkWriteResult aggregates the per-type counts of a bulkWrite, plus the ids of
// inserted and upserted documents keyed by their position in the ops slice (spec
// 2061 doc 13 §14.3).
type BulkWriteResult struct {
	InsertedCount int64
	MatchedCount  int64
	ModifiedCount int64
	DeletedCount  int64
	UpsertedCount int64
	UpsertedIDs   map[int]bson.RawValue
	InsertedIDs   map[int]bson.RawValue
}

// BulkWriteError records one failed operation in a bulk batch by its index in the
// ops slice along with the underlying error.
type BulkWriteError struct {
	Index int
	Err   error
}

// BulkWriteException is returned when one or more operations in an insertMany or
// bulkWrite fail. It carries the per-operation errors and the partial result for
// the operations that did succeed (spec 2061 doc 13 §14.3). The successful
// operations are still committed.
type BulkWriteException struct {
	WriteErrors []BulkWriteError
	Result      BulkWriteResult
}

// Error summarizes the failed operations.
func (e *BulkWriteException) Error() string {
	if len(e.WriteErrors) == 1 {
		return fmt.Sprintf("collection: bulk write failed at index %d: %v", e.WriteErrors[0].Index, e.WriteErrors[0].Err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "collection: bulk write failed with %d errors:", len(e.WriteErrors))
	for _, we := range e.WriteErrors {
		fmt.Fprintf(&b, " [%d: %v]", we.Index, we.Err)
	}
	return b.String()
}

// BulkOp is one operation in a bulkWrite batch: InsertOneOp, UpdateOneOp,
// UpdateManyOp, ReplaceOneOp, DeleteOneOp, or DeleteManyOp.
type BulkOp interface{ isBulkOp() }

// InsertOneOp inserts a single document.
type InsertOneOp struct{ Document bson.Raw }

// UpdateOneOp applies an update-operator document to the first matching document,
// optionally upserting when nothing matches.
type UpdateOneOp struct {
	Filter, Update bson.Raw
	Upsert         bool
}

// UpdateManyOp applies an update-operator document to every matching document,
// optionally upserting a single document when nothing matches.
type UpdateManyOp struct {
	Filter, Update bson.Raw
	Upsert         bool
}

// ReplaceOneOp replaces the first matching document, optionally upserting.
type ReplaceOneOp struct {
	Filter, Replacement bson.Raw
	Upsert              bool
}

// DeleteOneOp deletes the first matching document.
type DeleteOneOp struct{ Filter bson.Raw }

// DeleteManyOp deletes every matching document.
type DeleteManyOp struct{ Filter bson.Raw }

func (InsertOneOp) isBulkOp()  {}
func (UpdateOneOp) isBulkOp()  {}
func (UpdateManyOp) isBulkOp() {}
func (ReplaceOneOp) isBulkOp() {}
func (DeleteOneOp) isBulkOp()  {}
func (DeleteManyOp) isBulkOp() {}

// InsertMany buffers a batch of inserts into the transaction. In ordered mode the
// first failure halts the batch; in unordered mode every document is attempted and
// the failures are collected. The successfully buffered documents stay in the
// transaction so the caller's Commit persists them; the returned error, when
// non-nil, is a *BulkWriteException carrying the partial result (spec 2061 doc 13
// §3.3). Note that _id collisions are detected here per-document, while a secondary
// unique-index violation surfaces only at Commit as a batch-level error.
func (t *Txn) InsertMany(docs []bson.Raw, ordered bool) (InsertManyResult, error) {
	if t.done {
		return InsertManyResult{}, ErrTxnDone
	}
	if !t.writable {
		return InsertManyResult{}, storage.ErrReadOnly
	}
	var res InsertManyResult
	var werrs []BulkWriteError
	for i, d := range docs {
		_, id, err := t.insertBuffered(d)
		if err != nil {
			werrs = append(werrs, BulkWriteError{Index: i, Err: err})
			if ordered {
				break
			}
			continue
		}
		res.InsertedIDs = append(res.InsertedIDs, id)
	}
	if len(werrs) > 0 {
		return res, &BulkWriteException{
			WriteErrors: werrs,
			Result:      BulkWriteResult{InsertedCount: int64(len(res.InsertedIDs))},
		}
	}
	return res, nil
}

// BulkWrite buffers a mixed batch of insert, update, replace, and delete
// operations into the transaction. Ordered mode halts on the first error;
// unordered mode attempts every operation and collects the errors. The successful
// operations stay buffered for the caller's Commit; a non-nil error is a
// *BulkWriteException carrying the per-operation errors and the partial result
// (spec 2061 doc 13 §14).
func (t *Txn) BulkWrite(ops []BulkOp, ordered bool) (BulkWriteResult, error) {
	if t.done {
		return BulkWriteResult{}, ErrTxnDone
	}
	if !t.writable {
		return BulkWriteResult{}, storage.ErrReadOnly
	}
	res := BulkWriteResult{UpsertedIDs: map[int]bson.RawValue{}, InsertedIDs: map[int]bson.RawValue{}}
	var werrs []BulkWriteError
	for i, op := range ops {
		if err := t.applyBulkOp(i, op, &res); err != nil {
			werrs = append(werrs, BulkWriteError{Index: i, Err: err})
			if ordered {
				break
			}
		}
	}
	if len(werrs) > 0 {
		return res, &BulkWriteException{WriteErrors: werrs, Result: res}
	}
	return res, nil
}

// applyBulkOp dispatches one bulk operation and folds its outcome into res.
func (t *Txn) applyBulkOp(i int, op BulkOp, res *BulkWriteResult) error {
	switch o := op.(type) {
	case InsertOneOp:
		_, id, err := t.insertBuffered(o.Document)
		if err != nil {
			return err
		}
		res.InsertedCount++
		res.InsertedIDs[i] = id
	case UpdateOneOp:
		r, err := t.UpdateOneWith(o.Filter, o.Update, UpdateOptions{Upsert: o.Upsert})
		if err != nil {
			return err
		}
		foldUpdate(i, r, res)
	case UpdateManyOp:
		r, err := t.UpdateManyWith(o.Filter, o.Update, UpdateOptions{Upsert: o.Upsert})
		if err != nil {
			return err
		}
		foldUpdate(i, r, res)
	case ReplaceOneOp:
		r, err := t.ReplaceOneWith(o.Filter, o.Replacement, UpdateOptions{Upsert: o.Upsert})
		if err != nil {
			return err
		}
		foldUpdate(i, r, res)
	case DeleteOneOp:
		n, err := t.DeleteOne(o.Filter)
		if err != nil {
			return err
		}
		res.DeletedCount += n
	case DeleteManyOp:
		n, err := t.DeleteMany(o.Filter)
		if err != nil {
			return err
		}
		res.DeletedCount += n
	default:
		return fmt.Errorf("collection: unknown bulk op %T", op)
	}
	return nil
}

// foldUpdate accumulates an UpdateResult into a BulkWriteResult, recording an
// upserted _id under the operation's index.
func foldUpdate(i int, r UpdateResult, res *BulkWriteResult) {
	res.MatchedCount += r.Matched
	res.ModifiedCount += r.Modified
	res.UpsertedCount += r.Upserted
	if r.Upserted > 0 {
		res.UpsertedIDs[i] = r.UpsertedID
	}
}
