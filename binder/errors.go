package binder

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/julio641742/backendkit/httperr"
	"github.com/julio641742/backendkit/internal/httpx"
)

type FieldError = httperr.FieldError

// StatusError lets handlers choose a status without importing the envelope.
// Status must be a 4xx or 5xx; anything else is answered with a 500. Message is
// sent to the client and defaults to the status text. Fields are sent as the
// envelope's errors, like validation failures. Err is never sent: it is the
// cause, kept for the error handler and errors.Is/As.
type StatusError struct {
	Status  int
	Message string
	Fields  []FieldError
	Err     error
}

func (e *StatusError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

func (e *StatusError) Unwrap() error { return e.Err }

// Error builds a StatusError. An empty msg falls back to the status text; err
// may be nil.
//
//	return nil, binder.Error(http.StatusNotFound, "user not found", err)
func Error(status int, msg string, err error) error {
	return &StatusError{Status: status, Message: msg, Err: err}
}

// FieldErr builds a StatusError for a business rule that failed on one body
// field, reported in the same envelope as validation errors. Build a
// StatusError directly for several fields or another source.
//
//	return nil, binder.FieldErr(http.StatusConflict, "email", "is already taken")
func FieldErr(status int, field, msg string) error {
	return &StatusError{
		Status: status,
		Fields: []FieldError{{Source: "body", Field: field, Message: msg}},
	}
}

// handleError turns a handler error into the envelope. Errors that are not a
// StatusError become an opaque 500, so internal details never reach clients.
func (b *Binder) handleError(w *httpx.TrackingWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "internal server error"
	var fields []FieldError
	if se, ok := errors.AsType[*StatusError](err); ok && se.Status >= 400 && se.Status <= 599 {
		status, msg, fields = se.Status, se.Message, se.Fields
	}

	if w.Started() || status >= 500 {
		b.onError(r, err)
	}
	// Headers are gone; writing the envelope now would corrupt the body.
	if w.Started() {
		return
	}
	httperr.Write(w, status, msg, fields...)
}

// logError logs the handler errors that would otherwise be lost: any that map
// to a 5xx, any returned after the handler had already started its response,
// and results that could not be sent.
func logError(r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "binder: handler error",
		"method", r.Method,
		"path", r.URL.Path,
		"err", err,
	)
}

// ---------------------------------------------------------------------------
// Validation errors
// ---------------------------------------------------------------------------

// validationErrors maps validator's namespaces back onto sections, so a failure
// in Body.Email is reported with source "body" rather than a bare field name.
func (p *plan) validationErrors(ve validator.ValidationErrors) []FieldError {
	out := make([]FieldError, 0, len(ve))
	for _, fe := range ve {
		source, field := "request", fe.Field()
		// StructNamespace looks like "CreateUserReq.Body.Address.Street": the
		// type, the section field, then the Go path of the failing field,
		// translated with the section's own tag key. A failure on the
		// section itself (a required body) leaves field empty.
		parts := strings.Split(fe.StructNamespace(), ".")
		if len(parts) > 1 {
			if k, ok := p.byName[parts[1]]; ok {
				source = k.String()
				field = wirePath(p.sections[k].typ, k.tagKey(), parts[2:])
			}
		}
		out = append(out, FieldError{
			Source:  source,
			Field:   field,
			Message: defaultMessage(fe),
		})
	}
	return out
}

func defaultMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "min", "max", "len":
		return describeSize(fe)
	case "oneof":
		return fmt.Sprintf("must be one of: %s", fe.Param())
	default:
		if fe.Param() != "" {
			return fmt.Sprintf("failed %s=%s", fe.Tag(), fe.Param())
		}
		return fmt.Sprintf("failed %s", fe.Tag())
	}
}

// describeSize words min/max/len by what they measure: length for strings,
// item count for collections, value for numbers.
func describeSize(fe validator.FieldError) string {
	var bound string
	switch fe.Tag() {
	case "min":
		bound = "at least"
	case "max":
		bound = "at most"
	default:
		bound = "exactly"
	}
	switch fe.Kind() {
	case reflect.String:
		return fmt.Sprintf("must be %s %s characters long", bound, fe.Param())
	case reflect.Slice, reflect.Array, reflect.Map:
		return fmt.Sprintf("must contain %s %s items", bound, fe.Param())
	default:
		return fmt.Sprintf("must be %s %s", bound, fe.Param())
	}
}

// wirePath turns the Go field path of a failing field ("Address", "Street",
// or "Items[0]", "Name") into the dotted wire names that tagKey assigns
// ("address.street", "items[0].name"). Untagged embedded structs are
// flattened, as encoding/json and the path and query decoders do.
func wirePath(t reflect.Type, tagKey string, goPath []string) string {
	out := make([]string, 0, len(goPath))
	for i, seg := range goPath {
		for t != nil && t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		name, index, _ := strings.Cut(seg, "[")
		var f reflect.StructField
		ok := t != nil && t.Kind() == reflect.Struct
		if ok {
			f, ok = t.FieldByName(name)
		}
		if !ok {
			// Not a struct field we can see; keep the rest as reported.
			return strings.Join(append(out, goPath[i:]...), ".")
		}

		wire := tagName(f, tagKey)
		switch {
		case wire == "" && f.Anonymous:
			// flattened: no segment of its own
		case wire == "":
			wire = f.Name
			fallthrough
		default:
			if index != "" {
				wire += "[" + index
			}
			out = append(out, wire)
		}

		t = f.Type
		if index != "" {
			// "0]" or "0][1]": one step into an element per index.
			t = elemType(t, 1+strings.Count(index, "["))
		}
	}
	return strings.Join(out, ".")
}

// elemType steps n times into the element type of slices, arrays and maps,
// or returns nil when t has no element to step into.
func elemType(t reflect.Type, n int) reflect.Type {
	for range n {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return nil
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// Body decode errors
// ---------------------------------------------------------------------------

func jsonErrors(err error) ([]FieldError, int) {
	var (
		syntax   *jsontext.SyntacticError
		semantic *json.SemanticError
		tooLarge *http.MaxBytesError
	)
	// Order matters: v2 wraps io.ErrUnexpectedEOF in a SyntacticError and
	// ErrUnknownName in a SemanticError, so the specific cases come first.
	switch {
	case errors.As(err, &tooLarge):
		return []FieldError{{Source: "body", Message: "request body too large"}},
			http.StatusRequestEntityTooLarge
	case errors.Is(err, io.ErrUnexpectedEOF):
		return []FieldError{{Source: "body", Message: "malformed JSON: unexpected end of input"}}, 0
	case errors.Is(err, io.EOF):
		return []FieldError{{Source: "body", Message: "body is empty"}}, 0
	case errors.As(err, &semantic) && errors.Is(err, json.ErrUnknownName):
		return []FieldError{{Source: "body", Field: jsonField(semantic.JSONPointer), Message: "is not allowed"}}, 0
	case errors.As(err, &semantic):
		return []FieldError{{Source: "body", Field: jsonField(semantic.JSONPointer), Message: "is not a valid value"}}, 0
	case errors.As(err, &syntax):
		return []FieldError{{
			Source:  "body",
			Message: fmt.Sprintf("malformed JSON at byte %d", syntax.ByteOffset),
		}}, 0
	default:
		return []FieldError{{Source: "body", Message: "could not be decoded"}}, 0
	}
}

// jsonField turns a JSON Pointer such as "/address/street" into the dotted
// form used by validation errors ("address.street").
func jsonField(p jsontext.Pointer) string {
	return strings.Join(slices.Collect(p.Tokens()), ".")
}
