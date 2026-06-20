package bson

import (
	"bytes"
	"errors"
	"testing"
)

func TestAsFloat64Promotion(t *testing.T) {
	doc := NewBuilder().
		AppendInt32("i32", 5).
		AppendInt64("i64", 6).
		AppendDouble("d", 7.5).
		AppendString("s", "x").
		Build()
	cases := []struct {
		key  string
		want float64
		ok   bool
	}{
		{"i32", 5, true},
		{"i64", 6, true},
		{"d", 7.5, true},
		{"s", 0, false},
	}
	for _, c := range cases {
		v, _ := doc.Lookup(c.key)
		f, ok := v.AsFloat64()
		if ok != c.ok || (ok && f != c.want) {
			t.Errorf("%s: got %v,%v want %v,%v", c.key, f, ok, c.want, c.ok)
		}
	}
}

func TestRegexAccessor(t *testing.T) {
	// {r: /ab+c/im} encoded by hand: pattern cstring + options cstring.
	data := append([]byte("ab+c"), 0x00)
	data = append(data, []byte("im")...)
	data = append(data, 0x00)
	v := RawValue{Type: TypeRegex, Data: data}
	pat, opt, ok := v.Regex()
	if !ok || pat != "ab+c" || opt != "im" {
		t.Fatalf("regex = %q,%q,%v", pat, opt, ok)
	}
}

func TestWrongTypeAccessorsReturnFalse(t *testing.T) {
	v := RawValue{Type: TypeInt32, Data: []byte{1, 0, 0, 0}}
	if _, ok := v.DoubleOK(); ok {
		t.Error("DoubleOK on int32")
	}
	if _, ok := v.StringValueOK(); ok {
		t.Error("StringValueOK on int32")
	}
	if _, _, ok := v.Binary(); ok {
		t.Error("Binary on int32")
	}
	if v.Document() != nil {
		t.Error("Document on int32")
	}
	if _, _, ok := v.Regex(); ok {
		t.Error("Regex on int32")
	}
}

func TestTruncatedPayloadsRejected(t *testing.T) {
	if _, ok := (RawValue{Type: TypeDouble, Data: []byte{1, 2}}).DoubleOK(); ok {
		t.Error("short double accepted")
	}
	if _, ok := (RawValue{Type: TypeString, Data: []byte{0xff, 0xff, 0xff, 0xff}}).StringValueOK(); ok {
		t.Error("string with oversized length accepted")
	}
}

func TestStringValueVariants(t *testing.T) {
	// Symbol and JavaScript share the string accessor.
	for _, ty := range []Type{TypeSymbol, TypeJavaScript} {
		v := RawValue{Type: ty, Data: []byte{0x03, 0x00, 0x00, 0x00, 'h', 'i', 0x00}}
		if s, ok := v.StringValueOK(); !ok || s != "hi" {
			t.Errorf("type %v: %q,%v", ty, s, ok)
		}
	}
}

func TestElementsRejectsTruncated(t *testing.T) {
	// A length prefix claiming a string longer than the buffer.
	bad := Raw{0x0e, 0x00, 0x00, 0x00, 0x02, 's', 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00, 0x00}
	if _, err := bad.Elements(); err == nil {
		t.Fatal("expected error walking truncated string")
	}
}

func TestLookupMissingKey(t *testing.T) {
	doc := NewBuilder().AppendInt32("a", 1).Build()
	if _, ok := doc.Lookup("zzz"); ok {
		t.Fatal("found nonexistent key")
	}
}

func TestCloneIsIndependent(t *testing.T) {
	doc := NewBuilder().AppendInt32("a", 1).Build()
	c := doc.Clone()
	if !bytes.Equal(doc, c) {
		t.Fatal("clone differs")
	}
	c[4] = 0xFF
	if bytes.Equal(doc, c) {
		t.Fatal("clone aliases original")
	}
	if Raw(nil).Clone() != nil {
		t.Fatal("nil clone not nil")
	}
}

func TestValidateEmbeddedNUL(t *testing.T) {
	// A string value with an interior NUL must be rejected by deep Validate.
	doc := NewBuilder().AppendString("s", "a\x00b").Build()
	if err := doc.Validate(); !errors.Is(err, ErrEmbeddedNUL) {
		t.Fatalf("embedded NUL: got %v", err)
	}
}
