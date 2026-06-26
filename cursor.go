package doc

import (
	"context"
	"fmt"
	"reflect"

	"github.com/tamnd/doc/bson"
)

// Cursor iterates the documents a query produced. The result set is materialized
// from the engine as a snapshot at query time, so concurrent writes do not
// affect an in-flight cursor (spec 2061 doc 14 §16). A Cursor is not safe for
// concurrent use: drive it from one goroutine.
type Cursor struct {
	docs      []bson.Raw
	pos       int // index of the current document; -1 before the first Next
	err       error
	closed    bool
	batchSize int32
}

// newCursor wraps an already-materialized result set.
func newCursor(docs []bson.Raw) *Cursor {
	return &Cursor{docs: docs, pos: -1}
}

// Next advances to the next document, returning false at the end of the result
// set or when ctx is cancelled. After Next returns false, check Err.
func (c *Cursor) Next(ctx context.Context) bool {
	if c == nil || c.closed || c.err != nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		c.err = err
		return false
	}
	if c.pos+1 >= len(c.docs) {
		c.pos = len(c.docs)
		return false
	}
	c.pos++
	return true
}

// Decode unmarshals the current document into val.
func (c *Cursor) Decode(val any) error {
	if c == nil {
		return ErrNilCursor
	}
	if c.pos < 0 || c.pos >= len(c.docs) {
		return fmt.Errorf("doc: Decode called with no current document")
	}
	return Unmarshal(c.docs[c.pos], val)
}

// Current returns the raw BSON of the current document. The slice is valid until
// the next Next call; copy it if you need it longer.
func (c *Cursor) Current() Raw {
	if c == nil || c.pos < 0 || c.pos >= len(c.docs) {
		return nil
	}
	return Raw(c.docs[c.pos])
}

// All decodes every remaining document into results, which must be a pointer to
// a slice. It is a convenience over the Next/Decode loop; prefer streaming for
// large result sets.
func (c *Cursor) All(ctx context.Context, results any) error {
	if c == nil {
		return ErrNilCursor
	}
	rv := reflect.ValueOf(results)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("doc: All target must be a non-nil pointer to a slice")
	}
	sv := rv.Elem()
	if sv.Kind() != reflect.Slice {
		return fmt.Errorf("doc: All target must point to a slice, got %s", sv.Kind())
	}
	elemType := sv.Type().Elem()
	out := reflect.MakeSlice(sv.Type(), 0, len(c.docs)-c.pos-1)
	for c.Next(ctx) {
		ev := reflect.New(elemType)
		if err := Unmarshal(c.docs[c.pos], ev.Interface()); err != nil {
			return err
		}
		out = reflect.Append(out, ev.Elem())
	}
	if err := c.Err(); err != nil {
		return err
	}
	sv.Set(out)
	return c.Close(ctx)
}

// ID returns the cursor's server-side identifier, or 0 when the cursor is
// exhausted. doc materializes results client-side, so the id is always 0.
func (c *Cursor) ID() int64 { return 0 }

// Err returns the first error encountered during iteration, if any.
func (c *Cursor) Err() error {
	if c == nil {
		return ErrNilCursor
	}
	return c.err
}

// Close releases the cursor. It is safe to call more than once.
func (c *Cursor) Close(ctx context.Context) error {
	if c == nil {
		return ErrNilCursor
	}
	c.closed = true
	return nil
}

// RemainingBatchLength returns how many documents are buffered and not yet
// consumed.
func (c *Cursor) RemainingBatchLength() int {
	if c == nil || c.closed {
		return 0
	}
	rem := len(c.docs) - (c.pos + 1)
	if rem < 0 {
		return 0
	}
	return rem
}

// SetBatchSize records a batch-size hint. Because results are materialized, it
// does not change memory behavior; it is accepted for API compatibility.
func (c *Cursor) SetBatchSize(size int32) {
	if c != nil {
		c.batchSize = size
	}
}
