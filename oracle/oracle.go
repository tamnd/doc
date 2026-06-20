// Package oracle is the behavior-oracle harness: it runs the same operation
// against a reference (MongoDB) and against the subject (doc) and diffs the
// results, so every MQL feature is measured against MongoDB's actual behavior
// (spec 2061 doc 19 §17). M0 ships the framework only - the Op vocabulary, the
// Target seam, the result diff, and the case runner. The MongoDB target and the
// doc target are wired in M2, when the first insert/find operations exist.
package oracle

import (
	"fmt"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
)

// OpKind enumerates the operations the oracle can drive. The set grows as
// milestones add surface area; M0 declares the vocabulary so cases can be
// authored before the operations are implemented.
type OpKind string

const (
	OpInsertOne         OpKind = "insertOne"
	OpFindOne           OpKind = "findOne"
	OpFind              OpKind = "find"
	OpDeleteOne         OpKind = "deleteOne"
	OpUpdateOne         OpKind = "updateOne"
	OpUpdateMany        OpKind = "updateMany"
	OpReplaceOne        OpKind = "replaceOne"
	OpFindOneAndUpdate  OpKind = "findOneAndUpdate"
	OpFindOneAndReplace OpKind = "findOneAndReplace"
	OpFindOneAndDelete  OpKind = "findOneAndDelete"
	OpCount             OpKind = "countDocuments"
	OpAggregate         OpKind = "aggregate"
	OpCreateIndex       OpKind = "createIndex"
	OpDistinct          OpKind = "distinct"
)

// Op is a single operation to drive against a target. Collection names the
// collection; Doc carries an inserted/updated document; Filter, Update, and
// Pipeline carry MQL expressions as raw BSON. Unused fields are nil for a given
// kind.
type Op struct {
	Kind        OpKind
	Collection  string
	Doc         bson.Raw
	Filter      bson.Raw
	Update      bson.Raw
	Replacement bson.Raw // replaceOne / findOneAndReplace
	Pipeline    []bson.Raw
	Field       string // for distinct

	// Find shaping (OpFind/OpFindOne): a projection and sort as raw BSON, and the
	// skip/limit bounds. A nil Projection or Sort means none; a zero Limit means
	// no cap.
	Projection bson.Raw
	Sort       bson.Raw
	Skip       int64
	Limit      int64

	// ReturnAfter selects the after version for findOneAndUpdate /
	// findOneAndReplace; the default (false) returns the before version, matching
	// MongoDB.
	ReturnAfter bool

	// Index describes the secondary index to build for OpCreateIndex; nil for every
	// other kind.
	Index *IndexModel
}

// IndexModel is the index specification for an OpCreateIndex op: the ordered key
// and the option flags the planner and the unique check honor.
type IndexModel struct {
	Key    []catalog.KeyPart
	Name   string
	Unique bool
	Sparse bool
}

// Result is the normalized outcome of an Op. Docs holds returned documents in a
// canonical order; N holds a numeric result (count, matched, modified); ErrCode
// holds a non-empty error category when the operation failed. Two results are
// equal when their Docs, N, and ErrCode all match - the diff deliberately
// compares semantic outcome, not driver-specific wire details.
type Result struct {
	Docs     []bson.Raw
	N        int64
	Matched  int64 // matched count for update/replace
	Modified int64 // modified count for update/replace
	ErrCode  string
}

// Target is a system the oracle can drive: either the MongoDB reference or the
// doc subject. Implementations execute an Op and return a normalized Result.
type Target interface {
	// Name identifies the target in diff output ("mongodb", "doc").
	Name() string
	// Reset drops all collections so each case starts from an empty database.
	Reset() error
	// Exec runs op and returns its normalized result.
	Exec(op Op) (Result, error)
	// Close releases the target's resources.
	Close() error
}

// Case is a named sequence of operations. The setup operations establish state;
// the Probe operation's result is the one compared between targets. Splitting
// setup from probe keeps the diff focused on the operation under test while still
// exercising realistic preconditions.
type Case struct {
	Name  string
	Setup []Op
	Probe Op
}

// Diff is a single mismatch between the reference and the subject for one case.
type Diff struct {
	Case      string
	Reference Result
	Subject   Result
	Detail    string
}

// Harness pairs a reference target with a subject target and runs cases against
// both.
type Harness struct {
	Reference Target
	Subject   Target
}

// Run executes each case against both targets and returns the diffs. A case with
// no diff is conformant. Run resets both targets before each case and replays the
// setup operations, then compares the probe results.
func (h *Harness) Run(cases []Case) ([]Diff, error) {
	var diffs []Diff
	for _, c := range cases {
		if err := h.Reference.Reset(); err != nil {
			return diffs, fmt.Errorf("reset reference for %q: %w", c.Name, err)
		}
		if err := h.Subject.Reset(); err != nil {
			return diffs, fmt.Errorf("reset subject for %q: %w", c.Name, err)
		}
		for _, op := range c.Setup {
			if _, err := h.Reference.Exec(op); err != nil {
				return diffs, fmt.Errorf("reference setup %q: %w", c.Name, err)
			}
			if _, err := h.Subject.Exec(op); err != nil {
				return diffs, fmt.Errorf("subject setup %q: %w", c.Name, err)
			}
		}
		refRes, refErr := h.Reference.Exec(c.Probe)
		subRes, subErr := h.Subject.Exec(c.Probe)
		if refErr != nil || subErr != nil {
			// A transport error (not a modeled ErrCode) is a harness failure, not
			// a behavioral diff.
			return diffs, fmt.Errorf("exec probe %q: ref=%v sub=%v", c.Name, refErr, subErr)
		}
		if detail, ok := compare(refRes, subRes); !ok {
			diffs = append(diffs, Diff{
				Case:      c.Name,
				Reference: refRes,
				Subject:   subRes,
				Detail:    detail,
			})
		}
	}
	return diffs, nil
}

// compare reports whether two results are semantically equal, and if not, a
// human-readable detail of the first difference.
func compare(ref, sub Result) (string, bool) {
	if ref.ErrCode != sub.ErrCode {
		return fmt.Sprintf("errCode: ref=%q sub=%q", ref.ErrCode, sub.ErrCode), false
	}
	if ref.N != sub.N {
		return fmt.Sprintf("n: ref=%d sub=%d", ref.N, sub.N), false
	}
	if ref.Matched != sub.Matched {
		return fmt.Sprintf("matched: ref=%d sub=%d", ref.Matched, sub.Matched), false
	}
	if ref.Modified != sub.Modified {
		return fmt.Sprintf("modified: ref=%d sub=%d", ref.Modified, sub.Modified), false
	}
	if len(ref.Docs) != len(sub.Docs) {
		return fmt.Sprintf("docCount: ref=%d sub=%d", len(ref.Docs), len(sub.Docs)), false
	}
	for i := range ref.Docs {
		if !bytesEqual(ref.Docs[i], sub.Docs[i]) {
			return fmt.Sprintf("doc[%d] bytes differ", i), false
		}
	}
	return "", true
}

func bytesEqual(a, b bson.Raw) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
