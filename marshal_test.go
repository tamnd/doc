package doc

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
)

// roundTripM marshals v, decodes the bytes back into an M, and returns it.
func roundTripM(t *testing.T, v any) M {
	t.Helper()
	data, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T): %v", v, err)
	}
	if err := bson.Raw(data).Validate(); err != nil {
		t.Fatalf("Marshal(%T) produced invalid BSON: %v", v, err)
	}
	var m M
	if err := Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal into M: %v", err)
	}
	return m
}

func TestMarshalScalarTypesRoundTrip(t *testing.T) {
	oid := NewObjectID()
	dt := DateTime(1_700_000_000_000)
	bin := Binary{Subtype: 0x04, Data: []byte{1, 2, 3}}
	re := Regex{Pattern: "^go", Options: "i"}
	ts := Timestamp{T: 42, I: 7}
	var dec Decimal128
	dec[0] = 0xAB

	in := M{
		"double": 3.5,
		"string": "hello",
		"int32":  int32(7),
		"int64":  int64(1) << 40,
		"bool":   true,
		"oid":    oid,
		"date":   dt,
		"bin":    bin,
		"regex":  re,
		"js":     JavaScript("return 1;"),
		"ts":     ts,
		"dec":    dec,
		"min":    MinKey{},
		"max":    MaxKey{},
		"null":   nil,
	}
	m := roundTripM(t, in)

	if m["double"].(float64) != 3.5 {
		t.Errorf("double = %v", m["double"])
	}
	if m["string"].(string) != "hello" {
		t.Errorf("string = %v", m["string"])
	}
	if m["int32"].(int32) != 7 {
		t.Errorf("int32 = %v", m["int32"])
	}
	if m["int64"].(int64) != int64(1)<<40 {
		t.Errorf("int64 = %v", m["int64"])
	}
	if m["bool"].(bool) != true {
		t.Errorf("bool = %v", m["bool"])
	}
	if m["oid"].(ObjectID) != oid {
		t.Errorf("oid = %v, want %v", m["oid"], oid)
	}
	if m["date"].(DateTime) != dt {
		t.Errorf("date = %v", m["date"])
	}
	if gb := m["bin"].(Binary); gb.Subtype != 0x04 || !bytes.Equal(gb.Data, []byte{1, 2, 3}) {
		t.Errorf("bin = %+v", gb)
	}
	if gr := m["regex"].(Regex); gr != re {
		t.Errorf("regex = %+v", gr)
	}
	if m["js"].(JavaScript) != JavaScript("return 1;") {
		t.Errorf("js = %v", m["js"])
	}
	if gt := m["ts"].(Timestamp); gt != ts {
		t.Errorf("ts = %+v", gt)
	}
	if gd := m["dec"].(Decimal128); gd != dec {
		t.Errorf("dec = %x", gd)
	}
	if _, ok := m["min"].(MinKey); !ok {
		t.Errorf("min = %T", m["min"])
	}
	if _, ok := m["max"].(MaxKey); !ok {
		t.Errorf("max = %T", m["max"])
	}
	if v, ok := m["null"]; !ok || v != nil {
		t.Errorf("null = %v (present=%v)", v, ok)
	}
}

func TestIntWidthSelection(t *testing.T) {
	// Plain int that fits in int32 encodes as Int32; a large one as Int64.
	m := roundTripM(t, M{"small": 5, "big": 1 << 40})
	if _, ok := m["small"].(int32); !ok {
		t.Errorf("small int encoded as %T, want int32", m["small"])
	}
	if _, ok := m["big"].(int64); !ok {
		t.Errorf("big int encoded as %T, want int64", m["big"])
	}
}

type address struct {
	Street string `bson:"street"`
	City   string `bson:"city"`
}

type user struct {
	ID       ObjectID  `bson:"_id,omitempty"`
	Name     string    `bson:"name"`
	Age      int32     `bson:"age"`
	Score    float64   `bson:"score"`
	Tags     []string  `bson:"tags,omitempty"`
	Address  *address  `bson:"address,omitempty"`
	Created  time.Time `bson:"created"`
	Secret   string    `bson:"-"`
	internal int       //nolint:unused // exercises unexported skip
}

func TestStructRoundTrip(t *testing.T) {
	in := user{
		ID:      NewObjectID(),
		Name:    "Alice",
		Age:     30,
		Score:   92.5,
		Tags:    []string{"a", "b"},
		Address: &address{Street: "1 Main", City: "Springfield"},
		Created: time.UnixMilli(1_700_000_000_000).UTC(),
		Secret:  "hidden",
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The ignored field must not appear; the document must validate.
	var m M
	if err := Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to M: %v", err)
	}
	if _, ok := m["-"]; ok {
		t.Error("ignored field leaked into document")
	}
	if _, ok := m["secret"]; ok {
		t.Error("Secret field was not skipped")
	}
	if _, ok := m["internal"]; ok {
		t.Error("unexported field was encoded")
	}

	var out user
	if err := Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal to struct: %v", err)
	}
	out.Secret = in.Secret // not round-tripped by design
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestOmitEmptyDropsZeroValues(t *testing.T) {
	// Zero ID, no tags, nil address: all omitted.
	data, err := Marshal(user{Name: "Bob", Created: time.UnixMilli(0).UTC()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m M
	if err := Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"_id", "tags", "address"} {
		if _, ok := m[k]; ok {
			t.Errorf("omitempty field %q was encoded", k)
		}
	}
	if m["name"] != "Bob" {
		t.Errorf("name = %v", m["name"])
	}
}

type withInline struct {
	Type    string `bson:"type"`
	Payload M      `bson:",inline"`
}

func TestInlineMapFlattensAndCatches(t *testing.T) {
	in := withInline{Type: "event", Payload: M{"a": int32(1), "b": "x"}}
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m M
	if err := Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// Inline keys sit at the top level next to "type".
	if m["type"] != "event" || m["a"].(int32) != 1 || m["b"] != "x" {
		t.Errorf("inline flatten failed: %+v", m)
	}

	var out withInline
	if err := Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Type != "event" || out.Payload["a"].(int32) != 1 || out.Payload["b"] != "x" {
		t.Errorf("inline catch failed: %+v", out)
	}
}

func TestDPreservesOrder(t *testing.T) {
	in := D{{"z", int32(1)}, {"a", int32(2)}, {"m", int32(3)}}
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out D
	if err := Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{"z", "a", "m"}
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	for i, e := range out {
		if e.Key != wantKeys[i] {
			t.Errorf("position %d key = %q, want %q", i, e.Key, wantKeys[i])
		}
	}
}

func TestNestedDocumentsAndArrays(t *testing.T) {
	in := M{
		"meta": M{"k": "v", "n": int32(5)},
		"list": A{int32(1), "two", M{"deep": true}},
	}
	m := roundTripM(t, in)
	meta := m["meta"].(M)
	if meta["k"] != "v" || meta["n"].(int32) != 5 {
		t.Errorf("nested doc = %+v", meta)
	}
	list := m["list"].(A)
	if len(list) != 3 || list[0].(int32) != 1 || list[1] != "two" {
		t.Errorf("array = %+v", list)
	}
	if list[2].(M)["deep"] != true {
		t.Errorf("nested array doc = %+v", list[2])
	}
}

func TestPointerFieldNilEncodesAbsent(t *testing.T) {
	type box struct {
		P *int `bson:"p,omitempty"`
		Q *int `bson:"q"`
	}
	data, err := Marshal(box{})
	if err != nil {
		t.Fatal(err)
	}
	var m M
	if err := Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["p"]; ok {
		t.Error("nil pointer with omitempty should be absent")
	}
	if v, ok := m["q"]; !ok || v != nil {
		t.Errorf("nil pointer without omitempty should be null, got %v ok=%v", v, ok)
	}
}

// money is a custom document marshaler.
type money struct {
	Amount   int64
	Currency string
}

func (m money) MarshalBSON() ([]byte, error) {
	return Marshal(D{{"amount", m.Amount}, {"currency", m.Currency}})
}

func (m *money) UnmarshalBSON(data []byte) error {
	var raw struct {
		Amount   int64  `bson:"amount"`
		Currency string `bson:"currency"`
	}
	if err := Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Amount = raw.Amount
	m.Currency = raw.Currency
	return nil
}

func TestCustomMarshaler(t *testing.T) {
	type wallet struct {
		Owner   string `bson:"owner"`
		Balance money  `bson:"balance"`
	}
	in := wallet{Owner: "Alice", Balance: money{Amount: 1234, Currency: "USD"}}
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out wallet
	if err := Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Balance.Amount != 1234 || out.Balance.Currency != "USD" {
		t.Errorf("custom marshaler round trip failed: %+v", out.Balance)
	}
}

func TestMarshalValueRoundTrip(t *testing.T) {
	typ, data, err := MarshalValue("hello")
	if err != nil {
		t.Fatalf("MarshalValue: %v", err)
	}
	if typ != bson.TypeString {
		t.Fatalf("type = %v, want string", typ)
	}
	var s string
	if err := UnmarshalValue(typ, data, &s); err != nil {
		t.Fatalf("UnmarshalValue: %v", err)
	}
	if s != "hello" {
		t.Errorf("got %q", s)
	}
}

func TestNumericDecodeConversions(t *testing.T) {
	// A document carrying an int32 decodes into an int64 target and a float64
	// target via the conversion rules.
	data, err := Marshal(M{"n": int32(42), "f": 1.5})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		N int64   `bson:"n"`
		F float64 `bson:"f"`
		// Decode int32 into a plain int as well.
	}
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 42 || out.F != 1.5 {
		t.Errorf("conversions wrong: %+v", out)
	}
}

func TestUnmarshalRejectsNonPointer(t *testing.T) {
	data, _ := Marshal(M{"x": int32(1)})
	var m M
	if err := Unmarshal(data, m); err == nil {
		t.Error("expected error decoding into non-pointer map value")
	}
}

func TestMarshalEngineInterop(t *testing.T) {
	// A document built by the engine's bson.Builder decodes through the public
	// Unmarshal, proving the two layers share one wire format.
	raw := bson.NewBuilder().
		AppendString("name", "Eve").
		AppendInt32("age", int32(29)).
		AppendDouble("score", 88.0).
		Build()
	var out struct {
		Name  string  `bson:"name"`
		Age   int32   `bson:"age"`
		Score float64 `bson:"score"`
	}
	if err := Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal engine doc: %v", err)
	}
	if out.Name != "Eve" || out.Age != 29 || out.Score != 88.0 {
		t.Errorf("engine interop decode wrong: %+v", out)
	}

	// And the reverse: a public Marshal is readable by the engine's reader.
	data, err := Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	rv, ok := bson.Raw(data).Lookup("name")
	if !ok || rv.StringValue() != "Eve" {
		t.Errorf("engine could not read public marshal output")
	}
}
