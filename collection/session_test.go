package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/storage"
)

// readN returns the int32 value of field n in the document matching _id, or -1 when
// the document is absent.
func readN(t *testing.T, tx *Txn, id int32) int32 {
	t.Helper()
	doc, err := tx.FindOne(filterID(id))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if doc == nil {
		return -1
	}
	v, ok := doc.Lookup("n")
	if !ok {
		t.Fatalf("document _id=%d has no field n", id)
	}
	return v.Int32()
}

func TestWithTransactionCommitsAtomically(t *testing.T) {
	c := newTestColl(t)
	err := c.WithTransaction(func(tx *Txn) error {
		if _, err := tx.InsertOne(docInt(1, "n", 10)); err != nil {
			return err
		}
		if _, err := tx.InsertOne(docInt(2, "n", 20)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}
	if n, _ := c.CountDocuments(nil); n != 2 {
		t.Fatalf("count after commit: got %d, want 2", n)
	}
}

func TestWithTransactionAbortDiscardsWrites(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	sentinel := errors.New("boom")
	err := c.WithTransaction(func(tx *Txn) error {
		if _, err := tx.InsertOne(docInt(2, "n", 20)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTransaction error: got %v, want sentinel", err)
	}
	if n, _ := c.CountDocuments(nil); n != 1 {
		t.Fatalf("count after abort: got %d, want 1 (the insert must be rolled back)", n)
	}
}

// TestWithTransactionRetriesOnConflict drives a deterministic write-write conflict:
// on the first attempt the body injects a concurrent committed write to the same
// _id before its own write, so commit conflicts and the body runs again against a
// fresh snapshot that includes the injected write.
func TestWithTransactionRetriesOnConflict(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 0))
	attempts := 0
	err := c.WithTransaction(func(tx *Txn) error {
		attempts++
		n := readN(t, tx, 1)
		if attempts == 1 {
			// A separate committed transaction writes _id=1, invalidating this
			// transaction's snapshot of it.
			if _, err := c.UpdateOne(filterID(1), setDoc("n", 99)); err != nil {
				return err
			}
		}
		_, err := tx.UpdateOne(filterID(1), setDoc("n", n+1))
		return err
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts: got %d, want 2 (one conflict then one success)", attempts)
	}
	// Second attempt read n=99 (the injected write) and set 100.
	if got, _ := c.FindOne(filterID(1)); got != nil {
		if v, _ := got.Lookup("n"); v.Int32() != 100 {
			t.Fatalf("final n: got %d, want 100", v.Int32())
		}
	}
}

// TestCommitConflictSurfacesWriteConflictError checks that two transactions writing
// the same document surface a retriable WriteConflictError to the second committer.
func TestCommitConflictSurfacesWriteConflictError(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 0))

	t1 := c.Begin()
	t2 := c.Begin()
	if _, err := t1.UpdateOne(filterID(1), setDoc("n", 1)); err != nil {
		t.Fatalf("t1 update: %v", err)
	}
	if _, err := t2.UpdateOne(filterID(1), setDoc("n", 2)); err != nil {
		t.Fatalf("t2 update: %v", err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	err := mapCommitErr(t2.Commit())
	if !IsRetriable(err) {
		t.Fatalf("t2 commit error: got %v, want a retriable conflict", err)
	}
	var wce *WriteConflictError
	if !errors.As(err, &wce) {
		t.Fatalf("t2 commit error: got %T, want *WriteConflictError", err)
	}
}

// TestDuplicateKeyNotRetriable checks that a duplicate-key violation is not treated
// as retriable and is not retried by WithTransaction.
func TestDuplicateKeyNotRetriable(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	attempts := 0
	err := c.WithTransaction(func(tx *Txn) error {
		attempts++
		_, err := tx.InsertOne(docInt(1, "n", 11)) // _id=1 already exists
		return err
	})
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("WithTransaction error: got %v, want duplicate key", err)
	}
	if IsRetriable(err) {
		t.Fatal("duplicate key must not be retriable")
	}
	if attempts != 1 {
		t.Fatalf("attempts: got %d, want 1 (duplicate key is not retried)", attempts)
	}
}

func TestExplicitTransactionLifecycle(t *testing.T) {
	c := newTestColl(t)
	s := c.StartSession()
	defer s.EndSession()

	if err := s.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if err := s.StartTransaction(); !errors.Is(err, ErrTxnInProgress) {
		t.Fatalf("second StartTransaction: got %v, want ErrTxnInProgress", err)
	}
	if _, err := s.Transaction().InsertOne(docInt(1, "n", 10)); err != nil {
		t.Fatalf("insert in txn: %v", err)
	}
	// Not yet committed: a fresh read outside the transaction sees nothing.
	if got, _ := c.FindOne(filterID(1)); got != nil {
		t.Fatal("uncommitted insert is visible outside the transaction")
	}
	if err := s.CommitTransaction(); err != nil {
		t.Fatalf("CommitTransaction: %v", err)
	}
	if got, _ := c.FindOne(filterID(1)); got == nil {
		t.Fatal("committed insert is not visible")
	}
	if err := s.CommitTransaction(); !errors.Is(err, ErrNoTxn) {
		t.Fatalf("commit with no txn: got %v, want ErrNoTxn", err)
	}
}

func TestAbortTransactionDiscards(t *testing.T) {
	c := newTestColl(t)
	s := c.StartSession()
	defer s.EndSession()
	if err := s.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if _, err := s.Transaction().InsertOne(docInt(1, "n", 10)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.AbortTransaction(); err != nil {
		t.Fatalf("AbortTransaction: %v", err)
	}
	if n, _ := c.CountDocuments(nil); n != 0 {
		t.Fatalf("count after abort: got %d, want 0", n)
	}
	if err := s.AbortTransaction(); !errors.Is(err, ErrNoTxn) {
		t.Fatalf("abort with no txn: got %v, want ErrNoTxn", err)
	}
}
