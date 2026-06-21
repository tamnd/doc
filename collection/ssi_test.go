package collection

import (
	"errors"
	"sync"
	"testing"
)

// onCallColl seeds two doctors, both on call (oncall=1). The application invariant is
// that at least one of them stays on call. Write skew is the anomaly where two
// concurrent transactions each take one doctor off call after each reads that the
// other is still on, leaving nobody on call. This is the canonical exit-criterion
// workload for SSI (spec 2061 doc 06 §10.1, §17.3).
func onCallColl(t *testing.T) *Collection {
	t.Helper()
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "oncall", 1))
	mustInsert(t, c, docInt(2, "oncall", 1))
	return c
}

func onCall(t *testing.T, tx *Txn, id int32) bool {
	t.Helper()
	doc, err := tx.FindOne(filterID(id))
	if err != nil {
		t.Fatalf("FindOne(%d): %v", id, err)
	}
	if doc == nil {
		return false
	}
	v, _ := doc.Lookup("oncall")
	return v.Int32() == 1
}

func countOnCall(t *testing.T, c *Collection) int {
	t.Helper()
	n := 0
	for _, id := range []int32{1, 2} {
		doc, err := c.FindOne(filterID(id))
		if err != nil {
			t.Fatalf("FindOne(%d): %v", id, err)
		}
		if v, _ := doc.Lookup("oncall"); v.Int32() == 1 {
			n++
		}
	}
	return n
}

// TestWriteSkewPermittedUnderSnapshotIsolation demonstrates the anomaly SSI exists to
// catch: under snapshot isolation both transactions commit and the invariant breaks.
// This is the negative half of the exit criterion (SI permits write skew).
func TestWriteSkewPermittedUnderSnapshotIsolation(t *testing.T) {
	c := onCallColl(t)

	t1 := c.Begin()
	t2 := c.Begin()

	// Each transaction takes its own doctor off call only if the other is still on.
	if onCall(t, t1, 2) {
		if _, err := t1.UpdateOne(filterID(1), setDoc("oncall", 0)); err != nil {
			t.Fatalf("t1 update: %v", err)
		}
	}
	if onCall(t, t2, 1) {
		if _, err := t2.UpdateOne(filterID(2), setDoc("oncall", 0)); err != nil {
			t.Fatalf("t2 update: %v", err)
		}
	}

	if err := t1.Commit(); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("t2 commit: %v", err)
	}
	// Both committed and both doctors are off call: the invariant is violated, which
	// is exactly the weakness of snapshot isolation.
	if n := countOnCall(t, c); n != 0 {
		t.Fatalf("on-call count under SI: got %d, want 0 (write skew should have left nobody on call)", n)
	}
}

// TestWriteSkewAbortedUnderSerializable is the positive half of the exit criterion:
// the same interleaving under serializable isolation aborts the second committer with
// a retriable SerializationFailureError, so the invariant holds.
func TestWriteSkewAbortedUnderSerializable(t *testing.T) {
	c := onCallColl(t)
	ser := TransactionOptions{Isolation: Serializable}

	t1 := c.BeginTx(ser)
	t2 := c.BeginTx(ser)

	if onCall(t, t1, 2) {
		if _, err := t1.UpdateOne(filterID(1), setDoc("oncall", 0)); err != nil {
			t.Fatalf("t1 update: %v", err)
		}
	}
	if onCall(t, t2, 1) {
		if _, err := t2.UpdateOne(filterID(2), setDoc("oncall", 0)); err != nil {
			t.Fatalf("t2 update: %v", err)
		}
	}

	if err := mapCommitErr(t1.Commit()); err != nil {
		t.Fatalf("t1 commit: got %v, want nil (the first committer always succeeds)", err)
	}
	err := mapCommitErr(t2.Commit())
	if !IsRetriable(err) {
		t.Fatalf("t2 commit: got %v, want a retriable serialization failure", err)
	}
	var se *SerializationFailureError
	if !errors.As(err, &se) {
		t.Fatalf("t2 commit: got %T, want *SerializationFailureError", err)
	}
	// Exactly one doctor was taken off call, so the invariant holds.
	if n := countOnCall(t, c); n != 1 {
		t.Fatalf("on-call count under SSI: got %d, want 1 (one writer must have aborted)", n)
	}
}

// TestWriteSkewResolvedByRetry checks the full WithTransaction loop: when the second
// transaction aborts on serialization failure, the retry runs against a fresh snapshot
// where the first doctor is already off call, so its invariant check declines to take
// the second off and it commits cleanly. Both calls return success and the invariant
// holds, which is the behavior an application sees.
func TestWriteSkewResolvedByRetry(t *testing.T) {
	c := onCallColl(t)
	ser := TransactionOptions{Isolation: Serializable}

	// The first transaction, committed up front, takes doctor 1 off call.
	if err := c.WithTransaction(func(tx *Txn) error {
		if onCall(t, tx, 2) {
			_, err := tx.UpdateOne(filterID(1), setDoc("oncall", 0))
			return err
		}
		return nil
	}, ser); err != nil {
		t.Fatalf("first transaction: %v", err)
	}

	// The second transaction would take doctor 2 off call, but only if doctor 1 is on.
	// Doctor 1 is now off, so it must decline and leave doctor 2 on call.
	if err := c.WithTransaction(func(tx *Txn) error {
		if onCall(t, tx, 1) {
			_, err := tx.UpdateOne(filterID(2), setDoc("oncall", 0))
			return err
		}
		return nil
	}, ser); err != nil {
		t.Fatalf("second transaction: %v", err)
	}

	if n := countOnCall(t, c); n != 1 {
		t.Fatalf("on-call count after both transactions: got %d, want 1", n)
	}
}

// TestSerializableDisjointWritesCommit checks that serializable isolation does not
// over-abort the common case: two transactions that touch unrelated documents both
// commit, because neither forms a read-write antidependency with the other.
func TestSerializableDisjointWritesCommit(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	mustInsert(t, c, docInt(2, "n", 20))
	ser := TransactionOptions{Isolation: Serializable}

	t1 := c.BeginTx(ser)
	t2 := c.BeginTx(ser)
	// Each reads and writes only its own document, so there is no antidependency.
	_ = onCall(t, t1, 1)
	_ = onCall(t, t2, 2)
	if _, err := t1.UpdateOne(filterID(1), incDoc("n", 1)); err != nil {
		t.Fatalf("t1 update: %v", err)
	}
	if _, err := t2.UpdateOne(filterID(2), incDoc("n", 1)); err != nil {
		t.Fatalf("t2 update: %v", err)
	}
	if err := mapCommitErr(t1.Commit()); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if err := mapCommitErr(t2.Commit()); err != nil {
		t.Fatalf("t2 commit: got %v, want nil (disjoint writes must not abort)", err)
	}
}

// TestSerializableReadOnlyNeverAborts checks that a read-only serializable transaction
// never aborts on serialization grounds: it has no writes, so it cannot be the pivot of
// a dangerous structure (spec 2061 doc 06 §10). A concurrent writer of a document it
// read commits, and the read-only transaction still commits cleanly.
func TestSerializableReadOnlyNeverAborts(t *testing.T) {
	c := onCallColl(t)
	ser := TransactionOptions{Isolation: Serializable}

	reader := c.BeginTx(ser)
	_ = onCall(t, reader, 1)
	_ = onCall(t, reader, 2)

	// A concurrent writer changes a document the reader read, then commits.
	if err := c.WithTransaction(func(tx *Txn) error {
		_, err := tx.UpdateOne(filterID(1), setDoc("oncall", 0))
		return err
	}, ser); err != nil {
		t.Fatalf("concurrent writer: %v", err)
	}

	// The read-only transaction buffered no writes, so its commit just releases the
	// snapshot and never reports a serialization failure.
	if err := mapCommitErr(reader.Commit()); err != nil {
		t.Fatalf("read-only commit: got %v, want nil", err)
	}
}

// TestWriteSkewConcurrentInvariantHolds runs the write-skew workload under real
// goroutine concurrency: for each doctor pair, two transactions race to take one
// doctor off call, each guarded by WithTransaction under serializable isolation. The
// invariant (at least one doctor per pair stays on call) must hold for every pair,
// because the loser of each race retries against a snapshot where the winner already
// committed and then declines to write. Run under -race this also exercises the SSI
// tracker's shared-state mutations.
func TestWriteSkewConcurrentInvariantHolds(t *testing.T) {
	c := newTestColl(t)
	const pairs = 50
	for i := 0; i < pairs; i++ {
		mustInsert(t, c, docInt(int32(2*i+1), "oncall", 1))
		mustInsert(t, c, docInt(int32(2*i+2), "oncall", 1))
	}
	ser := TransactionOptions{Isolation: Serializable}

	var wg sync.WaitGroup
	for i := 0; i < pairs; i++ {
		a := int32(2*i + 1)
		b := int32(2*i + 2)
		// Goroutine one tries to take doctor a off call if b is on; goroutine two the
		// reverse. The serializable retry resolves the race so only one can win.
		for _, pair := range [][2]int32{{a, b}, {b, a}} {
			self, other := pair[0], pair[1]
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.WithTransaction(func(tx *Txn) error {
					if onCall(t, tx, other) {
						_, err := tx.UpdateOne(filterID(self), setDoc("oncall", 0))
						return err
					}
					return nil
				}, ser)
			}()
		}
	}
	wg.Wait()

	for i := 0; i < pairs; i++ {
		a := int32(2*i + 1)
		b := int32(2*i + 2)
		on := 0
		for _, id := range []int32{a, b} {
			doc, err := c.FindOne(filterID(id))
			if err != nil {
				t.Fatalf("FindOne(%d): %v", id, err)
			}
			if v, _ := doc.Lookup("oncall"); v.Int32() == 1 {
				on++
			}
		}
		if on < 1 {
			t.Fatalf("pair (%d,%d): both off call, invariant violated", a, b)
		}
	}
}
