package doc

import (
	"reflect"
	"sync"
)

// fieldInfo locates a struct field by its index path so decode can reach fields
// nested through inlined embedded structs.
type fieldInfo struct {
	index []int
	valid bool
}

// inlineMapKey is the reserved map key under which a struct's inline map field is
// registered, so unmatched document keys spill into it (spec 2061 doc 14 §4.5).
const inlineMapKey = "\x00inlinemap"

var fieldCache sync.Map // reflect.Type -> map[string]fieldInfo

// structFieldIndex builds, and caches, the BSON-key to field-index map for a
// struct type. Keys are the resolved bson names; the special inlineMapKey entry,
// when present, points at an inline map that catches unmatched keys.
func structFieldIndex(t reflect.Type) map[string]fieldInfo {
	if cached, ok := fieldCache.Load(t); ok {
		return cached.(map[string]fieldInfo)
	}
	out := make(map[string]fieldInfo)
	buildFieldIndex(t, nil, out)
	fieldCache.Store(t, out)
	return out
}

func buildFieldIndex(t reflect.Type, prefix []int, out map[string]fieldInfo) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous {
			continue // unexported
		}
		tag := parseStructTag(sf)
		if tag.skip {
			continue
		}
		idx := append(append([]int(nil), prefix...), i)
		if tag.inline {
			ft := sf.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				buildFieldIndex(ft, idx, out)
				continue
			case reflect.Map:
				out[inlineMapKey] = fieldInfo{index: idx, valid: true}
				continue
			}
		}
		if _, exists := out[tag.name]; !exists {
			out[tag.name] = fieldInfo{index: idx, valid: true}
		}
	}
}
