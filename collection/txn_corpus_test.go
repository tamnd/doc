package collection

import (
	"errors"
	"fmt"
	"testing"
)

// This file is the multi-document transaction oracle corpus (spec 2061 doc 19 §22, M5
// exit criterion): a generated set of more than 100 deterministic two-transaction
// scenarios, each with a known commit-or-abort outcome, run against the engine. The
// scenarios cover the cases the transaction layer must get right: disjoint writes
// commit, write-write conflicts abort the second committer, write skew commits under
// snapshot isolation but aborts under serializable, read-only transactions commit, and
// sequential transactions never conflict even when they touch the same key.
//
// Each scenario's expected outcome is computed from the class it belongs to, and every
// class has an unambiguous outcome under the engine's semantics, so the corpus asserts
// exact results rather than approximations.

// outcome is the expected result of a transaction's commit.
type outcome int

const (
	wantCommit outcome = iota
	wantWriteConflict
	wantSerialFailure
)

// kv is a key and the tag value to write to it.
type kv struct {
	key int32
	tag int64
}

// scenario is one corpus case: two transactions over a seeded key space, with explicit
// read and write sets, a commit order, and an interleaving (concurrent or sequential).
type scenario struct {
	name       string
	iso        IsolationLevel
	seedKeys   []int32
	t1Reads    []int32
	t1Writes   []kv
	t2Reads    []int32
	t2Writes   []kv
	sequential bool // commit t1 fully before t2 begins
	t2First    bool // commit t2 before t1 (concurrent only)
	wantT1     outcome
	wantT2     outcome
}

func isoName(iso IsolationLevel) string {
	if iso == Serializable {
		return "ssi"
	}
	return "si"
}

// buildCorpus generates the scenario set. It returns well over 100 cases spread across
// the five classes.
func buildCorpus() []scenario {
	var cs []scenario
	isos := []IsolationLevel{SnapshotIsolation, Serializable}

	// Class 1: disjoint read-write sets always commit. t1 owns key a, t2 owns key b.
	for a := int32(1); a <= 6; a++ {
		for b := int32(1); b <= 6; b++ {
			if a == b {
				continue
			}
			for _, iso := range isos {
				cs = append(cs, scenario{
					name:     fmt.Sprintf("disjoint/%s/a%d_b%d", isoName(iso), a, b),
					iso:      iso,
					seedKeys: []int32{a, b},
					t1Reads:  []int32{a}, t1Writes: []kv{{a, 100}},
					t2Reads: []int32{b}, t2Writes: []kv{{b, 200}},
					wantT1: wantCommit, wantT2: wantCommit,
				})
			}
		}
	}

	// Class 2: write-write conflict on a shared key. Both write it concurrently; the
	// second committer aborts with a write conflict under both isolation levels.
	for k := int32(1); k <= 5; k++ {
		for _, iso := range isos {
			for _, t2First := range []bool{false, true} {
				wantT1, wantT2 := wantCommit, wantWriteConflict
				if t2First {
					wantT1, wantT2 = wantWriteConflict, wantCommit
				}
				cs = append(cs, scenario{
					name:     fmt.Sprintf("wwconflict/%s/k%d/t2first=%v", isoName(iso), k, t2First),
					iso:      iso,
					seedKeys: []int32{k},
					t1Reads:  []int32{k}, t1Writes: []kv{{k, 100}},
					t2Reads: []int32{k}, t2Writes: []kv{{k, 200}},
					t2First: t2First,
					wantT1:  wantT1, wantT2: wantT2,
				})
			}
		}
	}

	// Class 3: write skew. Each reads both keys and writes a different one. Under SI both
	// commit (the anomaly); under SSI the second committer aborts with a serialization
	// failure.
	pairs := [][2]int32{{1, 2}, {2, 3}, {3, 4}, {4, 5}, {1, 3}, {2, 4}}
	for _, p := range pairs {
		a, b := p[0], p[1]
		for _, iso := range isos {
			wantT2 := wantCommit
			if iso == Serializable {
				wantT2 = wantSerialFailure
			}
			cs = append(cs, scenario{
				name:     fmt.Sprintf("writeskew/%s/a%d_b%d", isoName(iso), a, b),
				iso:      iso,
				seedKeys: []int32{a, b},
				t1Reads:  []int32{a, b}, t1Writes: []kv{{a, 100}},
				t2Reads: []int32{a, b}, t2Writes: []kv{{b, 200}},
				wantT1: wantCommit, wantT2: wantT2,
			})
		}
	}

	// Class 4: a read-only transaction concurrent with a writer. The read-only one never
	// aborts, the writer commits.
	for k := int32(1); k <= 5; k++ {
		for _, iso := range isos {
			cs = append(cs, scenario{
				name:     fmt.Sprintf("readonly/%s/k%d", isoName(iso), k),
				iso:      iso,
				seedKeys: []int32{k},
				t1Reads:  []int32{k}, // t1 is read-only
				t2Reads:  []int32{k}, t2Writes: []kv{{k, 200}},
				wantT1: wantCommit, wantT2: wantCommit,
			})
		}
	}

	// Class 5: sequential transactions. t1 commits fully before t2 begins, so even a
	// shared write key never conflicts: t2 sees t1's commit in its snapshot.
	for k := int32(1); k <= 5; k++ {
		for _, iso := range isos {
			cs = append(cs, scenario{
				name:     fmt.Sprintf("sequential/%s/k%d", isoName(iso), k),
				iso:      iso,
				seedKeys: []int32{k},
				t1Reads:  []int32{k}, t1Writes: []kv{{k, 100}},
				t2Reads: []int32{k}, t2Writes: []kv{{k, 200}},
				sequential: true,
				wantT1:     wantCommit, wantT2: wantCommit,
			})
		}
	}

	return cs
}

// applyOps runs a scenario transaction's reads then writes against tx.
func applyOps(t *testing.T, tx *Txn, reads []int32, writes []kv) {
	t.Helper()
	for _, k := range reads {
		if _, err := tx.FindOne(filterID(k)); err != nil {
			t.Fatalf("read key %d: %v", k, err)
		}
	}
	for _, w := range writes {
		if _, err := tx.UpdateOne(filterID(w.key), tagSet(w.tag)); err != nil {
			t.Fatalf("write key %d: %v", w.key, err)
		}
	}
}

// checkOutcome asserts a mapped commit error matches the expected outcome.
func checkOutcome(t *testing.T, label string, err error, want outcome) {
	t.Helper()
	err = mapCommitErr(err)
	switch want {
	case wantCommit:
		if err != nil {
			t.Fatalf("%s: got %v, want commit", label, err)
		}
	case wantWriteConflict:
		var wce *WriteConflictError
		if !errors.As(err, &wce) {
			t.Fatalf("%s: got %v, want a write conflict", label, err)
		}
	case wantSerialFailure:
		var se *SerializationFailureError
		if !errors.As(err, &se) {
			t.Fatalf("%s: got %v, want a serialization failure", label, err)
		}
	}
}

func runScenario(t *testing.T, sc scenario) {
	c := newTestColl(t)
	opts := TransactionOptions{Isolation: sc.iso}
	for _, k := range sc.seedKeys {
		mustInsert(t, c, docTag(k, 0))
	}

	if sc.sequential {
		t1 := c.BeginTx(opts)
		applyOps(t, t1, sc.t1Reads, sc.t1Writes)
		checkOutcome(t, "t1", t1.Commit(), sc.wantT1)
		t2 := c.BeginTx(opts)
		applyOps(t, t2, sc.t2Reads, sc.t2Writes)
		checkOutcome(t, "t2", t2.Commit(), sc.wantT2)
		return
	}

	t1 := c.BeginTx(opts)
	t2 := c.BeginTx(opts)
	applyOps(t, t1, sc.t1Reads, sc.t1Writes)
	applyOps(t, t2, sc.t2Reads, sc.t2Writes)
	if sc.t2First {
		checkOutcome(t, "t2", t2.Commit(), sc.wantT2)
		checkOutcome(t, "t1", t1.Commit(), sc.wantT1)
	} else {
		checkOutcome(t, "t1", t1.Commit(), sc.wantT1)
		checkOutcome(t, "t2", t2.Commit(), sc.wantT2)
	}
}

// TestTransactionOracleCorpus runs the full corpus and asserts it holds at least 100
// cases, satisfying the M5 exit criterion.
func TestTransactionOracleCorpus(t *testing.T) {
	corpus := buildCorpus()
	if len(corpus) < 100 {
		t.Fatalf("corpus has %d cases, want at least 100", len(corpus))
	}
	for _, sc := range corpus {
		t.Run(sc.name, func(t *testing.T) { runScenario(t, sc) })
	}
}
