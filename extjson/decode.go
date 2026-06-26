// Package extjson converts between MongoDB Extended JSON v2 text and doc's BSON
// byte form (spec 2061 doc 15 §14.1). It is the bridge the CLI uses to read filter
// and document arguments typed as JSON and to print query results back as JSON, and
// it is written zero-dependency over the standard library so the engine keeps its
// CGO_ENABLED=0 promise.
//
// Both encodings of the spec's table are supported. Canonical Extended JSON wraps
// every type in a $-prefixed object so the type round-trips exactly; relaxed
// Extended JSON prints numbers and dates in their natural JSON form and only falls
// back to a wrapper for types JSON cannot express. The decoder accepts either form
// interchangeably, because a user typing a filter at the shell writes whichever is
// convenient.
package extjson

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// Parse reads one Extended JSON document and returns its BSON encoding. Object key
// order is preserved, which matters for command documents and for a stable _id-first
// layout. The input must be a single JSON object; a top-level array or scalar is an
// error, since a BSON value is always a document at the root.
func Parse(data []byte) (bson.Raw, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("extjson: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("extjson: top-level value must be an object")
	}
	raw, err := parseObject(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing content so a typo such as two documents in a row is caught.
	if dec.More() {
		return nil, fmt.Errorf("extjson: trailing content after document")
	}
	return raw, nil
}

// ParseArray reads a top-level JSON array of documents and returns each element as a
// BSON document. It is the array counterpart to Parse, used for the shell's insertMany
// argument and aggregate pipeline where the outer value is an array rather than an
// object. Every element must itself be a document; a scalar or nested array element is
// an error, since the callers all expect documents.
func ParseArray(data []byte) ([]bson.Raw, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("extjson: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '[' {
		return nil, fmt.Errorf("extjson: top-level value must be an array")
	}
	var out []bson.Raw
	for dec.More() {
		v, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		if v.Type != bson.TypeDocument {
			return nil, fmt.Errorf("extjson: array element is not a document")
		}
		out = append(out, v.Document())
	}
	if _, err := dec.Token(); err != nil { // closing bracket
		return nil, fmt.Errorf("extjson: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("extjson: trailing content after array")
	}
	return out, nil
}

// ParseValue reads a single top-level JSON value of any shape and returns it as a BSON
// RawValue. Unlike Parse it does not require a document; the shell uses it for helper
// arguments that may be an object, an array, a string, or a number.
func ParseValue(data []byte) (bson.RawValue, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return bson.RawValue{}, err
	}
	if dec.More() {
		return bson.RawValue{}, fmt.Errorf("extjson: trailing content after value")
	}
	return v, nil
}

// parseObject consumes the body of an object whose opening brace was already read
// and returns either a BSON document or, when the object is a recognized $-wrapper,
// the single typed value it stands for.
func parseObject(dec *json.Decoder) (bson.Raw, error) {
	type member struct {
		key string
		val bson.RawValue
	}
	var members []member
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("extjson: %w", err)
		}
		key := keyTok.(string)
		val, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		members = append(members, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, fmt.Errorf("extjson: %w", err)
	}
	b := bson.NewBuilder()
	for _, m := range members {
		b.AppendValue(m.key, m.val)
	}
	return b.Build(), nil
}

// parseValue parses the next JSON value into a BSON RawValue, recognizing the
// Extended JSON wrappers when the value is an object whose keys form a known set.
func parseValue(dec *json.Decoder) (bson.RawValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return bson.RawValue{}, fmt.Errorf("extjson: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseObjectValue(dec)
		case '[':
			return parseArrayValue(dec)
		default:
			return bson.RawValue{}, fmt.Errorf("extjson: unexpected %q", t)
		}
	case string:
		return rawString(t), nil
	case json.Number:
		return numberValue(t)
	case bool:
		return rawBool(t), nil
	case nil:
		return bson.RawValue{Type: bson.TypeNull}, nil
	default:
		return bson.RawValue{}, fmt.Errorf("extjson: unexpected token %v", tok)
	}
}

// parseObjectValue handles an object in value position: it buffers the members,
// checks for a $-wrapper, and otherwise emits an embedded document.
func parseObjectValue(dec *json.Decoder) (bson.RawValue, error) {
	var keys []string
	var vals []bson.RawValue

	// Parse each member value fully into a RawValue while tracking its key, since a
	// $-wrapper is recognized from the key set and may need the parsed sub-tree (for
	// $binary, $regularExpression, $timestamp).
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return bson.RawValue{}, fmt.Errorf("extjson: %w", err)
		}
		key := keyTok.(string)
		val, err := parseValue(dec)
		if err != nil {
			return bson.RawValue{}, err
		}
		keys = append(keys, key)
		vals = append(vals, val)
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return bson.RawValue{}, fmt.Errorf("extjson: %w", err)
	}

	if wrapped, ok, err := detectWrapper(keys, vals); err != nil {
		return bson.RawValue{}, err
	} else if ok {
		return wrapped, nil
	}

	b := bson.NewBuilder()
	for i, k := range keys {
		b.AppendValue(k, vals[i])
	}
	return rawDocument(b.Build()), nil
}

// parseArrayValue handles an array in value position, encoding it as a BSON array
// (a document with the numeric string keys "0", "1", ...).
func parseArrayValue(dec *json.Decoder) (bson.RawValue, error) {
	b := bson.NewBuilder()
	i := 0
	for dec.More() {
		val, err := parseValue(dec)
		if err != nil {
			return bson.RawValue{}, err
		}
		b.AppendValue(strconv.Itoa(i), val)
		i++
	}
	if _, err := dec.Token(); err != nil { // closing bracket
		return bson.RawValue{}, fmt.Errorf("extjson: %w", err)
	}
	return rawArray(b.Build()), nil
}

// detectWrapper recognizes the Extended JSON $-wrappers from an object's keys and
// values and returns the BSON value they denote. The second result is false when the
// object is an ordinary document rather than a wrapper.
func detectWrapper(keys []string, vals []bson.RawValue) (bson.RawValue, bool, error) {
	has := func(k string) (bson.RawValue, bool) {
		for i, kk := range keys {
			if kk == k {
				return vals[i], true
			}
		}
		return bson.RawValue{}, false
	}
	if len(keys) == 1 {
		switch keys[0] {
		case "$oid":
			return wrapOID(vals[0])
		case "$date":
			return wrapDate(vals[0])
		case "$numberInt":
			return wrapInt32(vals[0])
		case "$numberLong":
			return wrapInt64(vals[0])
		case "$numberDouble":
			return wrapDouble(vals[0])
		case "$timestamp":
			return wrapTimestamp(vals[0])
		case "$binary":
			return wrapBinary(vals[0])
		case "$regularExpression":
			return wrapRegex(vals[0])
		case "$minKey":
			return bson.RawValue{Type: bson.TypeMinKey}, true, nil
		case "$maxKey":
			return bson.RawValue{Type: bson.TypeMaxKey}, true, nil
		case "$undefined":
			return bson.RawValue{Type: bson.TypeUndefined}, true, nil
		}
	}
	// Legacy two-key regex form {"$regex": "...", "$options": "..."}.
	if pat, ok := has("$regex"); ok && len(keys) <= 2 {
		opts, _ := has("$options")
		if pat.Type == bson.TypeString {
			optStr := ""
			if opts.Type == bson.TypeString {
				optStr = opts.StringValue()
			}
			return rawRegex(pat.StringValue(), optStr), true, nil
		}
	}
	return bson.RawValue{}, false, nil
}

func wrapOID(v bson.RawValue) (bson.RawValue, bool, error) {
	if v.Type != bson.TypeString {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $oid must be a string")
	}
	b, err := hex.DecodeString(v.StringValue())
	if err != nil || len(b) != 12 {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $oid must be 24 hex characters")
	}
	var oid sys.ObjectID
	copy(oid[:], b)
	bb := bson.NewBuilder()
	bb.AppendObjectID("v", oid)
	return elemValue(bb.Build()), true, nil
}

func wrapDate(v bson.RawValue) (bson.RawValue, bool, error) {
	switch v.Type {
	case bson.TypeString:
		t, err := time.Parse(time.RFC3339Nano, v.StringValue())
		if err != nil {
			return bson.RawValue{}, false, fmt.Errorf("extjson: $date string must be RFC3339: %v", err)
		}
		return rawDateTime(t.UnixMilli()), true, nil
	case bson.TypeInt32:
		return rawDateTime(int64(v.Int32())), true, nil
	case bson.TypeInt64:
		return rawDateTime(v.Int64()), true, nil
	case bson.TypeDocument:
		// {"$date": {"$numberLong": "ms"}}
		inner := v.Document()
		nl, ok := inner.Lookup("$numberLong")
		if !ok || nl.Type != bson.TypeString {
			return bson.RawValue{}, false, fmt.Errorf("extjson: $date object must hold $numberLong")
		}
		ms, err := strconv.ParseInt(nl.StringValue(), 10, 64)
		if err != nil {
			return bson.RawValue{}, false, fmt.Errorf("extjson: $date $numberLong: %v", err)
		}
		return rawDateTime(ms), true, nil
	default:
		return bson.RawValue{}, false, fmt.Errorf("extjson: $date has an unsupported value")
	}
}

func wrapInt32(v bson.RawValue) (bson.RawValue, bool, error) {
	s, ok := numberString(v)
	if !ok {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberInt must be a numeric string")
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberInt: %v", err)
	}
	return rawInt32(int32(n)), true, nil
}

func wrapInt64(v bson.RawValue) (bson.RawValue, bool, error) {
	s, ok := numberString(v)
	if !ok {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberLong must be a numeric string")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberLong: %v", err)
	}
	return rawInt64(n), true, nil
}

func wrapDouble(v bson.RawValue) (bson.RawValue, bool, error) {
	s, ok := numberString(v)
	if !ok {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberDouble must be a string")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $numberDouble: %v", err)
	}
	return rawDouble(f), true, nil
}

func wrapTimestamp(v bson.RawValue) (bson.RawValue, bool, error) {
	if v.Type != bson.TypeDocument {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $timestamp must be an object")
	}
	inner := v.Document()
	tv, ok1 := inner.Lookup("t")
	iv, ok2 := inner.Lookup("i")
	if !ok1 || !ok2 {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $timestamp needs t and i")
	}
	tSec, _ := tv.AsFloat64()
	iOrd, _ := iv.AsFloat64()
	ts := uint64(uint32(tSec))<<32 | uint64(uint32(iOrd))
	return rawTimestamp(ts), true, nil
}

func wrapBinary(v bson.RawValue) (bson.RawValue, bool, error) {
	if v.Type != bson.TypeDocument {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $binary must be an object")
	}
	inner := v.Document()
	b64, ok1 := inner.Lookup("base64")
	st, ok2 := inner.Lookup("subType")
	if !ok1 || !ok2 || b64.Type != bson.TypeString || st.Type != bson.TypeString {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $binary needs base64 and subType strings")
	}
	data, err := base64.StdEncoding.DecodeString(b64.StringValue())
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $binary base64: %v", err)
	}
	sub, err := strconv.ParseUint(st.StringValue(), 16, 8)
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $binary subType: %v", err)
	}
	return rawBinary(byte(sub), data), true, nil
}

func wrapRegex(v bson.RawValue) (bson.RawValue, bool, error) {
	if v.Type != bson.TypeDocument {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $regularExpression must be an object")
	}
	inner := v.Document()
	pat, ok := inner.Lookup("pattern")
	if !ok || pat.Type != bson.TypeString {
		return bson.RawValue{}, false, fmt.Errorf("extjson: $regularExpression needs a pattern string")
	}
	opts := ""
	if o, ok := inner.Lookup("options"); ok && o.Type == bson.TypeString {
		opts = o.StringValue()
	}
	return rawRegex(pat.StringValue(), opts), true, nil
}

// numberValue maps a bare JSON number to int32, int64, or double following the
// relaxed Extended JSON rule: an integer that fits int32 stays int32, a larger
// integer becomes int64, and any value with a fraction or exponent becomes a double.
func numberValue(n json.Number) (bson.RawValue, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			if i >= -(1<<31) && i < (1<<31) {
				return rawInt32(int32(i)), nil
			}
			return rawInt64(i), nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return bson.RawValue{}, fmt.Errorf("extjson: bad number %q", s)
	}
	return rawDouble(f), nil
}

// numberString returns the numeric string a $number* wrapper carries, accepting both
// the canonical string form and a bare JSON number a user might type by mistake.
func numberString(v bson.RawValue) (string, bool) {
	switch v.Type {
	case bson.TypeString:
		return v.StringValue(), true
	case bson.TypeInt32:
		return strconv.FormatInt(int64(v.Int32()), 10), true
	case bson.TypeInt64:
		return strconv.FormatInt(v.Int64(), 10), true
	case bson.TypeDouble:
		return strconv.FormatFloat(v.Double(), 'g', -1, 64), true
	default:
		return "", false
	}
}
