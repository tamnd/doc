package compat

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestCreateAndListIndex(t *testing.T) {
	c := coll(t, "idx_create")
	ctx := ctxFor(t)

	name, err := c.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "email", Value: 1}},
	})
	if err != nil {
		t.Fatalf("CreateOne: %v", err)
	}
	if name == "" {
		t.Fatal("CreateOne returned an empty index name")
	}

	cur, err := c.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("Indexes().List: %v", err)
	}
	var idx []bson.M
	if err := cur.All(ctx, &idx); err != nil {
		t.Fatalf("list All: %v", err)
	}
	// The _id_ index plus the one just created.
	found := false
	for _, ix := range idx {
		if ix["name"] == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("created index %q not in list %v", name, idx)
	}
}

func TestUniqueIndexRejectsDuplicate(t *testing.T) {
	c := coll(t, "idx_unique")
	ctx := ctxFor(t)
	if _, err := c.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sku", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		t.Fatalf("create unique: %v", err)
	}

	if _, err := c.InsertOne(ctx, bson.D{{Key: "sku", Value: "A1"}}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := c.InsertOne(ctx, bson.D{{Key: "sku", Value: "A1"}})
	if err == nil {
		t.Fatal("duplicate insert under a unique index should fail")
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("duplicate insert error = %v, want a duplicate-key error", err)
	}
}

func TestDropIndex(t *testing.T) {
	c := coll(t, "idx_drop")
	ctx := ctxFor(t)
	name, err := c.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "t", Value: 1}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Indexes().DropOne(ctx, name); err != nil {
		t.Fatalf("DropOne: %v", err)
	}

	cur, err := c.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var idx []bson.M
	if err := cur.All(ctx, &idx); err != nil {
		t.Fatalf("list All: %v", err)
	}
	for _, ix := range idx {
		if ix["name"] == name {
			t.Fatalf("index %q still present after DropOne", name)
		}
	}
}
