package doc

import (
	"reflect"
	"strings"

	"github.com/tamnd/doc/bson"
)

// Raw is a single BSON document as raw bytes, the zero-copy form a document takes
// in the buffer pool. It aliases the engine's bson.Raw so the public API and the
// storage layer share one currency.
type Raw = bson.Raw

// RawValue is a single BSON value: its type tag and the raw payload bytes. It is
// a lazy view into a document buffer and is valid only as long as the owning Raw
// is.
type RawValue = bson.RawValue

// structTag is the parsed form of a `bson:"..."` field tag.
type structTag struct {
	name      string
	skip      bool
	omitEmpty bool
	inline    bool
	minsize   bool
	truncate  bool
}

// parseStructTag reads the bson tag from a struct field, applying the same
// defaults as encoding/json and mongo-go-driver: the default key is the field
// name lowercased, and the comma-separated options after the name select
// behaviors (spec 2061 doc 14 §4.5).
func parseStructTag(sf reflect.StructField) structTag {
	raw, ok := sf.Tag.Lookup("bson")
	if !ok {
		// Fall back to the lowercased field name with no options.
		return structTag{name: strings.ToLower(sf.Name)}
	}
	if raw == "-" {
		return structTag{skip: true}
	}
	parts := strings.Split(raw, ",")
	t := structTag{name: parts[0]}
	if t.name == "" {
		t.name = strings.ToLower(sf.Name)
	}
	for _, opt := range parts[1:] {
		switch opt {
		case "omitempty":
			t.omitEmpty = true
		case "inline":
			t.inline = true
		case "minsize":
			t.minsize = true
		case "truncate":
			t.truncate = true
		}
	}
	return t
}
