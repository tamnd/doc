package oracle

import (
	"testing"

	"github.com/tamnd/doc/sys"
)

// fixedGen returns a deterministic generator; the corpus never relies on minted
// ids, but the target requires one.
func fixedGen() sys.IDGenerator { return &sys.FixedIDGenerator{Timestamp: 1} }

// TestCorpusSize guards against the corpus silently shrinking.
func TestCorpusSize(t *testing.T) {
	if n := len(Corpus()); n < 150 {
		t.Fatalf("corpus has %d cases, want at least 150", n)
	}
}

// TestDocTargetExecutesCorpus runs every case against the doc target and checks it
// completes without a transport error and never panics. It exercises the full
// InsertOne / FindOne / Find / DeleteOne / CountDocuments surface.
func TestDocTargetExecutesCorpus(t *testing.T) {
	target := NewDocTarget(fixedGen())
	defer target.Close()

	for _, c := range Corpus() {
		if err := target.Reset(); err != nil {
			t.Fatalf("reset for %q: %v", c.Name, err)
		}
		for i, op := range c.Setup {
			if _, err := target.Exec(op); err != nil {
				t.Fatalf("%q setup[%d]: %v", c.Name, i, err)
			}
		}
		if _, err := target.Exec(c.Probe); err != nil {
			t.Fatalf("%q probe: %v", c.Name, err)
		}
	}
}

// TestDocTargetIsDeterministic runs the corpus through the harness with the doc
// target on both sides. Two independent doc instances must agree on every probe,
// which both validates the harness wiring and proves doc's results are stable.
func TestDocTargetIsDeterministic(t *testing.T) {
	h := &Harness{
		Reference: NewDocTarget(fixedGen()),
		Subject:   NewDocTarget(fixedGen()),
	}
	defer h.Reference.Close()
	defer h.Subject.Close()

	diffs, err := h.Run(Corpus())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(diffs) != 0 {
		for _, d := range diffs {
			t.Errorf("nondeterministic case %q: %s", d.Case, d.Detail)
		}
	}
}
