package extjson

import "github.com/tamnd/doc/bson"

// The rawX helpers build a single-element document and pull the element's value back
// out, which is the zero-dependency way to mint a typed bson.RawValue without
// duplicating the codec's byte layout. The element key is irrelevant; only the value
// is kept.

func firstValue(raw bson.Raw) bson.RawValue {
	elems, err := raw.Elements()
	if err != nil || len(elems) == 0 {
		return bson.RawValue{}
	}
	return elems[0].Value
}

func elemValue(raw bson.Raw) bson.RawValue { return firstValue(raw) }

func rawString(s string) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendString("v", s).Build())
}

func rawBool(b bool) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendBoolean("v", b).Build())
}

func rawInt32(n int32) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendInt32("v", n).Build())
}

func rawInt64(n int64) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendInt64("v", n).Build())
}

func rawDouble(f float64) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendDouble("v", f).Build())
}

func rawDateTime(ms int64) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendDateTime("v", ms).Build())
}

func rawTimestamp(ts uint64) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendTimestamp("v", ts).Build())
}

func rawBinary(subtype byte, data []byte) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendBinary("v", subtype, data).Build())
}

func rawDocument(d bson.Raw) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendDocument("v", d).Build())
}

func rawArray(a bson.Raw) bson.RawValue {
	return firstValue(bson.NewBuilder().AppendArray("v", a).Build())
}

// rawRegex mints a BSON regex value. The Builder has no AppendRegex, so the bytes are
// laid out by hand: two C strings (pattern then options), each NUL-terminated, which
// is the BSON regex element body.
func rawRegex(pattern, options string) bson.RawValue {
	data := make([]byte, 0, len(pattern)+len(options)+2)
	data = append(data, pattern...)
	data = append(data, 0)
	data = append(data, options...)
	data = append(data, 0)
	return bson.RawValue{Type: bson.TypeRegex, Data: data}
}
