package bson

// Type is a BSON element type byte (spec 2061 doc 02 §4.3, §4.25). The values are
// the wire type bytes, so a Type read straight from a document or a query is the
// byte the BSON specification assigns.
type Type byte

const (
	TypeDouble        Type = 0x01
	TypeString        Type = 0x02
	TypeDocument      Type = 0x03
	TypeArray         Type = 0x04
	TypeBinary        Type = 0x05
	TypeUndefined     Type = 0x06 // deprecated; read for round-trip
	TypeObjectID      Type = 0x07
	TypeBoolean       Type = 0x08
	TypeDateTime      Type = 0x09
	TypeNull          Type = 0x0A
	TypeRegex         Type = 0x0B
	TypeDBPointer     Type = 0x0C // deprecated
	TypeJavaScript    Type = 0x0D
	TypeSymbol        Type = 0x0E // deprecated
	TypeCodeWithScope Type = 0x0F
	TypeInt32         Type = 0x10
	TypeTimestamp     Type = 0x11
	TypeInt64         Type = 0x12
	TypeDecimal128    Type = 0x13
	TypeMinKey        Type = 0xFF
	TypeMaxKey        Type = 0x7F
)

// String names the type for diagnostics. Unknown bytes render as their hex.
func (t Type) String() string {
	switch t {
	case TypeDouble:
		return "double"
	case TypeString:
		return "string"
	case TypeDocument:
		return "document"
	case TypeArray:
		return "array"
	case TypeBinary:
		return "binary"
	case TypeUndefined:
		return "undefined"
	case TypeObjectID:
		return "objectId"
	case TypeBoolean:
		return "bool"
	case TypeDateTime:
		return "date"
	case TypeNull:
		return "null"
	case TypeRegex:
		return "regex"
	case TypeDBPointer:
		return "dbPointer"
	case TypeJavaScript:
		return "javascript"
	case TypeSymbol:
		return "symbol"
	case TypeCodeWithScope:
		return "javascriptWithScope"
	case TypeInt32:
		return "int32"
	case TypeTimestamp:
		return "timestamp"
	case TypeInt64:
		return "int64"
	case TypeDecimal128:
		return "decimal128"
	case TypeMinKey:
		return "minKey"
	case TypeMaxKey:
		return "maxKey"
	default:
		const hexd = "0123456789abcdef"
		return "0x" + string([]byte{hexd[byte(t)>>4], hexd[byte(t)&0xf]})
	}
}

// IsNumeric reports whether the type is one of the four numeric BSON types, which
// share a comparison rank and key tag (spec 2061 doc 07 §3.2).
func (t Type) IsNumeric() bool {
	switch t {
	case TypeDouble, TypeInt32, TypeInt64, TypeDecimal128:
		return true
	default:
		return false
	}
}
