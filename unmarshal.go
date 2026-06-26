package doc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
)

// Unmarshaler is implemented by types that decode themselves from a BSON
// document.
type Unmarshaler interface {
	UnmarshalBSON([]byte) error
}

// ValueUnmarshaler is implemented by types that decode themselves from a single
// BSON value.
type ValueUnmarshaler interface {
	UnmarshalBSONValue(Type, []byte) error
}

// ErrDecodeTarget reports an Unmarshal target that is not a non-nil pointer.
var ErrDecodeTarget = errors.New("doc: Unmarshal target must be a non-nil pointer")

var (
	timeType       = reflect.TypeFor[time.Time]()
	objectIDType   = reflect.TypeFor[ObjectID]()
	dateTimeType   = reflect.TypeFor[DateTime]()
	binaryType     = reflect.TypeFor[Binary]()
	regexType      = reflect.TypeFor[Regex]()
	javaScriptType = reflect.TypeFor[JavaScript]()
	timestampType  = reflect.TypeFor[Timestamp]()
	decimal128Type = reflect.TypeFor[Decimal128]()
	minKeyType     = reflect.TypeFor[MinKey]()
	maxKeyType     = reflect.TypeFor[MaxKey]()
	nullType       = reflect.TypeFor[Null]()
	rawType        = reflect.TypeFor[Raw]()
	rawValueType   = reflect.TypeFor[RawValue]()
	dType          = reflect.TypeFor[D]()
	mType          = reflect.TypeFor[M]()
	aType          = reflect.TypeFor[A]()
)

// Unmarshal decodes a BSON document into a Go value. The target must be a non-nil
// pointer: to a struct, to a map, to D, M, A, Raw, or to an interface (spec 2061
// doc 14 §4.4).
func Unmarshal(data []byte, val any) error {
	raw := bson.Raw(data)
	if err := raw.Validate(); err != nil {
		return err
	}
	return unmarshalDocument(raw, val)
}

// UnmarshalValue decodes a single BSON value into a Go value.
func UnmarshalValue(t Type, data []byte, val any) error {
	rv := reflect.ValueOf(val)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrDecodeTarget
	}
	return decodeValue(bson.RawValue{Type: t, Data: data}, rv.Elem())
}

func unmarshalDocument(raw bson.Raw, val any) error {
	// Fast paths for the document container types.
	switch t := val.(type) {
	case *Raw:
		*t = raw.Clone()
		return nil
	case *M:
		m, err := decodeM(raw)
		if err != nil {
			return err
		}
		*t = m
		return nil
	case *D:
		d, err := decodeD(raw)
		if err != nil {
			return err
		}
		*t = d
		return nil
	case Unmarshaler:
		return t.UnmarshalBSON([]byte(raw))
	}

	rv := reflect.ValueOf(val)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrDecodeTarget
	}
	return decodeValue(bson.RawValue{Type: bson.TypeDocument, Data: raw}, rv.Elem())
}

// decodeM decodes a document into an M, with nested documents as M and arrays as
// A, the schema-agnostic shape callers iterate.
func decodeM(raw bson.Raw) (M, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	m := make(M, len(elems))
	for _, e := range elems {
		v, err := decodeNatural(e.Value)
		if err != nil {
			return nil, err
		}
		m[e.Key] = v
	}
	return m, nil
}

func decodeD(raw bson.Raw) (D, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	d := make(D, 0, len(elems))
	for _, e := range elems {
		v, err := decodeNatural(e.Value)
		if err != nil {
			return nil, err
		}
		d = append(d, E{Key: e.Key, Value: v})
	}
	return d, nil
}

func decodeA(raw bson.Raw) (A, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	a := make(A, 0, len(elems))
	for _, e := range elems {
		v, err := decodeNatural(e.Value)
		if err != nil {
			return nil, err
		}
		a = append(a, v)
	}
	return a, nil
}

// decodeNatural maps a BSON value to its natural Go type, the inverse of the
// fast-path encoder (spec 2061 doc 14 §4.1).
func decodeNatural(rv bson.RawValue) (any, error) {
	switch rv.Type {
	case bson.TypeDouble:
		return rv.Double(), nil
	case bson.TypeString:
		return rv.StringValue(), nil
	case bson.TypeDocument:
		return decodeM(bson.Raw(rv.Data))
	case bson.TypeArray:
		return decodeA(bson.Raw(rv.Data))
	case bson.TypeBinary:
		st, data, _ := rv.Binary()
		cp := make([]byte, len(data))
		copy(cp, data)
		return Binary{Subtype: st, Data: cp}, nil
	case bson.TypeObjectID:
		return rv.ObjectID(), nil
	case bson.TypeBoolean:
		return rv.Boolean(), nil
	case bson.TypeDateTime:
		return DateTime(rv.DateTime()), nil
	case bson.TypeNull, bson.TypeUndefined:
		return nil, nil
	case bson.TypeRegex:
		p, o, _ := rv.Regex()
		return Regex{Pattern: p, Options: o}, nil
	case bson.TypeJavaScript:
		return JavaScript(jsValue(rv)), nil
	case bson.TypeInt32:
		return rv.Int32(), nil
	case bson.TypeTimestamp:
		return timestampFromUint64(rv.Timestamp()), nil
	case bson.TypeInt64:
		return rv.Int64(), nil
	case bson.TypeDecimal128:
		return Decimal128(rv.Decimal128()), nil
	case bson.TypeMinKey:
		return MinKey{}, nil
	case bson.TypeMaxKey:
		return MaxKey{}, nil
	default:
		return nil, fmt.Errorf("doc: cannot decode BSON type %s", rv.Type)
	}
}

// jsValue extracts the code string from a JavaScript value payload (int32 length
// prefix, string bytes, trailing NUL).
func jsValue(rv bson.RawValue) string {
	if len(rv.Data) < 5 {
		return ""
	}
	n := int(binary.LittleEndian.Uint32(rv.Data))
	if n < 1 || 4+n > len(rv.Data) {
		return ""
	}
	return string(rv.Data[4 : 4+n-1])
}

// decodeValue sets target from a BSON value, converting between numeric types and
// recursing into documents and arrays as the target's kind requires.
func decodeValue(rv bson.RawValue, target reflect.Value) error {
	// Honor custom value unmarshalers on addressable targets.
	if target.CanAddr() {
		if vu, ok := target.Addr().Interface().(ValueUnmarshaler); ok {
			return vu.UnmarshalBSONValue(rv.Type, rv.Data)
		}
		if rv.Type == bson.TypeDocument {
			if u, ok := target.Addr().Interface().(Unmarshaler); ok {
				return u.UnmarshalBSON(rv.Data)
			}
		}
	}

	// Null clears the target to its zero value (nil for pointers, maps, slices).
	if rv.Type == bson.TypeNull || rv.Type == bson.TypeUndefined {
		target.Set(reflect.Zero(target.Type()))
		return nil
	}

	// Concrete named types we recognize directly.
	switch target.Type() {
	case timeType:
		if rv.Type == bson.TypeDateTime {
			target.Set(reflect.ValueOf(time.UnixMilli(rv.DateTime()).UTC()))
			return nil
		}
	case objectIDType:
		if rv.Type == bson.TypeObjectID {
			target.Set(reflect.ValueOf(rv.ObjectID()))
			return nil
		}
	case binaryType:
		st, data, _ := rv.Binary()
		cp := make([]byte, len(data))
		copy(cp, data)
		target.Set(reflect.ValueOf(Binary{Subtype: st, Data: cp}))
		return nil
	case regexType:
		p, o, _ := rv.Regex()
		target.Set(reflect.ValueOf(Regex{Pattern: p, Options: o}))
		return nil
	case timestampType:
		target.Set(reflect.ValueOf(timestampFromUint64(rv.Timestamp())))
		return nil
	case decimal128Type:
		target.Set(reflect.ValueOf(Decimal128(rv.Decimal128())))
		return nil
	case minKeyType:
		target.Set(reflect.ValueOf(MinKey{}))
		return nil
	case maxKeyType:
		target.Set(reflect.ValueOf(MaxKey{}))
		return nil
	case nullType:
		target.Set(reflect.ValueOf(Null{}))
		return nil
	}

	switch target.Kind() {
	case reflect.Interface:
		nv, err := decodeNatural(rv)
		if err != nil {
			return err
		}
		if nv == nil {
			target.Set(reflect.Zero(target.Type()))
			return nil
		}
		target.Set(reflect.ValueOf(nv))
		return nil
	case reflect.Bool:
		b, ok := rv.Boolean(), rv.Type == bson.TypeBoolean
		if !ok {
			return typeErr(rv.Type, target)
		}
		target.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, ok := toInt64(rv)
		if !ok {
			return typeErr(rv.Type, target)
		}
		target.SetInt(i)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		i, ok := toInt64(rv)
		if !ok || i < 0 {
			return typeErr(rv.Type, target)
		}
		target.SetUint(uint64(i))
		return nil
	case reflect.Float32, reflect.Float64:
		f, ok := toFloat64(rv)
		if !ok {
			return typeErr(rv.Type, target)
		}
		target.SetFloat(f)
		return nil
	case reflect.String:
		switch rv.Type {
		case bson.TypeString:
			target.SetString(rv.StringValue())
		case bson.TypeJavaScript:
			target.SetString(jsValue(rv))
		default:
			return typeErr(rv.Type, target)
		}
		return nil
	case reflect.Slice:
		return decodeSlice(rv, target)
	case reflect.Array:
		return decodeArray(rv, target)
	case reflect.Map:
		return decodeMapInto(rv, target)
	case reflect.Struct:
		if rv.Type != bson.TypeDocument {
			return typeErr(rv.Type, target)
		}
		return decodeStruct(bson.Raw(rv.Data), target)
	case reflect.Pointer:
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		return decodeValue(rv, target.Elem())
	default:
		return fmt.Errorf("doc: cannot decode into %s", target.Type())
	}
}

func decodeSlice(rv bson.RawValue, target reflect.Value) error {
	// []byte from a binary value.
	if target.Type().Elem().Kind() == reflect.Uint8 && rv.Type == bson.TypeBinary {
		_, data, _ := rv.Binary()
		cp := make([]byte, len(data))
		copy(cp, data)
		target.SetBytes(cp)
		return nil
	}
	if rv.Type != bson.TypeArray {
		return typeErr(rv.Type, target)
	}
	elems, err := bson.Raw(rv.Data).Elements()
	if err != nil {
		return err
	}
	out := reflect.MakeSlice(target.Type(), len(elems), len(elems))
	for i, e := range elems {
		if err := decodeValue(e.Value, out.Index(i)); err != nil {
			return err
		}
	}
	target.Set(out)
	return nil
}

func decodeArray(rv bson.RawValue, target reflect.Value) error {
	if rv.Type != bson.TypeArray {
		return typeErr(rv.Type, target)
	}
	elems, err := bson.Raw(rv.Data).Elements()
	if err != nil {
		return err
	}
	n := target.Len()
	for i := 0; i < n && i < len(elems); i++ {
		if err := decodeValue(elems[i].Value, target.Index(i)); err != nil {
			return err
		}
	}
	return nil
}

func decodeMapInto(rv bson.RawValue, target reflect.Value) error {
	if rv.Type != bson.TypeDocument {
		return typeErr(rv.Type, target)
	}
	if target.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("doc: map key is %s, want string", target.Type().Key())
	}
	elems, err := bson.Raw(rv.Data).Elements()
	if err != nil {
		return err
	}
	if target.IsNil() {
		target.Set(reflect.MakeMapWithSize(target.Type(), len(elems)))
	}
	et := target.Type().Elem()
	for _, e := range elems {
		ev := reflect.New(et).Elem()
		if err := decodeValue(e.Value, ev); err != nil {
			return err
		}
		target.SetMapIndex(reflect.ValueOf(e.Key), ev)
	}
	return nil
}

func decodeStruct(raw bson.Raw, target reflect.Value) error {
	elems, err := raw.Elements()
	if err != nil {
		return err
	}
	fields := structFieldIndex(target.Type())
	for _, e := range elems {
		sf, ok := fields[e.Key]
		if !ok {
			sf, ok = fields[strings.ToLower(e.Key)]
		}
		if !ok {
			if inlineMap := fields[inlineMapKey]; inlineMap.valid {
				if err := setInlineMapEntry(target, inlineMap, e); err != nil {
					return err
				}
			}
			continue
		}
		fv := target.FieldByIndex(sf.index)
		if err := decodeValue(e.Value, fv); err != nil {
			return err
		}
	}
	return nil
}

func setInlineMapEntry(target reflect.Value, f fieldInfo, e bson.Element) error {
	mv := target.FieldByIndex(f.index)
	if mv.IsNil() {
		mv.Set(reflect.MakeMap(mv.Type()))
	}
	val, err := decodeNatural(e.Value)
	if err != nil {
		return err
	}
	if val == nil {
		mv.SetMapIndex(reflect.ValueOf(e.Key), reflect.Zero(mv.Type().Elem()))
		return nil
	}
	mv.SetMapIndex(reflect.ValueOf(e.Key), reflect.ValueOf(val).Convert(mv.Type().Elem()))
	return nil
}

func toInt64(rv bson.RawValue) (int64, bool) {
	switch rv.Type {
	case bson.TypeInt32:
		return int64(rv.Int32()), true
	case bson.TypeInt64:
		return rv.Int64(), true
	case bson.TypeDouble:
		return int64(rv.Double()), true
	case bson.TypeDateTime:
		return rv.DateTime(), true
	case bson.TypeBoolean:
		if rv.Boolean() {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func toFloat64(rv bson.RawValue) (float64, bool) {
	switch rv.Type {
	case bson.TypeDouble:
		return rv.Double(), true
	case bson.TypeInt32:
		return float64(rv.Int32()), true
	case bson.TypeInt64:
		return float64(rv.Int64()), true
	default:
		if f, ok := rv.AsFloat64(); ok {
			return f, true
		}
		return 0, false
	}
}

func typeErr(t bson.Type, target reflect.Value) error {
	return fmt.Errorf("doc: cannot decode BSON %s into %s", t, target.Type())
}
