package extjson

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
)

// Options controls how Marshal renders a document.
type Options struct {
	// Canonical selects canonical Extended JSON (every type wrapped, fully
	// round-trippable) rather than the default relaxed form.
	Canonical bool
	// Indent turns on pretty-printing with two-space indentation. When false the
	// output is compact, one document with no internal newlines.
	Indent bool
}

// Marshal renders a BSON document as Extended JSON text per the chosen options. The
// relaxed default prints numbers and dates in natural JSON form; canonical wraps
// every value so the type survives a round trip (spec 2061 doc 15 §14.1).
func Marshal(raw bson.Raw, opts Options) ([]byte, error) {
	var sb strings.Builder
	if err := encodeDocument(&sb, raw, opts, 0); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// MarshalRelaxed is the common case: relaxed Extended JSON, compact.
func MarshalRelaxed(raw bson.Raw) ([]byte, error) {
	return Marshal(raw, Options{})
}

func encodeDocument(sb *strings.Builder, raw bson.Raw, opts Options, depth int) error {
	elems, err := raw.Elements()
	if err != nil {
		return fmt.Errorf("extjson: %w", err)
	}
	if len(elems) == 0 {
		sb.WriteString("{}")
		return nil
	}
	sb.WriteByte('{')
	for i, e := range elems {
		if i > 0 {
			sb.WriteByte(',')
		}
		newline(sb, opts, depth+1)
		writeJSONString(sb, e.Key)
		sb.WriteByte(':')
		if opts.Indent {
			sb.WriteByte(' ')
		}
		if err := encodeValue(sb, e.Value, opts, depth+1); err != nil {
			return err
		}
	}
	newline(sb, opts, depth)
	sb.WriteByte('}')
	return nil
}

func encodeArray(sb *strings.Builder, raw bson.Raw, opts Options, depth int) error {
	elems, err := raw.Elements()
	if err != nil {
		return fmt.Errorf("extjson: %w", err)
	}
	if len(elems) == 0 {
		sb.WriteString("[]")
		return nil
	}
	sb.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			sb.WriteByte(',')
		}
		newline(sb, opts, depth+1)
		if err := encodeValue(sb, e.Value, opts, depth+1); err != nil {
			return err
		}
	}
	newline(sb, opts, depth)
	sb.WriteByte(']')
	return nil
}

func encodeValue(sb *strings.Builder, v bson.RawValue, opts Options, depth int) error {
	switch v.Type {
	case bson.TypeDocument:
		return encodeDocument(sb, v.Document(), opts, depth)
	case bson.TypeArray:
		return encodeArray(sb, v.Document(), opts, depth)
	case bson.TypeString:
		writeJSONString(sb, v.StringValue())
	case bson.TypeBoolean:
		if v.Boolean() {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case bson.TypeNull:
		sb.WriteString("null")
	case bson.TypeUndefined:
		sb.WriteString(`{"$undefined":true}`)
	case bson.TypeInt32:
		encodeInt32(sb, v.Int32(), opts)
	case bson.TypeInt64:
		encodeInt64(sb, v.Int64(), opts)
	case bson.TypeDouble:
		encodeDouble(sb, v.Double(), opts)
	case bson.TypeObjectID:
		sb.WriteString(`{"$oid":"`)
		sb.WriteString(v.ObjectID().Hex())
		sb.WriteString(`"}`)
	case bson.TypeDateTime:
		encodeDate(sb, v.DateTime(), opts)
	case bson.TypeTimestamp:
		ts := v.Timestamp()
		fmt.Fprintf(sb, `{"$timestamp":{"t":%d,"i":%d}}`, uint32(ts>>32), uint32(ts))
	case bson.TypeBinary:
		sub, data, _ := v.Binary()
		sb.WriteString(`{"$binary":{"base64":"`)
		sb.WriteString(base64.StdEncoding.EncodeToString(data))
		fmt.Fprintf(sb, `","subType":"%02x"}}`, sub)
	case bson.TypeRegex:
		pat, optStr, _ := v.Regex()
		sb.WriteString(`{"$regularExpression":{"pattern":`)
		writeJSONString(sb, pat)
		sb.WriteString(`,"options":`)
		writeJSONString(sb, optStr)
		sb.WriteString("}}")
	case bson.TypeMinKey:
		sb.WriteString(`{"$minKey":1}`)
	case bson.TypeMaxKey:
		sb.WriteString(`{"$maxKey":1}`)
	default:
		return fmt.Errorf("extjson: cannot encode BSON type %s", v.Type)
	}
	return nil
}

func encodeInt32(sb *strings.Builder, n int32, opts Options) {
	if opts.Canonical {
		fmt.Fprintf(sb, `{"$numberInt":"%d"}`, n)
		return
	}
	sb.WriteString(strconv.FormatInt(int64(n), 10))
}

func encodeInt64(sb *strings.Builder, n int64, opts Options) {
	if opts.Canonical {
		fmt.Fprintf(sb, `{"$numberLong":"%d"}`, n)
		return
	}
	sb.WriteString(strconv.FormatInt(n, 10))
}

func encodeDouble(sb *strings.Builder, f float64, opts Options) {
	// JSON has no literal for the non-finite doubles, so they always take the wrapper
	// form, in both relaxed and canonical output.
	if math.IsNaN(f) {
		sb.WriteString(`{"$numberDouble":"NaN"}`)
		return
	}
	if math.IsInf(f, 1) {
		sb.WriteString(`{"$numberDouble":"Infinity"}`)
		return
	}
	if math.IsInf(f, -1) {
		sb.WriteString(`{"$numberDouble":"-Infinity"}`)
		return
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if opts.Canonical {
		fmt.Fprintf(sb, `{"$numberDouble":"%s"}`, s)
		return
	}
	sb.WriteString(s)
	// A whole-valued double must keep a marker so it does not read back as an int.
	if !strings.ContainsAny(s, ".eEnN") {
		sb.WriteString(".0")
	}
}

func encodeDate(sb *strings.Builder, ms int64, opts Options) {
	if opts.Canonical {
		fmt.Fprintf(sb, `{"$date":{"$numberLong":"%d"}}`, ms)
		return
	}
	// Relaxed form uses an ISO-8601 string for dates in a sensible range and falls
	// back to the canonical $numberLong form for dates outside it, matching the
	// driver's relaxed reader.
	if ms >= -62135596800000 && ms <= 253402300799999 {
		t := time.UnixMilli(ms).UTC()
		sb.WriteString(`{"$date":"`)
		sb.WriteString(t.Format(isoLayout(ms)))
		sb.WriteString(`"}`)
		return
	}
	fmt.Fprintf(sb, `{"$date":{"$numberLong":"%d"}}`, ms)
}

// isoLayout picks a millisecond-precision layout when the timestamp carries
// sub-second detail and a second-precision layout otherwise, so a whole-second date
// prints without a trailing .000.
func isoLayout(ms int64) string {
	if ms%1000 == 0 {
		return "2006-01-02T15:04:05Z07:00"
	}
	return "2006-01-02T15:04:05.000Z07:00"
}

func newline(sb *strings.Builder, opts Options, depth int) {
	if !opts.Indent {
		return
	}
	sb.WriteByte('\n')
	for range depth {
		sb.WriteString("  ")
	}
}

// writeJSONString writes s as a quoted, escaped JSON string. It mirrors
// encoding/json's escaping for the characters that must be escaped, without pulling
// the value through the reflection-based marshaler.
func writeJSONString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}
