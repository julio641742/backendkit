package binder

import (
	"context"
	"encoding"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// argKind says where Bind finds a handler argument.
type argKind uint8

const (
	argContext argKind = iota
	argRequest
	argProvided
	argInput
)

var (
	contextType = reflect.TypeFor[context.Context]()
	requestType = reflect.TypeFor[*http.Request]()
	errorType   = reflect.TypeFor[error]()
)

// reserved reports the types Bind fills itself, which Provide refuses.
func reserved(t reflect.Type) bool {
	return t == contextType || t == requestType
}

// plan is what Bind learns from a handler's signature.
type plan struct {
	fn      reflect.Value
	args    []argKind
	types   []reflect.Type
	input   *input // nil when the handler takes no input struct
	returns bool   // the handler returns (T, error) rather than error
}

// input describes the input struct: its path and query fields and its Body.
type input struct {
	typ    reflect.Type
	params []param
	body   *body
	byPath map[string]param // Go field path ("Page.Limit") -> param, for error attribution
}

// param is a path or query field: a scalar, a pointer to one or, in the
// query, a slice of them. Scalars are strings, bools, numbers and types
// implementing encoding.TextUnmarshaler, such as uuid.UUID and time.Time.
type param struct {
	source string // "path" or "query"
	name   string // the wildcard or query parameter name
	index  []int  // for reflect.Value.FieldByIndex on the input struct
}

type body struct {
	index int          // field index within the input struct
	typ   reflect.Type // pointer stripped
}

// buildPlan reflects over fn once and panics on anything malformed. Panicking
// is deliberate: these are programmer errors in route definitions, and a
// process that refuses to start beats one that 400s every request to one
// endpoint.
func (b *Binder) buildPlan(fn any) *plan {
	v := reflect.ValueOf(fn)
	if fn == nil || v.Kind() != reflect.Func || v.IsNil() {
		panic(fmt.Sprintf("binder: handler must be a non-nil func, got %T", fn))
	}
	t := v.Type()
	if t.IsVariadic() {
		panic(fmt.Sprintf("binder: handler %s must not be variadic", t))
	}

	p := &plan{fn: v}
	seen := make(map[reflect.Type]bool)
	for at := range t.Ins() {
		if seen[at] {
			panic(fmt.Sprintf("binder: handler %s takes %s twice", t, at))
		}
		seen[at] = true

		var k argKind
		switch _, provided := b.providers[at]; {
		case at == contextType:
			k = argContext
		case at == requestType:
			k = argRequest
		case provided:
			k = argProvided
		case at.Kind() == reflect.Struct:
			if p.input != nil {
				panic(fmt.Sprintf("binder: handler %s takes two input structs, %s and %s", t, p.input.typ, at))
			}
			k, p.input = argInput, buildInput(at)
		default:
			panic(fmt.Sprintf("binder: handler %s takes %s, which is neither built in, provided nor an input struct", t, at))
		}
		p.args = append(p.args, k)
		p.types = append(p.types, at)
	}

	switch {
	case t.NumOut() == 1 && t.Out(0) == errorType:
	case t.NumOut() == 2 && t.Out(1) == errorType:
		p.returns = true
	default:
		panic(fmt.Sprintf("binder: handler %s must return error or (T, error)", t))
	}
	return p
}

// buildInput lists the fields of an input struct, flattening embedded structs
// as encoding/json does, and panics on a field that is neither a tagged
// parameter nor the Body.
func buildInput(t reflect.Type) *input {
	in := &input{typ: t, byPath: make(map[string]param)}
	in.params = params(in, t, nil, nil)
	if len(in.params) == 0 && in.body == nil {
		panic(fmt.Sprintf("binder: input struct %s has no path or query fields and no Body", t))
	}
	seen := make(map[[2]string]bool)
	for _, p := range in.params {
		k := [2]string{p.source, p.name}
		if seen[k] {
			panic(fmt.Sprintf("binder: %s has two fields for the %s parameter %q", t, p.source, p.name))
		}
		seen[k] = true
	}
	return in
}

func params(in *input, t reflect.Type, index []int, goPath []string) []param {
	var out []param
	for f := range t.Fields() {
		idx := append(slices.Clone(index), f.Index...)
		path := append(slices.Clone(goPath), f.Name)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, params(in, f.Type, idx, path)...)
			continue
		}
		if !f.IsExported() {
			if _, ok := f.Tag.Lookup("path"); ok {
				panic(fmt.Sprintf("binder: %s.%s is unexported, so its `path` tag is ignored", in.typ, f.Name))
			}
			if _, ok := f.Tag.Lookup("query"); ok {
				panic(fmt.Sprintf("binder: %s.%s is unexported, so its `query` tag is ignored", in.typ, f.Name))
			}
			continue
		}
		if f.Name == "Body" && len(index) == 0 {
			in.body = buildBody(in.typ, f)
			continue
		}

		source, tag := "path", ""
		pathTag, isPath := f.Tag.Lookup("path")
		queryTag, isQuery := f.Tag.Lookup("query")
		switch {
		case isPath && isQuery:
			panic(fmt.Sprintf("binder: %s.%s has both a `path` and a `query` tag", in.typ, f.Name))
		case isPath:
			tag = pathTag
		case isQuery:
			source, tag = "query", queryTag
		default:
			panic(fmt.Sprintf("binder: %s.%s has no `path` or `query` tag; every field of an "+
				"input struct must declare its source, or be the Body", in.typ, f.Name))
		}
		if tag == "-" {
			continue
		}
		if tag == "" || strings.Contains(tag, ",") {
			panic(fmt.Sprintf("binder: %s.%s: the `%s` tag must be just a name; use `binding` for rules such as required",
				in.typ, f.Name, source))
		}
		if !isParamType(f.Type) {
			panic(fmt.Sprintf("binder: %s.%s: %s can't be decoded from a %s parameter",
				in.typ, f.Name, f.Type, source))
		}
		p := param{source: source, name: tag, index: idx}
		in.byPath[strings.Join(path, ".")] = p
		out = append(out, p)
	}
	return out
}

func buildBody(outer reflect.Type, f reflect.StructField) *body {
	if f.Type.Kind() != reflect.Struct {
		panic(fmt.Sprintf("binder: %s.Body is a %s; it must be a struct", outer, f.Type))
	}
	checkBodyTags(outer, f.Type)
	return &body{index: f.Index[0], typ: f.Type}
}

// checkBodyTags rejects body fields tagged as path or query parameters but
// not json — the swapped-tag mistake that otherwise fails silently.
func checkBodyTags(outer, t reflect.Type) {
	for f := range t.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			checkBodyTags(outer, f.Type)
			continue
		}
		_, isJSON := f.Tag.Lookup("json")
		_, isPath := f.Tag.Lookup("path")
		_, isQuery := f.Tag.Lookup("query")
		if f.IsExported() && !isJSON && (isPath || isQuery) {
			panic(fmt.Sprintf("binder: %s.Body.%s has a `path` or `query` tag but no `json` tag; "+
				"move it out of the Body", outer, f.Name))
		}
	}
}

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// isParamType reports whether setParam can fill a field of type t.
func isParamType(t reflect.Type) bool {
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
// absent, unnamed ("json:\",omitempty\"") or skipped ("-").
func tagName(f reflect.StructField, key string) string {
	name, _, _ := strings.Cut(f.Tag.Get(key), ",")
	if name == "-" {
		return ""
	}
	return name
}
