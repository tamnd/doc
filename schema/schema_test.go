package schema

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
)

// doc builds a BSON document from alternating key/value pairs for the tests.
func doc(t *testing.T, kv ...any) bson.Raw {
	t.Helper()
	b := bson.NewBuilder()
	for i := 0; i+1 < len(kv); i += 2 {
		appendAny(b, kv[i].(string), kv[i+1])
	}
	return b.Build()
}

func appendAny(b *bson.Builder, key string, v any) {
	switch x := v.(type) {
	case string:
		b.AppendString(key, x)
	case int:
		b.AppendInt32(key, int32(x))
	case int32:
		b.AppendInt32(key, x)
	case int64:
		b.AppendInt64(key, x)
	case float64:
		b.AppendDouble(key, x)
	case bool:
		b.AppendBoolean(key, x)
	case bson.Raw:
		b.AppendDocument(key, x)
	case []any:
		arr := bson.NewBuilder()
		for i, e := range x {
			appendAny(arr, itoa(i), e)
		}
		b.AppendArray(key, arr.Build())
	case nil:
		b.AppendNull(key)
	default:
		panic("unsupported test value")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func jsonSchema(t *testing.T, s bson.Raw) *Validator {
	t.Helper()
	v, err := Compile(doc(t, "$jsonSchema", s))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return v
}

func TestRequiredAndBsonType(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"required", []any{"name", "age"},
		"properties", doc(t,
			"name", doc(t, "bsonType", "string", "minLength", 1),
			"age", doc(t, "bsonType", "int", "minimum", 0, "maximum", 150),
		),
	))

	if err := v.Validate(doc(t, "name", "ada", "age", 36)); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if err := v.Validate(doc(t, "name", "ada")); err == nil {
		t.Fatalf("missing required age should fail")
	}
	if err := v.Validate(doc(t, "name", "", "age", 36)); err == nil {
		t.Fatalf("empty name should fail minLength")
	}
	if err := v.Validate(doc(t, "name", "ada", "age", 200)); err == nil {
		t.Fatalf("age over maximum should fail")
	}
	if err := v.Validate(doc(t, "name", 5, "age", 36)); err == nil {
		t.Fatalf("name of wrong type should fail")
	}
}

func TestAdditionalPropertiesFalse(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"properties", doc(t, "a", doc(t, "bsonType", "int")),
		"additionalProperties", false,
	))
	if err := v.Validate(doc(t, "a", 1)); err != nil {
		t.Fatalf("declared property rejected: %v", err)
	}
	if err := v.Validate(doc(t, "a", 1, "b", 2)); err == nil {
		t.Fatalf("undeclared property b should fail")
	}
}

func TestEnumAndPattern(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"properties", doc(t,
			"status", doc(t, "enum", []any{"active", "inactive"}),
			"email", doc(t, "bsonType", "string", "pattern", "^[^@]+@[^@]+$"),
		),
	))
	if err := v.Validate(doc(t, "status", "active", "email", "a@b")); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	if err := v.Validate(doc(t, "status", "deleted")); err == nil {
		t.Fatalf("status not in enum should fail")
	}
	if err := v.Validate(doc(t, "email", "nope")); err == nil {
		t.Fatalf("email failing pattern should fail")
	}
}

func TestArrayConstraints(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"properties", doc(t,
			"tags", doc(t,
				"bsonType", "array",
				"minItems", 1,
				"maxItems", 3,
				"uniqueItems", true,
				"items", doc(t, "bsonType", "string"),
			),
		),
	))
	if err := v.Validate(doc(t, "tags", []any{"a", "b"})); err != nil {
		t.Fatalf("valid array rejected: %v", err)
	}
	if err := v.Validate(doc(t, "tags", []any{})); err == nil {
		t.Fatalf("empty array should fail minItems")
	}
	if err := v.Validate(doc(t, "tags", []any{"a", "b", "c", "d"})); err == nil {
		t.Fatalf("over maxItems should fail")
	}
	if err := v.Validate(doc(t, "tags", []any{"a", "a"})); err == nil {
		t.Fatalf("duplicate items should fail")
	}
	if err := v.Validate(doc(t, "tags", []any{1})); err == nil {
		t.Fatalf("non-string item should fail")
	}
}

func TestLogicalCombinators(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"properties", doc(t,
			"v", doc(t, "anyOf", []any{
				doc(t, "bsonType", "string"),
				doc(t, "bsonType", "int", "minimum", 10),
			}),
		),
	))
	if err := v.Validate(doc(t, "v", "hi")); err != nil {
		t.Fatalf("string should satisfy anyOf: %v", err)
	}
	if err := v.Validate(doc(t, "v", 12)); err != nil {
		t.Fatalf("int>=10 should satisfy anyOf: %v", err)
	}
	if err := v.Validate(doc(t, "v", 3)); err == nil {
		t.Fatalf("int<10 and not string should fail anyOf")
	}
}

func TestNestedProperties(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"bsonType", "object",
		"properties", doc(t,
			"addr", doc(t,
				"bsonType", "object",
				"required", []any{"zip"},
				"properties", doc(t, "zip", doc(t, "bsonType", "string")),
			),
		),
	))
	if err := v.Validate(doc(t, "addr", doc(t, "zip", "12345"))); err != nil {
		t.Fatalf("valid nested rejected: %v", err)
	}
	err := v.Validate(doc(t, "addr", doc(t, "city", "x")))
	if err == nil {
		t.Fatalf("missing nested required should fail")
	}
	var f *Failure
	if !errors.As(err, &f) || f.Path != "addr.zip" {
		t.Fatalf("expected failure path addr.zip, got %v", err)
	}
}

func TestQueryExpressionValidator(t *testing.T) {
	v, err := Compile(doc(t,
		"age", doc(t, "$gte", 0),
		"status", doc(t, "$in", []any{"active", "inactive"}),
	))
	if err != nil {
		t.Fatalf("compile query validator: %v", err)
	}
	if err := v.Validate(doc(t, "age", 5, "status", "active")); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	if err := v.Validate(doc(t, "age", 5, "status", "banned")); err == nil {
		t.Fatalf("status not in set should fail")
	}
}

func TestEmptyValidatorAcceptsAll(t *testing.T) {
	v, err := Compile(nil)
	if err != nil {
		t.Fatalf("compile nil: %v", err)
	}
	if !v.Empty() {
		t.Fatalf("nil validator should be empty")
	}
	if err := v.Validate(doc(t, "anything", 1)); err != nil {
		t.Fatalf("empty validator should accept: %v", err)
	}
}

func TestIntegerTypeAcceptsWholeDouble(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"properties", doc(t, "n", doc(t, "type", "integer")),
	))
	if err := v.Validate(doc(t, "n", int32(7))); err != nil {
		t.Fatalf("int should match integer: %v", err)
	}
	if err := v.Validate(doc(t, "n", 5.0)); err != nil {
		t.Fatalf("whole-valued double should match integer: %v", err)
	}
	if err := v.Validate(doc(t, "n", 5.5)); err == nil {
		t.Fatalf("fractional double should not match integer")
	}
}

func TestNumberAlias(t *testing.T) {
	v := jsonSchema(t, doc(t,
		"properties", doc(t, "n", doc(t, "bsonType", "number")),
	))
	if err := v.Validate(doc(t, "n", 1.5)); err != nil {
		t.Fatalf("double should match number: %v", err)
	}
	if err := v.Validate(doc(t, "n", int64(7))); err != nil {
		t.Fatalf("long should match number: %v", err)
	}
	if err := v.Validate(doc(t, "n", "x")); err == nil {
		t.Fatalf("string should not match number")
	}
}
