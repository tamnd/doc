package schema

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/tamnd/doc/bson"
)

// node is a compiled $jsonSchema sub-schema. A nil field means the corresponding
// keyword was absent, so it imposes no constraint. The evaluator walks the tree
// once per document; compilation (including regexp parsing) happens at Compile time
// so the per-write cost is a tree walk (spec 2061 doc 09 §10.7).
type node struct {
	types       []bson.Type // accepted BSON types from bsonType/type; nil means any
	anyNumber   bool        // bsonType "number": any of the four numeric types
	wholeDouble bool        // JSON Schema "integer": a whole-valued double also counts
	hasType     bool        // a bsonType/type keyword was present

	required   []string
	properties map[string]*node

	addlAllowed bool  // value of a boolean additionalProperties
	addlSchema  *node // schema form of additionalProperties
	hasAddl     bool

	minLen *int64
	maxLen *int64

	minimum      *float64
	maximum      *float64
	exclusiveMin bool
	exclusiveMax bool

	enum    []bson.RawValue
	hasEnum bool
	pattern *regexp.Regexp

	items    *node
	minItems *int64
	maxItems *int64
	unique   bool

	allOf []*node
	anyOf []*node
	oneOf []*node
	not   *node
}

// compileNode builds a node tree from a JSON Schema document.
func compileNode(d bson.Raw) (*node, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	n := &node{addlAllowed: true}
	for _, e := range elems {
		switch e.Key {
		case "bsonType", "type":
			if err := n.compileTypes(e.Value, e.Key == "type"); err != nil {
				return nil, err
			}
		case "required":
			ss, err := stringArray(e.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: required: %v", ErrInvalidSchema, err)
			}
			n.required = ss
		case "properties":
			if e.Value.Type != bson.TypeDocument {
				return nil, fmt.Errorf("%w: properties must be a document", ErrInvalidSchema)
			}
			props, err := e.Value.Document().Elements()
			if err != nil {
				return nil, err
			}
			n.properties = make(map[string]*node, len(props))
			for _, p := range props {
				if p.Value.Type != bson.TypeDocument {
					return nil, fmt.Errorf("%w: property %q schema must be a document", ErrInvalidSchema, p.Key)
				}
				child, err := compileNode(p.Value.Document())
				if err != nil {
					return nil, err
				}
				n.properties[p.Key] = child
			}
		case "additionalProperties":
			n.hasAddl = true
			switch e.Value.Type {
			case bson.TypeBoolean:
				n.addlAllowed = e.Value.Boolean()
			case bson.TypeDocument:
				child, err := compileNode(e.Value.Document())
				if err != nil {
					return nil, err
				}
				n.addlSchema = child
			default:
				return nil, fmt.Errorf("%w: additionalProperties must be a bool or a document", ErrInvalidSchema)
			}
		case "minLength":
			n.minLen = int64Ptr(e.Value)
		case "maxLength":
			n.maxLen = int64Ptr(e.Value)
		case "minimum":
			n.minimum = float64Ptr(e.Value)
		case "maximum":
			n.maximum = float64Ptr(e.Value)
		case "exclusiveMinimum":
			n.exclusiveMin = e.Value.Type == bson.TypeBoolean && e.Value.Boolean()
		case "exclusiveMaximum":
			n.exclusiveMax = e.Value.Type == bson.TypeBoolean && e.Value.Boolean()
		case "enum":
			vals, err := arrayValues(e.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: enum must be an array", ErrInvalidSchema)
			}
			n.enum = vals
			n.hasEnum = true
		case "pattern":
			s, ok := e.Value.StringValueOK()
			if !ok {
				return nil, fmt.Errorf("%w: pattern must be a string", ErrInvalidSchema)
			}
			re, err := regexp.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%w: pattern: %v", ErrInvalidSchema, err)
			}
			n.pattern = re
		case "items":
			if e.Value.Type != bson.TypeDocument {
				return nil, fmt.Errorf("%w: items must be a document", ErrInvalidSchema)
			}
			child, err := compileNode(e.Value.Document())
			if err != nil {
				return nil, err
			}
			n.items = child
		case "minItems":
			n.minItems = int64Ptr(e.Value)
		case "maxItems":
			n.maxItems = int64Ptr(e.Value)
		case "uniqueItems":
			n.unique = e.Value.Type == bson.TypeBoolean && e.Value.Boolean()
		case "allOf":
			subs, err := schemaArray(e.Value)
			if err != nil {
				return nil, err
			}
			n.allOf = subs
		case "anyOf":
			subs, err := schemaArray(e.Value)
			if err != nil {
				return nil, err
			}
			n.anyOf = subs
		case "oneOf":
			subs, err := schemaArray(e.Value)
			if err != nil {
				return nil, err
			}
			n.oneOf = subs
		case "not":
			if e.Value.Type != bson.TypeDocument {
				return nil, fmt.Errorf("%w: not must be a document", ErrInvalidSchema)
			}
			child, err := compileNode(e.Value.Document())
			if err != nil {
				return nil, err
			}
			n.not = child
		case "title", "description", "$comment":
			// Annotation keywords carry no constraint.
		default:
			return nil, fmt.Errorf("%w: unsupported keyword %q", ErrInvalidSchema, e.Key)
		}
	}
	return n, nil
}

// compileTypes records the accepted BSON types from a bsonType or type keyword,
// which may be a single string or an array of strings.
func (n *node) compileTypes(v bson.RawValue, jsonType bool) error {
	n.hasType = true
	add := func(name string) error {
		if name == "number" {
			n.anyNumber = true
			return nil
		}
		t, ok := bsonTypeFor(name, jsonType)
		if !ok {
			return fmt.Errorf("%w: unknown type %q", ErrInvalidSchema, name)
		}
		n.types = append(n.types, t...)
		if name == "integer" {
			// JSON Schema integer accepts whole-valued doubles too (5.0), matched at eval.
			n.wholeDouble = true
		}
		return nil
	}
	switch v.Type {
	case bson.TypeString:
		return add(v.StringValue())
	case bson.TypeArray:
		names, err := stringArray(v)
		if err != nil {
			return fmt.Errorf("%w: type array: %v", ErrInvalidSchema, err)
		}
		for _, name := range names {
			if err := add(name); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: bsonType must be a string or array of strings", ErrInvalidSchema)
	}
}

// validate walks the node against a value at the given dotted path, returning the
// first unmet rule as a *Failure.
func (n *node) validate(v bson.RawValue, path string) error {
	if n.hasType && !n.typeMatches(v) {
		return &Failure{Rule: "bsonType", Path: path, Msg: fmt.Sprintf("value of type %s is not an accepted type", v.Type)}
	}
	if n.hasEnum && !n.enumMatches(v) {
		return &Failure{Rule: "enum", Path: path, Msg: "value is not in the enum"}
	}
	if err := n.checkString(v, path); err != nil {
		return err
	}
	if err := n.checkNumber(v, path); err != nil {
		return err
	}
	if err := n.checkArray(v, path); err != nil {
		return err
	}
	if err := n.checkObject(v, path); err != nil {
		return err
	}
	if err := n.checkLogical(v, path); err != nil {
		return err
	}
	return nil
}

func (n *node) checkString(v bson.RawValue, path string) error {
	if n.minLen == nil && n.maxLen == nil && n.pattern == nil {
		return nil
	}
	s, ok := v.StringValueOK()
	if !ok {
		return nil // length and pattern apply only to strings
	}
	rc := int64(len([]rune(s)))
	if n.minLen != nil && rc < *n.minLen {
		return &Failure{Rule: "minLength", Path: path, Msg: fmt.Sprintf("string length %d is below minLength %d", rc, *n.minLen)}
	}
	if n.maxLen != nil && rc > *n.maxLen {
		return &Failure{Rule: "maxLength", Path: path, Msg: fmt.Sprintf("string length %d exceeds maxLength %d", rc, *n.maxLen)}
	}
	if n.pattern != nil && !n.pattern.MatchString(s) {
		return &Failure{Rule: "pattern", Path: path, Msg: "string does not match pattern"}
	}
	return nil
}

func (n *node) checkNumber(v bson.RawValue, path string) error {
	if n.minimum == nil && n.maximum == nil {
		return nil
	}
	f, ok := v.AsFloat64()
	if !ok {
		return nil
	}
	if n.minimum != nil {
		if (n.exclusiveMin && f <= *n.minimum) || (!n.exclusiveMin && f < *n.minimum) {
			return &Failure{Rule: "minimum", Path: path, Msg: fmt.Sprintf("value %v is below minimum %v", f, *n.minimum)}
		}
	}
	if n.maximum != nil {
		if (n.exclusiveMax && f >= *n.maximum) || (!n.exclusiveMax && f > *n.maximum) {
			return &Failure{Rule: "maximum", Path: path, Msg: fmt.Sprintf("value %v exceeds maximum %v", f, *n.maximum)}
		}
	}
	return nil
}

func (n *node) checkArray(v bson.RawValue, path string) error {
	if n.items == nil && n.minItems == nil && n.maxItems == nil && !n.unique {
		return nil
	}
	if v.Type != bson.TypeArray {
		return nil
	}
	vals, err := arrayValues(v)
	if err != nil {
		return err
	}
	cnt := int64(len(vals))
	if n.minItems != nil && cnt < *n.minItems {
		return &Failure{Rule: "minItems", Path: path, Msg: fmt.Sprintf("array length %d is below minItems %d", cnt, *n.minItems)}
	}
	if n.maxItems != nil && cnt > *n.maxItems {
		return &Failure{Rule: "maxItems", Path: path, Msg: fmt.Sprintf("array length %d exceeds maxItems %d", cnt, *n.maxItems)}
	}
	if n.unique {
		for i := range vals {
			for j := i + 1; j < len(vals); j++ {
				if bson.Equal(vals[i], vals[j]) {
					return &Failure{Rule: "uniqueItems", Path: path, Msg: "array has duplicate items"}
				}
			}
		}
	}
	if n.items != nil {
		for i, ev := range vals {
			if err := n.items.validate(ev, indexPath(path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *node) checkObject(v bson.RawValue, path string) error {
	if len(n.required) == 0 && n.properties == nil && !n.hasAddl {
		return nil
	}
	if v.Type != bson.TypeDocument {
		return nil
	}
	doc := v.Document()
	present := map[string]bson.RawValue{}
	elems, err := doc.Elements()
	if err != nil {
		return err
	}
	for _, e := range elems {
		present[e.Key] = e.Value
	}
	for _, req := range n.required {
		if _, ok := present[req]; !ok {
			return &Failure{Rule: "required", Path: childPath(path, req), Msg: "missing required property"}
		}
	}
	for _, e := range elems {
		child, declared := n.properties[e.Key]
		if declared {
			if err := child.validate(e.Value, childPath(path, e.Key)); err != nil {
				return err
			}
			continue
		}
		if !n.hasAddl {
			continue
		}
		if n.addlSchema != nil {
			if err := n.addlSchema.validate(e.Value, childPath(path, e.Key)); err != nil {
				return err
			}
			continue
		}
		if !n.addlAllowed {
			return &Failure{Rule: "additionalProperties", Path: childPath(path, e.Key), Msg: "additional property is not allowed"}
		}
	}
	return nil
}

func (n *node) checkLogical(v bson.RawValue, path string) error {
	for _, sub := range n.allOf {
		if err := sub.validate(v, path); err != nil {
			return err
		}
	}
	if n.anyOf != nil {
		ok := false
		for _, sub := range n.anyOf {
			if sub.validate(v, path) == nil {
				ok = true
				break
			}
		}
		if !ok {
			return &Failure{Rule: "anyOf", Path: path, Msg: "value did not match any of the required schemas"}
		}
	}
	if n.oneOf != nil {
		matched := 0
		for _, sub := range n.oneOf {
			if sub.validate(v, path) == nil {
				matched++
			}
		}
		if matched != 1 {
			return &Failure{Rule: "oneOf", Path: path, Msg: fmt.Sprintf("value matched %d schemas, expected exactly one", matched)}
		}
	}
	if n.not != nil {
		if n.not.validate(v, path) == nil {
			return &Failure{Rule: "not", Path: path, Msg: "value matched a schema it must not match"}
		}
	}
	return nil
}

// typeMatches reports whether v's BSON type is one the node accepts.
func (n *node) typeMatches(v bson.RawValue) bool {
	if n.anyNumber && v.Type.IsNumeric() {
		return true
	}
	if n.wholeDouble && v.Type == bson.TypeDouble {
		if f, ok := v.AsFloat64(); ok && f == float64(int64(f)) {
			return true
		}
	}
	return slices.Contains(n.types, v.Type)
}

func (n *node) enumMatches(v bson.RawValue) bool {
	for _, want := range n.enum {
		if bson.Equal(v, want) {
			return true
		}
	}
	return false
}

// ---- helpers -------------------------------------------------------------

func childPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func indexPath(parent string, i int) string {
	return fmt.Sprintf("%s.%d", parent, i)
}

func int64Ptr(v bson.RawValue) *int64 {
	if f, ok := v.AsFloat64(); ok {
		n := int64(f)
		return &n
	}
	return nil
}

func float64Ptr(v bson.RawValue) *float64 {
	if f, ok := v.AsFloat64(); ok {
		return &f
	}
	return nil
}

// stringArray reads a BSON array of strings.
func stringArray(v bson.RawValue) ([]string, error) {
	vals, err := arrayValues(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(vals))
	for _, e := range vals {
		s, ok := e.StringValueOK()
		if !ok {
			return nil, fmt.Errorf("element is not a string")
		}
		out = append(out, s)
	}
	return out, nil
}

// arrayValues reads the elements of a BSON array value in order.
func arrayValues(v bson.RawValue) ([]bson.RawValue, error) {
	if v.Type != bson.TypeArray {
		return nil, fmt.Errorf("value is not an array")
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	out := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		out[i] = e.Value
	}
	return out, nil
}

// schemaArray compiles a BSON array of sub-schema documents (allOf/anyOf/oneOf).
func schemaArray(v bson.RawValue) ([]*node, error) {
	vals, err := arrayValues(v)
	if err != nil {
		return nil, fmt.Errorf("%w: expected an array of schemas", ErrInvalidSchema)
	}
	out := make([]*node, len(vals))
	for i, e := range vals {
		if e.Type != bson.TypeDocument {
			return nil, fmt.Errorf("%w: schema array element must be a document", ErrInvalidSchema)
		}
		child, err := compileNode(e.Document())
		if err != nil {
			return nil, err
		}
		out[i] = child
	}
	return out, nil
}

// bsonTypeFor maps a MongoDB bsonType alias (or a JSON Schema type name) to the
// concrete BSON types it accepts (spec 2061 doc 09 §10.2).
func bsonTypeFor(name string, jsonType bool) ([]bson.Type, bool) {
	if jsonType {
		switch name {
		case "object":
			return []bson.Type{bson.TypeDocument}, true
		case "array":
			return []bson.Type{bson.TypeArray}, true
		case "string":
			return []bson.Type{bson.TypeString}, true
		case "boolean":
			return []bson.Type{bson.TypeBoolean}, true
		case "null":
			return []bson.Type{bson.TypeNull}, true
		case "number":
			return []bson.Type{bson.TypeDouble, bson.TypeInt32, bson.TypeInt64, bson.TypeDecimal128}, true
		case "integer":
			return []bson.Type{bson.TypeInt32, bson.TypeInt64}, true
		}
		return nil, false
	}
	switch name {
	case "double":
		return []bson.Type{bson.TypeDouble}, true
	case "string":
		return []bson.Type{bson.TypeString}, true
	case "object":
		return []bson.Type{bson.TypeDocument}, true
	case "array":
		return []bson.Type{bson.TypeArray}, true
	case "binData":
		return []bson.Type{bson.TypeBinary}, true
	case "objectId":
		return []bson.Type{bson.TypeObjectID}, true
	case "bool":
		return []bson.Type{bson.TypeBoolean}, true
	case "date":
		return []bson.Type{bson.TypeDateTime}, true
	case "null":
		return []bson.Type{bson.TypeNull}, true
	case "regex":
		return []bson.Type{bson.TypeRegex}, true
	case "javascript":
		return []bson.Type{bson.TypeJavaScript}, true
	case "int":
		return []bson.Type{bson.TypeInt32}, true
	case "timestamp":
		return []bson.Type{bson.TypeTimestamp}, true
	case "long":
		return []bson.Type{bson.TypeInt64}, true
	case "decimal":
		return []bson.Type{bson.TypeDecimal128}, true
	case "minKey":
		return []bson.Type{bson.TypeMinKey}, true
	case "maxKey":
		return []bson.Type{bson.TypeMaxKey}, true
	}
	return nil, false
}
