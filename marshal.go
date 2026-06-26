package doc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/tamnd/doc/bson"
)

// Marshaler is implemented by types that encode themselves to a BSON document.
type Marshaler interface {
	MarshalBSON() ([]byte, error)
}

// ValueMarshaler is implemented by types that encode themselves to a single BSON
// value (type tag plus payload bytes), not a whole document.
type ValueMarshaler interface {
	MarshalBSONValue() (Type, []byte, error)
}

// ErrNotDocument reports a top-level Marshal of a value that is not a document.
var ErrNotDocument = errors.New("doc: value does not marshal to a BSON document")

// Marshal encodes a Go value into a BSON document. The value must be a document
// form: a D, M, struct, pointer to one of those, a Marshaler, or an already
// encoded Raw. It returns the raw BSON bytes (spec 2061 doc 14 §4.4).
func Marshal(val any) ([]byte, error) {
	raw, err := marshalDocument(val)
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// MarshalValue encodes a single Go value into its BSON type tag and payload bytes
// (no element name). It is the value-level counterpart to Marshal.
func MarshalValue(val any) (Type, []byte, error) {
	b := bson.NewBuilder()
	if err := appendValue(b, "v", val); err != nil {
		return 0, nil, err
	}
	raw := b.Build()
	rv, ok := raw.Lookup("v")
	if !ok {
		return 0, nil, ErrNotDocument
	}
	// Copy the payload so it does not alias the builder's scratch buffer.
	data := make([]byte, len(rv.Data))
	copy(data, rv.Data)
	return rv.Type, data, nil
}

// marshalDocument turns a document-shaped value into a Raw.
func marshalDocument(val any) (bson.Raw, error) {
	switch v := val.(type) {
	case nil:
		return nil, ErrNilDocument
	case bson.Raw:
		return v, v.Validate()
	case []byte:
		return bson.Raw(v), bson.Raw(v).Validate()
	case D:
		return marshalD(v)
	case M:
		return marshalM(v)
	case Marshaler:
		data, err := v.MarshalBSON()
		if err != nil {
			return nil, err
		}
		return bson.Raw(data), bson.Raw(data).Validate()
	}

	rv := reflect.ValueOf(val)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, ErrNilDocument
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Struct:
		return marshalStruct(rv)
	case reflect.Map:
		return marshalReflectMap(rv)
	default:
		return nil, fmt.Errorf("%w: %T", ErrNotDocument, val)
	}
}

func marshalD(d D) (bson.Raw, error) {
	b := bson.NewBuilder()
	for _, e := range d {
		if err := appendValue(b, e.Key, e.Value); err != nil {
			return nil, err
		}
	}
	return b.Build(), nil
}

func marshalM(m M) (bson.Raw, error) {
	b := bson.NewBuilder()
	for k, v := range m {
		if err := appendValue(b, k, v); err != nil {
			return nil, err
		}
	}
	return b.Build(), nil
}

func marshalReflectMap(rv reflect.Value) (bson.Raw, error) {
	if rv.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("%w: map key is %s, want string", ErrNotDocument, rv.Type().Key())
	}
	b := bson.NewBuilder()
	for _, mk := range rv.MapKeys() {
		if err := appendReflectValue(b, mk.String(), rv.MapIndex(mk)); err != nil {
			return nil, err
		}
	}
	return b.Build(), nil
}

// appendValue appends one key-value element, dispatching on the concrete Go type
// first and falling back to reflection for structs, slices, maps, and pointers.
func appendValue(b *bson.Builder, key string, val any) error {
	switch v := val.(type) {
	case nil:
		b.AppendNull(key)
	case bool:
		b.AppendBoolean(key, v)
	case int32:
		b.AppendInt32(key, v)
	case int64:
		b.AppendInt64(key, v)
	case int:
		appendInt(b, key, int64(v))
	case float64:
		b.AppendDouble(key, v)
	case float32:
		b.AppendDouble(key, float64(v))
	case string:
		b.AppendString(key, v)
	case ObjectID:
		b.AppendObjectID(key, v)
	case DateTime:
		b.AppendDateTime(key, int64(v))
	case time.Time:
		b.AppendDateTime(key, v.UnixMilli())
	case Binary:
		b.AppendBinary(key, v.Subtype, v.Data)
	case Regex:
		appendRegex(b, key, v)
	case JavaScript:
		appendJavaScript(b, key, string(v))
	case Timestamp:
		b.AppendTimestamp(key, v.uint64())
	case Decimal128:
		appendDecimal128(b, key, v)
	case MinKey:
		b.AppendValue(key, bson.RawValue{Type: bson.TypeMinKey})
	case MaxKey:
		b.AppendValue(key, bson.RawValue{Type: bson.TypeMaxKey})
	case Null:
		b.AppendNull(key)
	case D:
		raw, err := marshalD(v)
		if err != nil {
			return err
		}
		b.AppendDocument(key, raw)
	case M:
		raw, err := marshalM(v)
		if err != nil {
			return err
		}
		b.AppendDocument(key, raw)
	case A:
		return appendArray(b, key, v)
	case bson.Raw:
		b.AppendDocument(key, v)
	case bson.RawValue:
		b.AppendValue(key, v)
	case Marshaler:
		data, err := v.MarshalBSON()
		if err != nil {
			return err
		}
		b.AppendDocument(key, bson.Raw(data))
	case ValueMarshaler:
		t, data, err := v.MarshalBSONValue()
		if err != nil {
			return err
		}
		b.AppendValue(key, bson.RawValue{Type: t, Data: data})
	default:
		return appendReflectValue(b, key, reflect.ValueOf(val))
	}
	return nil
}

// appendInt encodes a platform int as Int32 when it fits, else Int64, matching
// the mongo-go-driver default.
func appendInt(b *bson.Builder, key string, v int64) {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		b.AppendInt32(key, int32(v))
		return
	}
	b.AppendInt64(key, v)
}

func appendRegex(b *bson.Builder, key string, r Regex) {
	data := make([]byte, 0, len(r.Pattern)+len(r.Options)+2)
	data = append(data, r.Pattern...)
	data = append(data, 0x00)
	data = append(data, r.Options...)
	data = append(data, 0x00)
	b.AppendValue(key, bson.RawValue{Type: bson.TypeRegex, Data: data})
}

func appendJavaScript(b *bson.Builder, key, code string) {
	data := make([]byte, 4, len(code)+5)
	binary.LittleEndian.PutUint32(data, uint32(len(code)+1))
	data = append(data, code...)
	data = append(data, 0x00)
	b.AppendValue(key, bson.RawValue{Type: bson.TypeJavaScript, Data: data})
}

func appendDecimal128(b *bson.Builder, key string, d Decimal128) {
	b.AppendValue(key, bson.RawValue{Type: bson.TypeDecimal128, Data: d[:]})
}

func appendArray(b *bson.Builder, key string, a A) error {
	body, err := marshalArray(a)
	if err != nil {
		return err
	}
	b.AppendArray(key, body)
	return nil
}

func marshalArray(a A) (bson.Raw, error) {
	ab := bson.NewBuilder()
	for i, v := range a {
		if err := appendValue(ab, strconv.Itoa(i), v); err != nil {
			return nil, err
		}
	}
	return ab.Build(), nil
}

// appendReflectValue is the reflection fallback for values whose dynamic type is
// not one of the fast-path cases: named structs, slices, arrays, maps, pointers.
func appendReflectValue(b *bson.Builder, key string, rv reflect.Value) error {
	if !rv.IsValid() {
		b.AppendNull(key)
		return nil
	}
	// Unwrap interfaces and pointers, encoding nil pointers as BSON null.
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			b.AppendNull(key)
			return nil
		}
		rv = rv.Elem()
	}
	// Special named types carry an encoding their reflect kind alone would not
	// produce: an ObjectID is a [12]byte, a DateTime an int64, a Raw a byte
	// slice. Route those through the concrete-type encoder, which matches them
	// exactly. Every other type falls to the kind switch below, so a plain named
	// scalar never bounces back here and recurses.
	if rv.CanInterface() && isSpecialMarshalType(rv.Type()) {
		return appendValue(b, key, rv.Interface())
	}
	switch rv.Kind() {
	case reflect.Bool:
		b.AppendBoolean(key, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		appendInt(b, key, rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return fmt.Errorf("doc: uint value %d overflows int64", u)
		}
		appendInt(b, key, int64(u))
	case reflect.Float32, reflect.Float64:
		b.AppendDouble(key, rv.Float())
	case reflect.String:
		b.AppendString(key, rv.String())
	case reflect.Struct:
		raw, err := marshalStruct(rv)
		if err != nil {
			return err
		}
		b.AppendDocument(key, raw)
	case reflect.Map:
		raw, err := marshalReflectMap(rv)
		if err != nil {
			return err
		}
		b.AppendDocument(key, raw)
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// []byte encodes as generic binary.
			b.AppendBinary(key, 0x00, rv.Bytes())
			return nil
		}
		return appendReflectSlice(b, key, rv)
	case reflect.Array:
		return appendReflectSlice(b, key, rv)
	default:
		return fmt.Errorf("doc: cannot marshal value of kind %s", rv.Kind())
	}
	return nil
}

// isSpecialMarshalType reports whether t is one of the named types whose BSON
// encoding differs from what its reflect kind would produce. These all have a
// concrete case in appendValue, so routing them there cannot recurse back into
// appendReflectValue.
func isSpecialMarshalType(t reflect.Type) bool {
	switch t {
	case timeType, objectIDType, dateTimeType, binaryType, regexType,
		javaScriptType, timestampType, decimal128Type, minKeyType, maxKeyType,
		nullType, rawType, rawValueType, dType, mType, aType:
		return true
	default:
		return false
	}
}

func appendReflectSlice(b *bson.Builder, key string, rv reflect.Value) error {
	ab := bson.NewBuilder()
	for i := 0; i < rv.Len(); i++ {
		if err := appendReflectValue(ab, strconv.Itoa(i), rv.Index(i)); err != nil {
			return err
		}
	}
	b.AppendArray(key, ab.Build())
	return nil
}

func marshalStruct(rv reflect.Value) (bson.Raw, error) {
	b := bson.NewBuilder()
	if err := encodeStructFields(b, rv); err != nil {
		return nil, err
	}
	return b.Build(), nil
}

// encodeStructFields writes a struct's fields into b, honoring the bson tags and
// flattening inline fields into the same builder (spec 2061 doc 14 §4.5).
func encodeStructFields(b *bson.Builder, rv reflect.Value) error {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous {
			continue // unexported
		}
		tag := parseStructTag(sf)
		if tag.skip {
			continue
		}
		fv := rv.Field(i)
		if tag.inline {
			if err := encodeInline(b, fv); err != nil {
				return err
			}
			continue
		}
		if tag.omitEmpty && fv.IsZero() {
			continue
		}
		if tag.minsize && isMinsizeInt(fv) {
			appendInt(b, tag.name, fv.Int())
			continue
		}
		if err := appendReflectValue(b, tag.name, fv); err != nil {
			return err
		}
	}
	return nil
}

func encodeInline(b *bson.Builder, fv reflect.Value) error {
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.Struct:
		return encodeStructFields(b, fv)
	case reflect.Map:
		if fv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("doc: inline map key is %s, want string", fv.Type().Key())
		}
		for _, mk := range fv.MapKeys() {
			if err := appendReflectValue(b, mk.String(), fv.MapIndex(mk)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("doc: inline applied to non-struct, non-map field of kind %s", fv.Kind())
	}
}

func isMinsizeInt(fv reflect.Value) bool {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	default:
		return false
	}
}
