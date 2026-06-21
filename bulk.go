package doc

import (
	"context"
	"errors"

	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/options"
)

// WriteModel is one operation in a BulkWrite batch. The concrete models are
// InsertOneModel, UpdateOneModel, UpdateManyModel, ReplaceOneModel,
// DeleteOneModel, and DeleteManyModel (spec 2061 doc 14 §11). Build them with the
// New* constructors and the fluent Set* methods.
type WriteModel interface {
	writeModel()
}

// InsertOneModel inserts a single document.
type InsertOneModel struct {
	Document any
}

// NewInsertOneModel returns an empty InsertOneModel.
func NewInsertOneModel() *InsertOneModel { return &InsertOneModel{} }

// SetDocument sets the document to insert.
func (m *InsertOneModel) SetDocument(doc any) *InsertOneModel {
	m.Document = doc
	return m
}

func (*InsertOneModel) writeModel() {}

// UpdateOneModel applies an update-operator document to the first match.
type UpdateOneModel struct {
	Filter any
	Update any
	Upsert *bool
}

// NewUpdateOneModel returns an empty UpdateOneModel.
func NewUpdateOneModel() *UpdateOneModel { return &UpdateOneModel{} }

// SetFilter sets the query selecting the document to update.
func (m *UpdateOneModel) SetFilter(f any) *UpdateOneModel { m.Filter = f; return m }

// SetUpdate sets the update-operator document to apply.
func (m *UpdateOneModel) SetUpdate(u any) *UpdateOneModel { m.Update = u; return m }

// SetUpsert toggles inserting a new document when nothing matches.
func (m *UpdateOneModel) SetUpsert(b bool) *UpdateOneModel { m.Upsert = &b; return m }

func (*UpdateOneModel) writeModel() {}

// UpdateManyModel applies an update-operator document to every match.
type UpdateManyModel struct {
	Filter any
	Update any
	Upsert *bool
}

// NewUpdateManyModel returns an empty UpdateManyModel.
func NewUpdateManyModel() *UpdateManyModel { return &UpdateManyModel{} }

// SetFilter sets the query selecting the documents to update.
func (m *UpdateManyModel) SetFilter(f any) *UpdateManyModel { m.Filter = f; return m }

// SetUpdate sets the update-operator document to apply.
func (m *UpdateManyModel) SetUpdate(u any) *UpdateManyModel { m.Update = u; return m }

// SetUpsert toggles inserting a new document when nothing matches.
func (m *UpdateManyModel) SetUpsert(b bool) *UpdateManyModel { m.Upsert = &b; return m }

func (*UpdateManyModel) writeModel() {}

// ReplaceOneModel replaces the first matching document.
type ReplaceOneModel struct {
	Filter      any
	Replacement any
	Upsert      *bool
}

// NewReplaceOneModel returns an empty ReplaceOneModel.
func NewReplaceOneModel() *ReplaceOneModel { return &ReplaceOneModel{} }

// SetFilter sets the query selecting the document to replace.
func (m *ReplaceOneModel) SetFilter(f any) *ReplaceOneModel { m.Filter = f; return m }

// SetReplacement sets the replacement document.
func (m *ReplaceOneModel) SetReplacement(r any) *ReplaceOneModel { m.Replacement = r; return m }

// SetUpsert toggles inserting the replacement when nothing matches.
func (m *ReplaceOneModel) SetUpsert(b bool) *ReplaceOneModel { m.Upsert = &b; return m }

func (*ReplaceOneModel) writeModel() {}

// DeleteOneModel deletes the first matching document.
type DeleteOneModel struct {
	Filter any
}

// NewDeleteOneModel returns an empty DeleteOneModel.
func NewDeleteOneModel() *DeleteOneModel { return &DeleteOneModel{} }

// SetFilter sets the query selecting the document to delete.
func (m *DeleteOneModel) SetFilter(f any) *DeleteOneModel { m.Filter = f; return m }

func (*DeleteOneModel) writeModel() {}

// DeleteManyModel deletes every matching document.
type DeleteManyModel struct {
	Filter any
}

// NewDeleteManyModel returns an empty DeleteManyModel.
func NewDeleteManyModel() *DeleteManyModel { return &DeleteManyModel{} }

// SetFilter sets the query selecting the documents to delete.
func (m *DeleteManyModel) SetFilter(f any) *DeleteManyModel { m.Filter = f; return m }

func (*DeleteManyModel) writeModel() {}

// upsertBool reports the upsert flag value, defaulting to false when unset.
func upsertBool(b *bool) bool { return b != nil && *b }

// toBulkOp lowers one public WriteModel into the engine's BulkOp.
func toBulkOp(m WriteModel) (collection.BulkOp, error) {
	switch v := m.(type) {
	case *InsertOneModel:
		d, err := toDoc(v.Document)
		if err != nil {
			return nil, err
		}
		return collection.InsertOneOp{Document: d}, nil
	case *UpdateOneModel:
		f, err := toFilter(v.Filter)
		if err != nil {
			return nil, err
		}
		u, err := toDoc(v.Update)
		if err != nil {
			return nil, err
		}
		return collection.UpdateOneOp{Filter: f, Update: u, Upsert: upsertBool(v.Upsert)}, nil
	case *UpdateManyModel:
		f, err := toFilter(v.Filter)
		if err != nil {
			return nil, err
		}
		u, err := toDoc(v.Update)
		if err != nil {
			return nil, err
		}
		return collection.UpdateManyOp{Filter: f, Update: u, Upsert: upsertBool(v.Upsert)}, nil
	case *ReplaceOneModel:
		f, err := toFilter(v.Filter)
		if err != nil {
			return nil, err
		}
		r, err := toDoc(v.Replacement)
		if err != nil {
			return nil, err
		}
		return collection.ReplaceOneOp{Filter: f, Replacement: r, Upsert: upsertBool(v.Upsert)}, nil
	case *DeleteOneModel:
		f, err := toFilter(v.Filter)
		if err != nil {
			return nil, err
		}
		return collection.DeleteOneOp{Filter: f}, nil
	case *DeleteManyModel:
		f, err := toFilter(v.Filter)
		if err != nil {
			return nil, err
		}
		return collection.DeleteManyOp{Filter: f}, nil
	default:
		return nil, ErrNilDocument
	}
}

// BulkWrite executes a batch of write models in one transaction. By default the
// batch is ordered: the first failure stops it. Pass an unordered BulkWriteOptions
// to attempt every operation and collect the failures (spec 2061 doc 14 §11).
func (c *Collection) BulkWrite(ctx context.Context, models []WriteModel, opts ...*options.BulkWriteOptions) (*BulkWriteResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, ErrEmptySlice
	}
	ordered := true
	for _, o := range opts {
		if o != nil && o.Ordered != nil {
			ordered = *o.Ordered
		}
	}
	ops := make([]collection.BulkOp, len(models))
	for i, m := range models {
		op, err := toBulkOp(m)
		if err != nil {
			return nil, err
		}
		ops[i] = op
	}
	col, err := c.writeExec(ctx)
	if err != nil {
		return nil, err
	}
	res, err := col.BulkWrite(ops, ordered)
	out := bulkResultToPublic(res)
	if err != nil {
		return out, mapBulkErr(err)
	}
	return out, nil
}

// bulkResultToPublic converts the engine's per-position result into the public
// shape with any-typed ids keyed by int64.
func bulkResultToPublic(r collection.BulkWriteResult) *BulkWriteResult {
	out := &BulkWriteResult{
		InsertedCount: r.InsertedCount,
		MatchedCount:  r.MatchedCount,
		ModifiedCount: r.ModifiedCount,
		DeletedCount:  r.DeletedCount,
		UpsertedCount: r.UpsertedCount,
		UpsertedIDs:   map[int64]any{},
	}
	for i, id := range r.UpsertedIDs {
		if v, err := decodeNatural(id); err == nil {
			out.UpsertedIDs[int64(i)] = v
		}
	}
	return out
}

// mapBulkErr translates a collection bulk exception into the public
// BulkWriteException, preserving per-operation indexes and the partial result.
func mapBulkErr(err error) error {
	var be *collection.BulkWriteException
	if !errors.As(err, &be) {
		return mapEngineErr(err)
	}
	out := &BulkWriteException{}
	for _, we := range be.WriteErrors {
		out.WriteErrors = append(out.WriteErrors, BulkWriteError{
			WriteError: WriteError{
				Index:   we.Index,
				Code:    codeForErr(we.Err),
				Message: we.Err.Error(),
			},
		})
	}
	return out
}

// codeForErr maps a known sentinel to its MongoDB error code, defaulting to a
// generic bad-value code.
func codeForErr(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, collection.ErrDuplicateKey):
		return codeDuplicateKey
	default:
		return codeBadValue
	}
}
