package doc

import "github.com/tamnd/doc/bson"

// SingleResult holds the outcome of a one-document operation (FindOne and the
// findAndModify family). Call Decode to unmarshal the document, or Err to test
// for a no-match without decoding (spec 2061 doc 14 §6.2).
type SingleResult struct {
	doc bson.Raw
	err error
}

// newSingleResult wraps a found document, or ErrNoDocuments when raw is nil.
func newSingleResult(raw bson.Raw, err error) *SingleResult {
	if err != nil {
		return &SingleResult{err: err}
	}
	if raw == nil {
		return &SingleResult{err: ErrNoDocuments}
	}
	return &SingleResult{doc: raw}
}

// Decode unmarshals the result document into val. It returns ErrNoDocuments when
// the operation matched nothing.
func (r *SingleResult) Decode(val any) error {
	if r.err != nil {
		return r.err
	}
	return Unmarshal(r.doc, val)
}

// Raw returns the undecoded BSON of the result document.
func (r *SingleResult) Raw() (Raw, error) {
	if r.err != nil {
		return nil, r.err
	}
	return Raw(r.doc), nil
}

// Err returns the error captured when the result was produced, ErrNoDocuments
// included.
func (r *SingleResult) Err() error { return r.err }
