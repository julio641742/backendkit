package binder

import (
	"encoding"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

type kind uint8

const (
	kindPath kind = iota
	kindQuery
	kindBody
	numKinds
)

// kinds holds each source's name, which is also its `bind` tag value, and
// the tag key its fields are named with.
var kinds = [numKinds]struct{ name, tagKey string }{
	kindPath:  {"path", "path"},
	kindQuery: {"query", "query"},
	kindBody:  {"body", "json"},
}

func (k kind) String() string { return kinds[k].name }
func (k kind) tagKey() string { return kinds[k].tagKey }

// kindOf returns the kind a `bind` tag names.
func kindOf(tag string) (kind, bool) {
	for k := range numKinds {
		if kinds[k].name == tag {
			return k, true
		}
	}
	return 0, false
}

type section struct {
	present bool
	ptr     bool         // declared as *struct: optional, left nil when absent
	index   int          // field index within the request struct
	typ     reflect.Type // the section's struct type, pointer stripped
	params  []param      // path and query only: the fields to fill
}

// param is a path or query field: a scalar, a pointer to one or, in the
// query, a slice of them. Scalars are strings, bools, numbers and types
// implementing encoding.TextUnmarshaler, such as uuid.UUID and time.Time.
type param struct {
	name  string // the wildcard or query parameter name
	index []int  // for reflect.Value.FieldByIndex on the section
}

type plan struct {
	sections [numKinds]section
	byName   map[string]kind // field name -> kind, for error attribution
}

// buildPlan reflects over T once and panics on anything malformed. Panicking is
// deliberate: these are programmer errors in route definitions, and a process
// that refuses to start beats one that 400s every request to one endpoint.
func buildPlan[T any]() *plan {
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("binder: %s is not a struct", t))
	}

	p := &plan{byName: make(map[string]kind)}

	for i := range t.NumField() {
		f := t.Field(i)

		tag, ok := f.Tag.Lookup("bind")
		if !ok {
			panic(fmt.Sprintf("binder: %s.%s has no `bind` tag; every field of a "+
				"request struct must declare its source", t, f.Name))
		}
		k, ok := kindOf(tag)
		if !ok {
			panic(fmt.Sprintf("binder: %s.%s has unknown bind source %q", t, f.Name, tag))
		}
		if p.sections[k].present {
			panic(fmt.Sprintf("binder: %s declares two %q sections", t, k))
		}
		if !f.IsExported() {
			panic(fmt.Sprintf("binder: %s.%s is unexported and cannot be bound", t, f.Name))
		}

		ft, isPtr := f.Type, false
		if ft.Kind() == reflect.Pointer {
			ft, isPtr = ft.Elem(), true
		}
		if ft.Kind() != reflect.Struct {
			panic(fmt.Sprintf("binder: %s.%s must be a struct or *struct, got %s",
				t, f.Name, f.Type))
		}
		if isPtr && k != kindBody {
			panic(fmt.Sprintf("binder: %s.%s: only the body section may be optional", t, f.Name))
		}

		sec := section{present: true, ptr: isPtr, index: i, typ: ft}
		if k == kindBody {
			checkSectionTags(t, f.Name, ft, k)
		} else {
			sec.params = params(t, f.Name, ft, k, nil)
		}

		p.sections[k] = sec
		p.byName[f.Name] = k
	}

	return p
}

// checkSectionTags rejects a section whose fields carry the wrong tag key for
// their source — the swapped-tag mistake that otherwise fails silently.
func checkSectionTags(outer reflect.Type, fieldName string, sec reflect.Type, k kind) {
	want := k.tagKey()
	for f := range sec.Fields() {
		// Embedded structs are flattened, so their fields belong to the section.
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			checkSectionTags(outer, fieldName, f.Type, k)
			continue
		}
		if !f.IsExported() || f.Anonymous {
			continue
		}
		if _, ok := f.Tag.Lookup(want); !ok {
			panic(fmt.Sprintf("binder: %s.%s.%s is in the %q section but has no `%s` tag",
				outer, fieldName, f.Name, k, want))
		}
	}
}

// params lists the fields of a path or query section, flattening embedded
// structs as encoding/json does, and panics on a field that has no name in
// its tag or a type that can't be decoded from a string.
func params(outer reflect.Type, fieldName string, sec reflect.Type, k kind, index []int) []param {
	var out []param
	for f := range sec.Fields() {
		idx := append(slices.Clone(index), f.Index...)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, params(outer, fieldName, f.Type, k, idx)...)
			continue
		}
		if !f.IsExported() {
			continue
		}

		key := k.tagKey()
		tag, ok := f.Tag.Lookup(key)
		if !ok {
			panic(fmt.Sprintf("binder: %s.%s.%s is in the %q section but has no `%s` tag",
				outer, fieldName, f.Name, k, key))
		}
		if tag == "-" {
			continue
		}
		if tag == "" || strings.Contains(tag, ",") {
			panic(fmt.Sprintf("binder: %s.%s.%s: the `%s` tag must be just a name; use `binding` for rules such as required",
				outer, fieldName, f.Name, key))
		}
		if !isParamType(f.Type, k == kindQuery) {
			panic(fmt.Sprintf("binder: %s.%s.%s: %s can't be decoded from a %s parameter",
				outer, fieldName, f.Name, f.Type, k))
		}
		out = append(out, param{name: tag, index: idx})
	}
	return out
}

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// isParamType reports whether setParam can fill a field of type t.
func isParamType(t reflect.Type, allowSlice bool) bool {
	if allowSlice && t.Kind() == reflect.Slice {
		return isParamType(t.Elem(), false)
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// tagName returns the wire name a struct tag assigns, or "" when the tag is
// absent, unnamed ("path:\",required\"") or skipped ("-").
func tagName(f reflect.StructField, key string) string {
	name, _, _ := strings.Cut(f.Tag.Get(key), ",")
	if name == "-" {
		return ""
	}
	return name
}
