// Package schema compiles and evaluates collection validators (spec 2061 doc 09
// §10). A validator is attached to a collection and checked on every insert and on
// every update or replace that produces a new document version. doc supports the
// two MongoDB validator forms: a $jsonSchema document and a plain MQL query
// expression. Compile detects the form, builds an evaluation tree once, and
// Validate walks it against a candidate document, returning a Failure that names
// the rule that was not satisfied.
package schema

import (
	"errors"
	"fmt"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
)

// ErrInvalidSchema reports a validator document that is not a well-formed
// $jsonSchema or query expression.
var ErrInvalidSchema = errors.New("schema: invalid validator")

// Failure is the error a validator returns when a document does not satisfy it. It
// carries the rule that failed and the dotted path of the offending field so the
// write layer can build MongoDB's errInfo response (spec 2061 doc 09 §10.6a).
type Failure struct {
	Rule string // the JSON Schema keyword or "query" for a query-expression validator
	Path string // dotted path to the field, empty at the document root
	Msg  string // human-readable description
}

func (f *Failure) Error() string {
	if f.Path != "" {
		return fmt.Sprintf("document failed validation: %s at %q: %s", f.Rule, f.Path, f.Msg)
	}
	return "document failed validation: " + f.Msg
}

// Validator evaluates one collection validator. Exactly one of schema or matcher is
// set, decided at Compile time by the validator's form.
type Validator struct {
	schema  *node          // set for the $jsonSchema form
	matcher *query.Matcher // set for the query-expression form
	raw     bson.Raw
}

// Compile builds a Validator from a validator document. A document whose sole
// top-level key is $jsonSchema is compiled as JSON Schema; anything else is treated
// as an MQL query expression and compiled with the query matcher. A nil or empty
// validator compiles to a Validator that accepts every document.
func Compile(validator bson.Raw) (*Validator, error) {
	if len(validator) == 0 {
		return &Validator{}, nil
	}
	elems, err := validator.Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) == 1 && elems[0].Key == "$jsonSchema" {
		if elems[0].Value.Type != bson.TypeDocument {
			return nil, fmt.Errorf("%w: $jsonSchema must be a document", ErrInvalidSchema)
		}
		n, err := compileNode(elems[0].Value.Document())
		if err != nil {
			return nil, err
		}
		return &Validator{schema: n, raw: validator.Clone()}, nil
	}
	m, err := query.Compile(validator)
	if err != nil {
		return nil, err
	}
	return &Validator{matcher: m, raw: validator.Clone()}, nil
}

// Raw returns the validator document the Validator was compiled from.
func (v *Validator) Raw() bson.Raw { return v.raw }

// Empty reports whether the Validator accepts every document.
func (v *Validator) Empty() bool { return v == nil || (v.schema == nil && v.matcher == nil) }

// Validate checks doc against the validator, returning a *Failure when it does not
// pass and nil when it does. An empty validator passes everything.
func (v *Validator) Validate(doc bson.Raw) error {
	if v.Empty() {
		return nil
	}
	if v.matcher != nil {
		if v.matcher.Match(doc) {
			return nil
		}
		return &Failure{Rule: "query", Msg: "document did not match the validator expression"}
	}
	return v.schema.validate(bson.RawValue{Type: bson.TypeDocument, Data: doc}, "")
}
