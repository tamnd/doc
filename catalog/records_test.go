package catalog

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/bson"
)

func TestDatabaseRecordRoundTrip(t *testing.T) {
	in := &DatabaseRecord{
		Name:        "shop",
		UUID:        [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		CreatedAt:   1_700_000_000_000,
		FormatMinor: 0,
	}
	out := decodeDatabase(encodeDatabase(in))
	if out.Name != in.Name || out.UUID != in.UUID || out.CreatedAt != in.CreatedAt {
		t.Fatalf("round trip mismatch: %+v vs %+v", out, in)
	}
}

func TestCollectionRecordRoundTrip(t *testing.T) {
	validator := bson.NewBuilder().AppendInt32("x", 1).Build()
	in := &CollectionRecord{
		DBName:           "shop",
		Name:             "orders",
		UUID:             [16]byte{9: 1},
		Kind:             KindCapped,
		CreatedAt:        100,
		ModifiedAt:       200,
		CollID:           48,
		IDIndexRoot:      7,
		Validator:        validator,
		ValidationLevel:  ValidationStrict,
		ValidationAction: ValidationWarn,
		Options: CollectionOptions{
			SizeBytes: 1 << 20,
			MaxDocs:   1000,
			HeadPage:  5,
			TailPage:  6,
			PageFill:  80,
		},
		FormatMinor: 0,
	}
	out := decodeCollection(encodeCollection(in))
	if out.DBName != in.DBName || out.Name != in.Name || out.UUID != in.UUID {
		t.Fatalf("identity mismatch: %+v", out)
	}
	if out.Kind != in.Kind || out.CollID != in.CollID || out.IDIndexRoot != in.IDIndexRoot {
		t.Fatalf("kind/root mismatch: %+v", out)
	}
	if out.ValidationLevel != in.ValidationLevel || out.ValidationAction != in.ValidationAction {
		t.Fatalf("validation mismatch: %+v", out)
	}
	if out.Options.SizeBytes != in.Options.SizeBytes ||
		out.Options.MaxDocs != in.Options.MaxDocs ||
		out.Options.HeadPage != in.Options.HeadPage ||
		out.Options.TailPage != in.Options.TailPage ||
		out.Options.PageFill != in.Options.PageFill {
		t.Fatalf("options mismatch: %+v vs %+v", out.Options, in.Options)
	}
	if !bytes.Equal(out.Validator, in.Validator) {
		t.Fatalf("validator mismatch: %x vs %x", out.Validator, in.Validator)
	}
}

func TestCollectionRecordColumnarRoundTrip(t *testing.T) {
	in := &CollectionRecord{
		DBName: "shop",
		Name:   "sales",
		CollID: 64,
		Options: CollectionOptions{
			ColumnarMode:   "transactional",
			ColumnarFields: []string{"region", "units", "ts"},
		},
	}
	out := decodeCollection(encodeCollection(in))
	if out.Options.ColumnarMode != in.Options.ColumnarMode {
		t.Fatalf("columnar mode = %q, want %q", out.Options.ColumnarMode, in.Options.ColumnarMode)
	}
	if len(out.Options.ColumnarFields) != len(in.Options.ColumnarFields) {
		t.Fatalf("columnar fields = %v, want %v", out.Options.ColumnarFields, in.Options.ColumnarFields)
	}
	for i, f := range in.Options.ColumnarFields {
		if out.Options.ColumnarFields[i] != f {
			t.Fatalf("columnar field %d = %q, want %q", i, out.Options.ColumnarFields[i], f)
		}
	}
}

// TestCollectionRecordColumnarEmpty checks the heap-only default round-trips with no
// columnar fields written, so an ordinary collection's record carries nothing extra.
func TestCollectionRecordColumnarEmpty(t *testing.T) {
	in := &CollectionRecord{DBName: "shop", Name: "plain", CollID: 32}
	out := decodeCollection(encodeCollection(in))
	if out.Options.ColumnarMode != "" || len(out.Options.ColumnarFields) != 0 {
		t.Fatalf("expected heap-only options, got %+v", out.Options)
	}
}

func TestSecondaryCollIDDerivation(t *testing.T) {
	r := &CollectionRecord{CollID: 32}
	if r.SecondaryCollID() != 33 {
		t.Fatalf("SecondaryCollID = %d, want 33", r.SecondaryCollID())
	}
}
