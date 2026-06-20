package mvcc

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// doc builds a one-field {n: int32} document, a compact stand-in for a record
// whose identity the tests track by its single value.
func doc(n int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("n", n).Build()
}

// valOf reads the "n" field back out of a record.
func valOf(t *testing.T, r bson.Raw) int32 {
	t.Helper()
	v, ok := r.Lookup("n")
	if !ok {
		t.Fatalf("record has no n field: % x", []byte(r))
	}
	return v.Int32()
}

// seed inserts one record in its own transaction and returns its RID.
func seed(t *testing.T, s *Store, n int32) storage.RID {
	t.Helper()
	var rid storage.RID
	if err := s.WithTransaction(func(tx *Txn) error {
		r, err := s.Insert(tx, doc(n))
		rid = r
		return err
	}); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	return rid
}

func TestInsertThenReadInLaterSnapshot(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 7)

	rt := s.Begin(false)
	got, err := s.Get(rt, rid)
	if err != nil {
		t.Fatalf("get after commit: %v", err)
	}
	if v := valOf(t, got); v != 7 {
		t.Fatalf("read %d, want 7", v)
	}
	_ = rt.Commit()
}

// A snapshot taken before a write does not see that write, even after it
// commits: the read point is fixed at begin (spec 2061 doc 06 §5.3).
func TestSnapshotDoesNotSeeLaterCommit(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 1)

	reader := s.Begin(false) // snapshot before the update
	if err := s.WithTransaction(func(tx *Txn) error {
		return s.Update(tx, rid, doc(2))
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.Get(reader, rid)
	if err != nil {
		t.Fatalf("reader get: %v", err)
	}
	if v := valOf(t, got); v != 1 {
		t.Fatalf("reader saw %d through its snapshot, want 1", v)
	}
	_ = reader.Commit()

	// A fresh snapshot does see it.
	fresh := s.Begin(false)
	got, _ = s.Get(fresh, rid)
	if v := valOf(t, got); v != 2 {
		t.Fatalf("fresh snapshot saw %d, want 2", v)
	}
	_ = fresh.Commit()
}

// Spec §17.2: non-repeatable read is prevented. Two reads in the same
// transaction return the same value despite a committed update in between.
func TestNonRepeatableReadPrevented(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 10)

	tx := s.Begin(false)
	first, _ := s.Get(tx, rid)

	if err := s.WithTransaction(func(w *Txn) error {
		return s.Update(w, rid, doc(99))
	}); err != nil {
		t.Fatalf("interleaved update: %v", err)
	}

	second, _ := s.Get(tx, rid)
	if valOf(t, first) != valOf(t, second) {
		t.Fatalf("non-repeatable read: first %d, second %d", valOf(t, first), valOf(t, second))
	}
	_ = tx.Commit()
}

// Spec §17.4 / §20.1: two transactions that both update the same record from the
// same snapshot conflict; the first to commit wins, the second gets a retriable
// ConflictError. This is lost-update prevention.
func TestWriteWriteConflictFirstCommitterWins(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)

	t1 := s.Begin(true)
	t2 := s.Begin(true) // same snapshot as t1

	if err := s.Update(t1, rid, doc(1)); err != nil {
		t.Fatalf("t1 update: %v", err)
	}
	if err := s.Update(t2, rid, doc(2)); err != nil {
		t.Fatalf("t2 update: %v", err)
	}

	if err := t1.Commit(); err != nil {
		t.Fatalf("t1 commit (should win): %v", err)
	}
	err := t2.Commit()
	if err == nil {
		t.Fatal("t2 commit succeeded, want conflict")
	}
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("t2 error %v, want storage.ErrConflict", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("t2 error %v, want *ConflictError", err)
	}

	final := s.Begin(false)
	got, _ := s.Get(final, rid)
	if v := valOf(t, got); v != 1 {
		t.Fatalf("after conflict record is %d, want 1 (first committer)", v)
	}
	_ = final.Commit()
}

// Spec §17.3: write skew is permitted under snapshot isolation. Two transactions
// reading a shared snapshot and writing disjoint records both commit; SI does not
// promise serializability.
func TestWriteSkewPermitted(t *testing.T) {
	s := NewStore()
	a := seed(t, s, 0)
	b := seed(t, s, 0)

	t1 := s.Begin(true)
	t2 := s.Begin(true)

	// each reads both, writes the one the other did not.
	if _, err := s.Get(t1, a); err != nil {
		t.Fatalf("t1 read a: %v", err)
	}
	if _, err := s.Get(t2, b); err != nil {
		t.Fatalf("t2 read b: %v", err)
	}
	if err := s.Update(t1, a, doc(1)); err != nil {
		t.Fatalf("t1 write a: %v", err)
	}
	if err := s.Update(t2, b, doc(1)); err != nil {
		t.Fatalf("t2 write b: %v", err)
	}

	if err := t1.Commit(); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("t2 commit (disjoint write sets, must succeed): %v", err)
	}
}

// A non-conflicting concurrent write (different record) does not block or fail a
// commit: only an overlapping write set conflicts.
func TestDisjointWritesDoNotConflict(t *testing.T) {
	s := NewStore()
	a := seed(t, s, 0)
	b := seed(t, s, 0)

	t1 := s.Begin(true)
	t2 := s.Begin(true)
	if err := s.Update(t1, a, doc(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t2, b, doc(1)); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("t2: %v", err)
	}
}

func TestDeleteThenReadIsNotFound(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 5)
	if err := s.WithTransaction(func(tx *Txn) error {
		return s.Delete(tx, rid)
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rt := s.Begin(false)
	if _, err := s.Get(rt, rid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	_ = rt.Commit()
}

// read-your-writes: a transaction sees its own buffered insert/update/delete
// before commit, while no other transaction does.
func TestReadYourWrites(t *testing.T) {
	s := NewStore()
	tx := s.Begin(true)
	rid, err := s.Insert(tx, doc(42))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(tx, rid)
	if err != nil {
		t.Fatalf("own-write read: %v", err)
	}
	if v := valOf(t, got); v != 42 {
		t.Fatalf("own-write read %d, want 42", v)
	}

	other := s.Begin(false)
	if _, err := s.Get(other, rid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("other txn saw uncommitted insert: %v", err)
	}
	_ = other.Commit()

	if err := s.Update(tx, rid, doc(43)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(tx, rid)
	if v := valOf(t, got); v != 43 {
		t.Fatalf("own update read %d, want 43", v)
	}
	if err := s.Delete(tx, rid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(tx, rid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("own delete read: %v, want ErrNotFound", err)
	}
	_ = tx.Commit()
}

// A rolled-back transaction leaves no trace: its writes never become visible and
// its snapshot is released.
func TestRollbackLeavesNoTrace(t *testing.T) {
	s := NewStore()
	tx := s.Begin(true)
	rid, err := s.Insert(tx, doc(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	rt := s.Begin(false)
	if _, err := s.Get(rt, rid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("rolled-back insert visible: %v", err)
	}
	_ = rt.Commit()
	if s.o.LiveSnapshots() != 0 {
		t.Fatalf("live snapshots %d after all done, want 0", s.o.LiveSnapshots())
	}
}

func TestCommitOrRollbackTwiceIsRejected(t *testing.T) {
	s := NewStore()
	tx := s.Begin(true)
	if _, err := s.Insert(tx, doc(1)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxnDone) {
		t.Fatalf("second commit: %v, want ErrTxnDone", err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrTxnDone) {
		t.Fatalf("rollback after commit: %v, want ErrTxnDone", err)
	}
}

func TestReadOnlyTxnCannotWrite(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 1)
	ro := s.Begin(false)
	if _, err := s.Insert(ro, doc(2)); !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("insert on read-only: %v, want ErrReadOnly", err)
	}
	if err := s.Update(ro, rid, doc(2)); !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("update on read-only: %v, want ErrReadOnly", err)
	}
	if err := s.Delete(ro, rid); !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("delete on read-only: %v, want ErrReadOnly", err)
	}
	_ = ro.Commit()
}

// WithTransaction retries a conflicting body and eventually commits once the
// racing writer is gone, and gives up with ErrMaxRetries under relentless
// conflict.
func TestWithTransactionRetries(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)

	// One interfering commit, then the retry succeeds.
	interfered := false
	err := s.WithTransaction(func(tx *Txn) error {
		if _, err := s.Get(tx, rid); err != nil {
			return err
		}
		if !interfered {
			interfered = true
			// commit a racing write from the same original snapshot.
			if e := s.WithTransaction(func(w *Txn) error {
				return s.Update(w, rid, doc(1))
			}); e != nil {
				t.Fatalf("interfering commit: %v", e)
			}
		}
		return s.Update(tx, rid, doc(2))
	})
	if err != nil {
		t.Fatalf("WithTransaction should have retried to success: %v", err)
	}
	final := s.Begin(false)
	got, _ := s.Get(final, rid)
	if v := valOf(t, got); v != 2 {
		t.Fatalf("after retry record is %d, want 2", v)
	}
	_ = final.Commit()
}

func TestWithTransactionGivesUp(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)
	err := s.WithTransaction(func(tx *Txn) error {
		if _, err := s.Get(tx, rid); err != nil {
			return err
		}
		// always interfere, so every attempt conflicts.
		if e := s.WithTransaction(func(w *Txn) error {
			return s.Update(w, rid, doc(1))
		}); e != nil {
			t.Fatalf("interfering commit: %v", e)
		}
		return s.Update(tx, rid, doc(2))
	})
	if !errors.Is(err, ErrMaxRetries) {
		t.Fatalf("relentless conflict: %v, want ErrMaxRetries", err)
	}
}

// A non-conflict error from the body aborts immediately without retry.
func TestWithTransactionNonConflictErrorDoesNotRetry(t *testing.T) {
	s := NewStore()
	sentinel := errors.New("boom")
	calls := 0
	err := s.WithTransaction(func(tx *Txn) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err %v, want sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("body ran %d times, want 1 (no retry on non-conflict)", calls)
	}
}
