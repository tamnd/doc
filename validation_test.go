package doc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tamnd/doc/options"
)

// jsonSchemaRequireAge is a $jsonSchema validator requiring an integer age field.
func jsonSchemaRequireAge() M {
	return M{
		"$jsonSchema": M{
			"bsonType": "object",
			"required": []any{"age"},
			"properties": M{
				"age": M{"bsonType": "int"},
			},
		},
	}
}

func TestCreateCollectionWithValidatorRejectsInsert(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")

	opt := options.CreateCollection().
		SetValidator(jsonSchemaRequireAge()).
		SetValidationLevel("strict").
		SetValidationAction("error")
	if err := d.CreateCollection(ctx, "people", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("people")

	// Missing age is rejected with DocumentValidationFailure (code 121).
	_, err := c.InsertOne(ctx, M{"name": "x"})
	if err == nil {
		t.Fatal("insert of invalid document succeeded, want validation failure")
	}
	if !errors.Is(err, ErrDocumentValidation) {
		t.Fatalf("insert err = %v, want ErrDocumentValidation", err)
	}
	var we WriteException
	if !errors.As(err, &we) || !we.HasErrorCode(codeDocumentValidation) {
		t.Fatalf("insert err = %v, want WriteException with code 121", err)
	}

	// A conforming document is admitted.
	if _, err := c.InsertOne(ctx, M{"name": "y", "age": int32(40)}); err != nil {
		t.Fatalf("insert of valid document: %v", err)
	}
}

func TestCreateCollectionWarnAdmits(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")

	opt := options.CreateCollection().
		SetValidator(jsonSchemaRequireAge()).
		SetValidationAction("warn")
	if err := d.CreateCollection(ctx, "people", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("people")
	if _, err := c.InsertOne(ctx, M{"name": "x"}); err != nil {
		t.Fatalf("warn action should admit invalid insert, got %v", err)
	}
	if n, _ := c.CountDocuments(ctx, M{}); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestCappedCollectionEvictsOldest(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("logs")

	opt := options.CreateCollection().SetCapped(true).SetMaxDocuments(3)
	if err := d.CreateCollection(ctx, "events", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("events")
	for i := range 5 {
		if _, err := c.InsertOne(ctx, M{"seq": int32(i)}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	n, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 3 {
		t.Fatalf("capped count = %d, want 3", n)
	}
}

func TestCappedCollectionRejectsDelete(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("logs")

	opt := options.CreateCollection().SetCapped(true).SetMaxDocuments(10)
	if err := d.CreateCollection(ctx, "events", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("events")
	if _, err := c.InsertOne(ctx, M{"seq": int32(1)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := c.DeleteOne(ctx, M{"seq": int32(1)})
	if err == nil {
		t.Fatal("delete on capped collection succeeded, want rejection")
	}
	var ce CommandError
	if !errors.As(err, &ce) || ce.Code != codeCappedDelete {
		t.Fatalf("delete err = %v, want CappedCollection CommandError", err)
	}
}

func TestTTLSweepExpiresDocuments(t *testing.T) {
	ctx := context.Background()
	// Disable the background sweeper so expiry happens only when we drive it.
	db, err := Open(memoryPath, WithTTLInterval(0))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := db.Database("shop").Collection("sessions")
	_, err = c.Indexes().CreateOne(ctx, IndexModel{
		Keys:    M{"createdAt": 1},
		Options: options.Index().SetExpireAfterSeconds(100),
	})
	if err != nil {
		t.Fatalf("CreateOne index: %v", err)
	}

	old := time.Now().Add(-time.Hour)
	fresh := time.Now()
	if _, err := c.InsertOne(ctx, M{"_id": int32(1), "createdAt": old}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := c.InsertOne(ctx, M{"_id": int32(2), "createdAt": fresh}); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}

	n, err := db.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpired deleted %d, want 1", n)
	}
	if cnt, _ := c.CountDocuments(ctx, M{}); cnt != 1 {
		t.Fatalf("count after sweep = %d, want 1", cnt)
	}
	if err := c.FindOne(ctx, M{"_id": int32(2)}).Err(); err != nil {
		t.Fatalf("fresh document should survive: %v", err)
	}
}
