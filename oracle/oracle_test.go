package oracle

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
)

// fakeTarget is a scripted Target: it records the ops it receives and returns
// results from a per-op-kind table, so the harness machinery can be tested
// without a real database.
type fakeTarget struct {
	name     string
	results  map[OpKind]Result
	execErr  error
	resetErr error
	resets   int
	gotSetup []Op
	gotProbe []Op
	closed   bool
}

func (f *fakeTarget) Name() string { return f.name }

func (f *fakeTarget) Reset() error {
	f.resets++
	return f.resetErr
}

func (f *fakeTarget) Exec(op Op) (Result, error) {
	if f.execErr != nil {
		return Result{}, f.execErr
	}
	// Heuristic: probe ops in these tests are always the last kind looked up;
	// record every op for assertions.
	f.gotProbe = append(f.gotProbe, op)
	return f.results[op.Kind], nil
}

func (f *fakeTarget) Close() error {
	f.closed = true
	return nil
}

func doc(b ...byte) bson.Raw { return bson.Raw(b) }

func TestHarnessNoDiffWhenEqual(t *testing.T) {
	res := Result{N: 1, Docs: []bson.Raw{doc(5, 0, 0, 0, 0)}}
	ref := &fakeTarget{name: "mongodb", results: map[OpKind]Result{OpFindOne: res}}
	sub := &fakeTarget{name: "doc", results: map[OpKind]Result{OpFindOne: res}}
	h := &Harness{Reference: ref, Subject: sub}

	diffs, err := h.Run([]Case{{Name: "find", Probe: Op{Kind: OpFindOne}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %+v", diffs)
	}
	if ref.resets != 1 || sub.resets != 1 {
		t.Fatalf("each target should be reset once: ref=%d sub=%d", ref.resets, sub.resets)
	}
}

func TestHarnessDetectsCountDiff(t *testing.T) {
	ref := &fakeTarget{name: "mongodb", results: map[OpKind]Result{OpCount: {N: 3}}}
	sub := &fakeTarget{name: "doc", results: map[OpKind]Result{OpCount: {N: 2}}}
	h := &Harness{Reference: ref, Subject: sub}

	diffs, err := h.Run([]Case{{Name: "count", Probe: Op{Kind: OpCount}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].Case != "count" {
		t.Fatalf("diff case = %q", diffs[0].Case)
	}
	if diffs[0].Detail == "" {
		t.Fatal("diff should carry a human-readable detail")
	}
}

func TestHarnessDetectsErrCodeDiff(t *testing.T) {
	ref := &fakeTarget{name: "mongodb", results: map[OpKind]Result{OpInsertOne: {ErrCode: "DuplicateKey"}}}
	sub := &fakeTarget{name: "doc", results: map[OpKind]Result{OpInsertOne: {ErrCode: ""}}}
	h := &Harness{Reference: ref, Subject: sub}

	diffs, _ := h.Run([]Case{{Name: "dup", Probe: Op{Kind: OpInsertOne}}})
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
}

func TestHarnessDetectsDocByteDiff(t *testing.T) {
	ref := &fakeTarget{name: "mongodb", results: map[OpKind]Result{OpFindOne: {Docs: []bson.Raw{doc(5, 0, 0, 0, 0)}}}}
	sub := &fakeTarget{name: "doc", results: map[OpKind]Result{OpFindOne: {Docs: []bson.Raw{doc(5, 0, 0, 0, 1)}}}}
	h := &Harness{Reference: ref, Subject: sub}

	diffs, _ := h.Run([]Case{{Name: "bytes", Probe: Op{Kind: OpFindOne}}})
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
}

func TestHarnessReplaysSetup(t *testing.T) {
	ref := &fakeTarget{name: "mongodb", results: map[OpKind]Result{OpInsertOne: {N: 1}, OpCount: {N: 1}}}
	sub := &fakeTarget{name: "doc", results: map[OpKind]Result{OpInsertOne: {N: 1}, OpCount: {N: 1}}}
	h := &Harness{Reference: ref, Subject: sub}

	c := Case{
		Name:  "setup-then-count",
		Setup: []Op{{Kind: OpInsertOne}, {Kind: OpInsertOne}},
		Probe: Op{Kind: OpCount},
	}
	if _, err := h.Run([]Case{c}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Each target sees 2 setup ops + 1 probe.
	if len(sub.gotProbe) != 3 {
		t.Fatalf("subject saw %d ops, want 3", len(sub.gotProbe))
	}
}

func TestHarnessResetErrorSurfaces(t *testing.T) {
	boom := errors.New("reset failed")
	ref := &fakeTarget{name: "mongodb", resetErr: boom}
	sub := &fakeTarget{name: "doc"}
	h := &Harness{Reference: ref, Subject: sub}

	if _, err := h.Run([]Case{{Name: "x", Probe: Op{Kind: OpFindOne}}}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want reset failure", err)
	}
}

func TestHarnessExecErrorSurfaces(t *testing.T) {
	boom := errors.New("transport down")
	ref := &fakeTarget{name: "mongodb", execErr: boom}
	sub := &fakeTarget{name: "doc"}
	h := &Harness{Reference: ref, Subject: sub}

	if _, err := h.Run([]Case{{Name: "x", Probe: Op{Kind: OpFindOne}}}); err == nil {
		t.Fatal("expected probe exec error to surface")
	}
}
