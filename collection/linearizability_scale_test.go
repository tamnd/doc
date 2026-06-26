package collection

import (
	"os"
	"strconv"
	"testing"
)

// This file scales the linearizability suite to the doc 19 §M9 target: one million
// concurrent histories checked at SI and SSI. A single n-history round costs O(n^2)
// in the serializability cycle check, so the suite does not build one giant graph.
// It runs many independent rounds, each a fresh concurrent stress with its own
// deterministic seed, and checks each round in full. Histories accumulate across
// rounds until the target is met, so the total coverage reaches a million while
// each individual check stays bounded. The seeds are fixed, so a failing round is
// reproducible from its index.

// linScale returns the target total history count, honoring DOC_LIN_HISTORIES and
// the -short cap. The default keeps CI quick; the release sweep sets the env var to
// 1000000 to hit the §M9 target.
func linScale(dflt int) int {
	n := dflt
	if v := os.Getenv("DOC_LIN_HISTORIES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if testing.Short() && n > 2000 {
		n = 2000
	}
	return n
}

// runLinScale runs rounds of the concurrent stress at iso until at least target
// histories have been checked, verifying snapshot isolation on every round and, for
// the serializable level, the absence of an antidependency cycle. It returns the
// total number of histories checked.
func runLinScale(t *testing.T, iso IsolationLevel, target int) int {
	t.Helper()
	// roundTxns is the per-worker transaction count; with 2*GOMAXPROCS workers a
	// round yields a few hundred histories, small enough that the O(n^2) cycle
	// check on the round stays cheap.
	const roundTxns = 40
	checked := 0
	for round := 0; checked < target; round++ {
		seedCommit, keys, hist := runStressParams(t, iso, roundTxns, uint64(round+1)*7919)
		if len(hist) == 0 {
			t.Fatalf("round %d recorded no committed transactions", round)
		}
		checkSnapshotIsolation(t, seedCommit, keys, hist)
		if iso == Serializable {
			checkSerializable(t, hist)
		}
		checked += len(hist)
	}
	return checked
}

// TestLinearizabilityScaleSI checks snapshot isolation across many rounds up to the
// configured history target.
func TestLinearizabilityScaleSI(t *testing.T) {
	if testing.Short() {
		t.Skip("scale linearizability skipped in -short")
	}
	target := linScale(4000)
	n := runLinScale(t, SnapshotIsolation, target)
	t.Logf("checked %d histories under snapshot isolation across deterministic rounds", n)
}

// TestLinearizabilityScaleSSI checks serializable isolation across many rounds: each
// round must satisfy the snapshot-read contract and contain no read-write
// antidependency cycle.
func TestLinearizabilityScaleSSI(t *testing.T) {
	if testing.Short() {
		t.Skip("scale linearizability skipped in -short")
	}
	target := linScale(4000)
	n := runLinScale(t, Serializable, target)
	t.Logf("checked %d histories under serializable isolation across deterministic rounds", n)
}
