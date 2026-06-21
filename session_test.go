package doc

import (
	"context"
	"errors"
	"testing"
)

func TestWithTransactionCommitsAcrossCollections(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	orders := db.Database("shop").Collection("orders")
	inv := db.Database("shop").Collection("inventory")
	if _, err := inv.InsertOne(ctx, M{"sku": "widget", "stock": 10}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)

	_, err = sess.WithTransaction(ctx, func(sctx context.Context) (any, error) {
		if _, e := orders.InsertOne(sctx, M{"_id": "order-1", "sku": "widget", "qty": 1}); e != nil {
			return nil, e
		}
		if _, e := inv.UpdateOne(sctx, M{"sku": "widget"}, M{"$inc": M{"stock": -1}}); e != nil {
			return nil, e
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}

	n, err := orders.CountDocuments(ctx, M{"_id": "order-1"})
	if err != nil || n != 1 {
		t.Fatalf("orders count = %d err %v, want 1", n, err)
	}
	var got struct {
		Stock int `bson:"stock"`
	}
	if err := inv.FindOne(ctx, M{"sku": "widget"}).Decode(&got); err != nil {
		t.Fatalf("inventory FindOne: %v", err)
	}
	if got.Stock != 9 {
		t.Fatalf("stock = %d, want 9", got.Stock)
	}
}

func TestTransactionAbortRollsBackBothCollections(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	a := db.Database("shop").Collection("a")
	b := db.Database("shop").Collection("b")

	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)

	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	sctx := NewSessionContext(ctx, sess)
	if _, err := a.InsertOne(sctx, M{"_id": "a1"}); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := b.InsertOne(sctx, M{"_id": "b1"}); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if err := sess.AbortTransaction(ctx); err != nil {
		t.Fatalf("AbortTransaction: %v", err)
	}

	na, _ := a.CountDocuments(ctx, M{})
	nb, _ := b.CountDocuments(ctx, M{})
	if na != 0 || nb != 0 {
		t.Fatalf("after abort a=%d b=%d, want 0 0", na, nb)
	}
}

func TestTransactionReadYourOwnWrites(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("c")

	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	sctx := NewSessionContext(ctx, sess)

	if _, err := c.InsertOne(sctx, M{"_id": "x", "v": 1}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The write is visible inside the transaction...
	n, err := c.CountDocuments(sctx, M{})
	if err != nil {
		t.Fatalf("count in txn: %v", err)
	}
	if n != 1 {
		t.Fatalf("in-txn count = %d, want 1", n)
	}
	// ...but not to a reader outside it until commit.
	nOut, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("count outside txn: %v", err)
	}
	if nOut != 0 {
		t.Fatalf("outside-txn count = %d, want 0 before commit", nOut)
	}
	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	nOut, _ = c.CountDocuments(ctx, M{})
	if nOut != 1 {
		t.Fatalf("after commit count = %d, want 1", nOut)
	}
}

func TestDoubleStartTransactionRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if err := sess.StartTransaction(); err == nil {
		t.Fatalf("second StartTransaction should fail")
	}
}

func TestCommitWithoutTransactionFails(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)
	if err := sess.CommitTransaction(ctx); err == nil {
		t.Fatalf("CommitTransaction with no open txn should fail")
	}
}

func TestSessionAfterEndIsDisconnected(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	sess.EndSession(ctx)
	if err := sess.StartTransaction(); !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("StartTransaction after EndSession = %v, want ErrClientDisconnected", err)
	}
}

func TestSessionIDStable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)
	id1 := sess.ID()
	id2 := sess.ID()
	if id1["id"] != id2["id"] {
		t.Fatalf("session id not stable: %v vs %v", id1, id2)
	}
}
