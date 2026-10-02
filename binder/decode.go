package binder

import (
	"bufio"
	"cmp"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strconv"
)

func decodePath(in reflect.Value, params []param, r *http.Request) []FieldError {
	// ServeMux and chi hand wildcards over already unescaped. A name missing
	// from the route pattern reads as "" and is left alone, so `required`
	// reports it.
	var fails []FieldError
	for _, p := range params {
		if p.source != "path" {
			continue
		}
		if val := r.PathValue(p.name); val != "" {
			if msg := setParam(in.FieldByIndex(p.index), []string{val}); msg != "" {
				fails = append(fails, FieldError{Source: "path", Field: p.name, Message: msg})
			}
		}
	}
	return fails
}

// decodeQuery rejects parameters the input struct doesn't declare. Errors
// are sorted by name, so identical requests produce identical responses.
func decodeQuery(in reflect.Value, params []param, r *http.Request) []FieldError {
	values := r.URL.Query()
	var fails []FieldError
	for _, p := range params {
		if p.source != "query" {
			continue
		}
		if msg := setParam(in.FieldByIndex(p.index), values[p.name]); msg != "" {
			fails = append(fails, FieldError{Source: "query", Field: p.name, Message: msg})
		}
		delete(values, p.name)
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		fails = append(fails, FieldError{Source: "query", Field: name, Message: "is not allowed"})
	}
	slices.SortStableFunc(fails, func(a, b FieldError) int { return cmp.Compare(a.Field, b.Field) })
	return fails
}

// setParam fills v from the values of one parameter and returns a message for
// the client when they don't fit. An empty value counts as absent, leaving v
// zero for `required` to report.
func setParam(v reflect.Value, vals []string) string {
	switch {
	case len(vals) > 1:
		return "must not be repeated"
	case len(vals) == 0 || vals[0] == "":
		return ""
	case parseValue(v, vals[0]) != nil:
		// The parse error names Go types, so it is deliberately not
		// surfaced to the client.
		return "is not a valid value"
	}
	return ""
}

// parseValue sets v, of a type isParamType accepts, from s.
func parseValue(v reflect.Value, s string) error {
	if v.Kind() == reflect.Pointer {
		p := reflect.New(v.Type().Elem())
		if err := parseValue(p.Elem(), s); err != nil {
			return err
		}
		v.Set(p)
		return nil
	}
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	}
	return nil
}

// decodeBody fills the Body field. A non-zero status means the request is
// refused outright (413, 415) rather than failing field validation.
func decodeBody(v reflect.Value, s *body, w http.ResponseWriter, r *http.Request) ([]FieldError, int) {
	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxBodyBytes))

	// Peek rather than trusting ContentLength, which is -1 for chunked and
	// many HTTP/2 requests. Emptiness is settled before Content-Type because
	// clients routinely omit the header when they send nothing.
	if _, err := body.Peek(1); err != nil {
		return jsonErrors(err)
	}

	if !isJSON(r.Header.Get("Content-Type")) {
		return []FieldError{{Source: "body", Message: "Content-Type must be application/json"}},
			http.StatusUnsupportedMediaType
	}

	dec := jsontext.NewDecoder(body, json.RejectUnknownMembers(true))
	if err := json.UnmarshalDecode(dec, v.Field(s.index).Addr().Interface()); err != nil {
		return jsonErrors(err)
	}

	// Reject trailing data: `{"a":1}{"b":2}` would otherwise decode cleanly.
	// The limit can also trip here, and that is still a 413.
	if err := json.UnmarshalDecode(dec, new(jsontext.Value)); !errors.Is(err, io.EOF) {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return jsonErrors(err)
		}
		return []FieldError{{Source: "body", Message: "unexpected data after JSON value"}}, 0
	}
	return nil, 0
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}
