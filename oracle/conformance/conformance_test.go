//go:build mongo

package conformance

import (
	"os"
	"testing"

	"github.com/tamnd/doc/oracle"
	"github.com/tamnd/doc/sys"
)

// TestConformance drives the shared M2-c corpus against a live MongoDB (the
// reference) and the doc engine (the subject) and fails on any behavioral diff.
// It skips unless MONGO_URL points at a reachable server (spec 2061 doc 19 §17):
//
//	MONGO_URL=mongodb://localhost:27017 go test -tags mongo ./...
func TestConformance(t *testing.T) {
	uri := os.Getenv("MONGO_URL")
	if uri == "" {
		t.Skip("set MONGO_URL to run the live MongoDB conformance suite")
	}

	ref, err := NewMongoTarget(uri, "doc_conformance")
	if err != nil {
		t.Fatalf("connect MongoDB at %s: %v", uri, err)
	}
	defer func() { _ = ref.Close() }()

	sub := oracle.NewDocTarget(&sys.FixedIDGenerator{Timestamp: 1})
	defer func() { _ = sub.Close() }()

	h := &oracle.Harness{Reference: ref, Subject: sub}
	cases := oracle.Corpus()

	diffs, err := h.Run(cases)
	if err != nil {
		t.Fatalf("run corpus (%d cases): %v", len(cases), err)
	}
	if len(diffs) != 0 {
		for _, d := range diffs {
			t.Errorf("case %q: %s\n  reference=%+v\n  subject=%+v",
				d.Case, d.Detail, d.Reference, d.Subject)
		}
		t.Fatalf("%d of %d cases diverge from MongoDB", len(diffs), len(cases))
	}
	t.Logf("all %d cases conform to MongoDB", len(cases))
}
