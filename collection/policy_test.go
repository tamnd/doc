package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/schema"
)

// compileValidator builds a validator from a raw MQL/JSON-Schema document and fails
// the test if it does not compile, so each case reads as the validator it installs.
func compileValidator(t *testing.T, raw bson.Raw) *schema.Validator {
	t.Helper()
	v, err := schema.Compile(raw)
	if err != nil {
		t.Fatalf("compile validator: %v", err)
	}
	return v
}

// requiredAgeSchema is a $jsonSchema that requires an integer age field, the
// smallest validator that exercises bsonType plus required.
func requiredAgeSchema(t *testing.T) bson.Raw {
	t.Helper()
	props := bson.NewBuilder().
		AppendDocument("age", bson.NewBuilder().AppendString("bsonType", "int").Build()).
		Build()
	js := bson.NewBuilder().
		AppendString("bsonType", "object").
		AppendArray("required", bson.NewBuilder().AppendString("0", "age").Build()).
		AppendDocument("properties", props).
		Build()
	return bson.NewBuilder().AppendDocument("$jsonSchema", js).Build()
}

func TestInsertRejectedByValidator(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{
		Validator:        compileValidator(t, requiredAgeSchema(t)),
		ValidationLevel:  catalog.ValidationStrict,
		ValidationAction: catalog.ValidationError,
	})

	// Missing the required age field is rejected.
	bad := bson.NewBuilder().AppendInt32("_id", 1).AppendString("name", "x").Build()
	if _, err := c.InsertOne(bad); !errors.Is(err, ErrDocumentValidation) {
		t.Fatalf("insert of invalid doc: got %v, want ErrDocumentValidation", err)
	}

	// A conforming document is accepted.
	good := bson.NewBuilder().AppendInt32("_id", 2).AppendInt32("age", 30).Build()
	if _, err := c.InsertOne(good); err != nil {
		t.Fatalf("insert of valid doc: %v", err)
	}
	if got, _ := c.FindOne(filterID(2)); got == nil {
		t.Fatal("valid document was not stored")
	}
}

func TestValidatorWarnActionAdmits(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{
		Validator:        compileValidator(t, requiredAgeSchema(t)),
		ValidationLevel:  catalog.ValidationStrict,
		ValidationAction: catalog.ValidationWarn,
	})
	bad := bson.NewBuilder().AppendInt32("_id", 1).Build()
	if _, err := c.InsertOne(bad); err != nil {
		t.Fatalf("warn action should admit invalid insert, got %v", err)
	}
	if got, _ := c.FindOne(filterID(1)); got == nil {
		t.Fatal("warn action dropped the document")
	}
}

func TestValidationOffSkips(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{
		Validator:        compileValidator(t, requiredAgeSchema(t)),
		ValidationLevel:  catalog.ValidationOff,
		ValidationAction: catalog.ValidationError,
	})
	bad := bson.NewBuilder().AppendInt32("_id", 1).Build()
	if _, err := c.InsertOne(bad); err != nil {
		t.Fatalf("validation off should admit, got %v", err)
	}
}

func TestModerateSkipsUpdateOfInvalidPreImage(t *testing.T) {
	c := newTestColl(t)
	// Seed a non-conforming document with validation off, then attach a moderate
	// validator. An update that leaves it non-conforming must still be allowed.
	c.SetPolicy(Policy{ValidationLevel: catalog.ValidationOff})
	seed := bson.NewBuilder().AppendInt32("_id", 1).AppendString("name", "x").Build()
	if _, err := c.InsertOne(seed); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	c.SetPolicy(Policy{
		Validator:        compileValidator(t, requiredAgeSchema(t)),
		ValidationLevel:  catalog.ValidationModerate,
		ValidationAction: catalog.ValidationError,
	})
	set := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendString("name", "y").Build()).
		Build()
	if _, err := c.UpdateOne(filterID(1), set); err != nil {
		t.Fatalf("moderate update of invalid pre-image: got %v, want allowed", err)
	}
}

func TestStrictBlocksUpdateOfInvalidPreImage(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{ValidationLevel: catalog.ValidationOff})
	seed := bson.NewBuilder().AppendInt32("_id", 1).AppendString("name", "x").Build()
	if _, err := c.InsertOne(seed); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	c.SetPolicy(Policy{
		Validator:        compileValidator(t, requiredAgeSchema(t)),
		ValidationLevel:  catalog.ValidationStrict,
		ValidationAction: catalog.ValidationError,
	})
	set := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendString("name", "y").Build()).
		Build()
	if _, err := c.UpdateOne(filterID(1), set); !errors.Is(err, ErrDocumentValidation) {
		t.Fatalf("strict update of invalid pre-image: got %v, want ErrDocumentValidation", err)
	}
}

func TestCappedEvictsOldest(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{Capped: true, CappedMaxDocs: 3})
	for i := int32(1); i <= 5; i++ {
		if _, err := c.InsertOne(docID(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Only the last three insertions survive; the first two were evicted.
	for _, id := range []int32{1, 2} {
		if got, _ := c.FindOne(filterID(id)); got != nil {
			t.Fatalf("_id %d should have been evicted", id)
		}
	}
	for _, id := range []int32{3, 4, 5} {
		if got, _ := c.FindOne(filterID(id)); got == nil {
			t.Fatalf("_id %d should still be present", id)
		}
	}
	if n, _ := c.CountDocuments(nil); n != 3 {
		t.Fatalf("capped count: got %d, want 3", n)
	}
}

func TestCappedRejectsDelete(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{Capped: true, CappedMaxDocs: 10})
	if _, err := c.InsertOne(docID(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := c.DeleteOne(filterID(1)); !errors.Is(err, ErrCappedDelete) {
		t.Fatalf("delete on capped: got %v, want ErrCappedDelete", err)
	}
}

func TestCappedRejectsGrowingUpdate(t *testing.T) {
	c := newTestColl(t)
	c.SetPolicy(Policy{Capped: true, CappedMaxDocs: 10})
	seed := bson.NewBuilder().AppendInt32("_id", 1).AppendString("s", "a").Build()
	if _, err := c.InsertOne(seed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Replace the short string with a much longer one: the document grows.
	grow := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendString("s", "aaaaaaaaaaaaaaaaaaaa").Build()).
		Build()
	if _, err := c.UpdateOne(filterID(1), grow); !errors.Is(err, ErrCappedGrow) {
		t.Fatalf("growing update on capped: got %v, want ErrCappedGrow", err)
	}
}

func TestCappedMaxBytesEviction(t *testing.T) {
	c := newTestColl(t)
	// Each docID is the same small size; cap the byte budget to roughly two of them.
	one := docID(1)
	c.SetPolicy(Policy{Capped: true, CappedMaxBytes: int64(len(one)) * 2})
	for i := int32(1); i <= 4; i++ {
		if _, err := c.InsertOne(docID(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	n, _ := c.CountDocuments(nil)
	if n > 2 {
		t.Fatalf("capped byte budget held %d docs, want at most 2", n)
	}
	// The most recent insert always survives.
	if got, _ := c.FindOne(filterID(4)); got == nil {
		t.Fatal("most recent insert was evicted")
	}
}
