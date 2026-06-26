package compat

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestTransactionCommitPersists(t *testing.T) {
	c := coll(t, "txn_commit")
	ctx := ctxFor(t)

	sess, err := client.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)

	_, err = sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		if _, err := c.InsertOne(sc, bson.D{{Key: "n", Value: 1}}); err != nil {
			return nil, err
		}
		if _, err := c.InsertOne(sc, bson.D{{Key: "n", Value: 2}}); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}

	got, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Fatalf("after commit count = %d, want 2", got)
	}
}

func TestTransactionAbortDiscards(t *testing.T) {
	c := coll(t, "txn_abort")
	ctx := ctxFor(t)

	sess, err := client.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)

	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	sc := mongo.NewSessionContext(ctx, sess)
	if _, err := c.InsertOne(sc, bson.D{{Key: "n", Value: 99}}); err != nil {
		t.Fatalf("insert in txn: %v", err)
	}
	if err := sess.AbortTransaction(ctx); err != nil {
		t.Fatalf("AbortTransaction: %v", err)
	}

	got, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Fatalf("after abort count = %d, want 0 (the write was discarded)", got)
	}
}

func TestTransactionReadYourWrites(t *testing.T) {
	c := coll(t, "txn_ryow")
	ctx := ctxFor(t)

	sess, err := client.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)

	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	sc := mongo.NewSessionContext(ctx, sess)
	if _, err := c.InsertOne(sc, bson.D{{Key: "k", Value: "v"}}); err != nil {
		t.Fatalf("insert in txn: %v", err)
	}

	// Inside the same transaction the write is visible.
	inside, err := c.CountDocuments(sc, bson.D{})
	if err != nil {
		t.Fatalf("count inside txn: %v", err)
	}
	if inside != 1 {
		t.Fatalf("read-your-writes count inside txn = %d, want 1", inside)
	}

	// A read outside the transaction does not yet see the uncommitted write.
	outside, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count outside txn: %v", err)
	}
	if outside != 0 {
		t.Fatalf("count outside the open txn = %d, want 0", outside)
	}

	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatalf("CommitTransaction: %v", err)
	}
	after, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if after != 1 {
		t.Fatalf("count after commit = %d, want 1", after)
	}
}
