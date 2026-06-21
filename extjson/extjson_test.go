package extjson

import (
	"math"
	"strings"
	"testing"

	"github.com/tamnd/doc/bson"
)

// parseField parses a one-field document and returns the field's value, failing the
// test on a parse error. It keeps the per-type tests to a single line each.
func parseField(t *testing.T, text string) bson.RawValue {
	t.Helper()
	raw, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse(%s): %v", text, err)
	}
	v, ok := raw.Lookup("v")
	if !ok {
		t.Fatalf("Parse(%s): no field v", text)
	}
	return v
}

func TestParseTopLevelMustBeObject(t *testing.T) {
	for _, in := range []string{`5`, `"x"`, `[1,2]`, `true`, `null`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%s): want error, got nil", in)
		}
	}
}

func TestParseTrailingContentRejected(t *testing.T) {
	if _, err := Parse([]byte(`{"a":1}{"b":2}`)); err == nil {
		t.Error("Parse with two documents: want error, got nil")
	}
}

func TestParseRelaxedNumbers(t *testing.T) {
	if v := parseField(t, `{"v":5}`); v.Type != bson.TypeInt32 || v.Int32() != 5 {
		t.Errorf("small int: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":5000000000}`); v.Type != bson.TypeInt64 || v.Int64() != 5000000000 {
		t.Errorf("large int: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":1.5}`); v.Type != bson.TypeDouble || v.Double() != 1.5 {
		t.Errorf("fraction: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":1e3}`); v.Type != bson.TypeDouble || v.Double() != 1000 {
		t.Errorf("exponent: got type %s", v.Type)
	}
}

func TestParseScalars(t *testing.T) {
	if v := parseField(t, `{"v":"hi"}`); v.Type != bson.TypeString || v.StringValue() != "hi" {
		t.Errorf("string: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":true}`); v.Type != bson.TypeBoolean || !v.Boolean() {
		t.Errorf("bool: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":null}`); v.Type != bson.TypeNull {
		t.Errorf("null: got type %s", v.Type)
	}
}

func TestParseWrappers(t *testing.T) {
	hex := "0123456789abcdef01234567"
	if v := parseField(t, `{"v":{"$oid":"`+hex+`"}}`); v.Type != bson.TypeObjectID || v.ObjectID().Hex() != hex {
		t.Errorf("$oid: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$numberInt":"7"}}`); v.Type != bson.TypeInt32 || v.Int32() != 7 {
		t.Errorf("$numberInt: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$numberLong":"7"}}`); v.Type != bson.TypeInt64 || v.Int64() != 7 {
		t.Errorf("$numberLong: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$numberDouble":"1.25"}}`); v.Type != bson.TypeDouble || v.Double() != 1.25 {
		t.Errorf("$numberDouble: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$minKey":1}}`); v.Type != bson.TypeMinKey {
		t.Errorf("$minKey: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$maxKey":1}}`); v.Type != bson.TypeMaxKey {
		t.Errorf("$maxKey: got type %s", v.Type)
	}
	if v := parseField(t, `{"v":{"$undefined":true}}`); v.Type != bson.TypeUndefined {
		t.Errorf("$undefined: got type %s", v.Type)
	}
}

func TestParseDateForms(t *testing.T) {
	want := int64(1609459200123) // 2021-01-01T00:00:00.123Z
	if v := parseField(t, `{"v":{"$date":"2021-01-01T00:00:00.123Z"}}`); v.Type != bson.TypeDateTime || v.DateTime() != want {
		t.Errorf("$date string: got type %s value %d", v.Type, v.DateTime())
	}
	if v := parseField(t, `{"v":{"$date":{"$numberLong":"1609459200123"}}}`); v.DateTime() != want {
		t.Errorf("$date $numberLong: got %d", v.DateTime())
	}
}

func TestParseBinary(t *testing.T) {
	v := parseField(t, `{"v":{"$binary":{"base64":"AQID","subType":"00"}}}`)
	if v.Type != bson.TypeBinary {
		t.Fatalf("$binary: got type %s", v.Type)
	}
	sub, data, ok := v.Binary()
	if !ok || sub != 0 || string(data) != "\x01\x02\x03" {
		t.Errorf("$binary: sub=%d data=%v ok=%v", sub, data, ok)
	}
}

func TestParseRegexForms(t *testing.T) {
	check := func(text string) {
		v := parseField(t, text)
		if v.Type != bson.TypeRegex {
			t.Fatalf("%s: got type %s", text, v.Type)
		}
		pat, opts, ok := v.Regex()
		if !ok || pat != "ab" || opts != "i" {
			t.Errorf("%s: pat=%q opts=%q ok=%v", text, pat, opts, ok)
		}
	}
	check(`{"v":{"$regularExpression":{"pattern":"ab","options":"i"}}}`)
	check(`{"v":{"$regex":"ab","$options":"i"}}`)
}

func TestParseTimestamp(t *testing.T) {
	v := parseField(t, `{"v":{"$timestamp":{"t":42,"i":7}}}`)
	if v.Type != bson.TypeTimestamp {
		t.Fatalf("$timestamp: got type %s", v.Type)
	}
	if got := v.Timestamp(); got != uint64(42)<<32|7 {
		t.Errorf("$timestamp: got %d", got)
	}
}

func TestParsePreservesKeyOrder(t *testing.T) {
	raw, err := Parse([]byte(`{"z":1,"a":2,"m":3}`))
	if err != nil {
		t.Fatal(err)
	}
	elems, err := raw.Elements()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{elems[0].Key, elems[1].Key, elems[2].Key}
	want := []string{"z", "a", "m"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key order: got %v want %v", got, want)
		}
	}
}

func TestParseNestedDocumentAndArray(t *testing.T) {
	raw, err := Parse([]byte(`{"a":{"b":[1,2,{"c":"x"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := raw.Lookup("a")
	if a.Type != bson.TypeDocument {
		t.Fatalf("a: got type %s", a.Type)
	}
	b, _ := a.Document().Lookup("b")
	if b.Type != bson.TypeArray {
		t.Fatalf("b: got type %s", b.Type)
	}
	elems, _ := b.Document().Elements()
	if len(elems) != 3 || elems[0].Key != "0" || elems[2].Key != "2" {
		t.Fatalf("array keys: %v", elems)
	}
}

func TestMarshalRelaxedScalars(t *testing.T) {
	cases := map[string]string{
		`{"v":5}`:       `{"v":5}`,
		`{"v":1.5}`:     `{"v":1.5}`,
		`{"v":"hi"}`:    `{"v":"hi"}`,
		`{"v":true}`:    `{"v":true}`,
		`{"v":null}`:    `{"v":null}`,
		`{"a":1,"b":2}`: `{"a":1,"b":2}`,
	}
	for in, want := range cases {
		raw, err := Parse([]byte(in))
		if err != nil {
			t.Fatalf("Parse(%s): %v", in, err)
		}
		out, err := MarshalRelaxed(raw)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", in, err)
		}
		if string(out) != want {
			t.Errorf("relaxed %s: got %s want %s", in, out, want)
		}
	}
}

func TestMarshalRelaxedWholeDoubleKeepsMarker(t *testing.T) {
	raw := bson.NewBuilder().AppendDouble("v", 5).Build()
	out, err := MarshalRelaxed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"v":5.0}` {
		t.Errorf("whole double: got %s want {\"v\":5.0}", out)
	}
}

func TestMarshalCanonicalWraps(t *testing.T) {
	raw := bson.NewBuilder().
		AppendInt32("i", 5).
		AppendInt64("l", 9).
		AppendDouble("d", 1.5).
		Build()
	out, err := Marshal(raw, Options{Canonical: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"i":{"$numberInt":"5"},"l":{"$numberLong":"9"},"d":{"$numberDouble":"1.5"}}`
	if string(out) != want {
		t.Errorf("canonical: got %s want %s", out, want)
	}
}

func TestMarshalNonFiniteDoubles(t *testing.T) {
	inf := bson.NewBuilder().AppendDouble("v", math.Inf(1)).Build()
	out, _ := MarshalRelaxed(inf)
	if !strings.Contains(string(out), `"$numberDouble":"Infinity"`) {
		t.Errorf("infinity: got %s", out)
	}
}

func TestMarshalIndent(t *testing.T) {
	raw, _ := Parse([]byte(`{"a":1,"b":2}`))
	out, err := Marshal(raw, Options{Indent: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": 2\n}"
	if string(out) != want {
		t.Errorf("indent: got %q want %q", out, want)
	}
}

// TestRoundTripCanonical confirms a canonical render parses back to the same BSON
// bytes for every type the codec carries through the bridge.
func TestRoundTripCanonical(t *testing.T) {
	src := bson.NewBuilder().
		AppendString("s", "hello").
		AppendInt32("i", -7).
		AppendInt64("l", 1<<40).
		AppendDouble("d", 3.14159).
		AppendBoolean("b", true).
		AppendNull("n").
		AppendDateTime("dt", 1609459200123).
		AppendTimestamp("ts", uint64(42)<<32|7).
		AppendBinary("bin", 0, []byte{1, 2, 3}).
		Build()

	out, err := Marshal(src, Options{Canonical: true})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse %s: %v", out, err)
	}
	if !bytesEqual(src, back) {
		again, _ := Marshal(back, Options{Canonical: true})
		t.Errorf("round trip differs:\n first %s\nsecond %s", out, again)
	}
}

func bytesEqual(a, b bson.Raw) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
