package binder

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/go-playground/validator/v10"

	"github.com/julio641742/backendkit/httperr"
	"github.com/julio641742/backendkit/internal/httpx"
)

// maxBodyBytes caps request bodies before they reach the JSON decoder.
const maxBodyBytes = 1 << 20 // 1 MiB

// Handler receives the fully bound and validated request. A nil result is
// answered with 204 and anything else with 200 and the result as JSON; wrap it
// in a Result (see Created) to pick another 2xx status. Returning an error
// routes it through the same JSON envelope used for binding failures, so
// handlers never hand-roll error responses.
//
// Handlers may set response headers on w before returning a result, such as
// Location for a Created one; they are sent along with it. Handlers may also
// write to w themselves, for streaming and the like; once they have, the
// returned result is ignored.
type Handler[T any] func(req *T, w http.ResponseWriter, r *http.Request) (any, error)

// Binder holds decoding and validation configuration. It is immutable once
// NewBinder returns, so one Binder can be shared by every route.
type Binder struct {
	validate *validator.Validate
	onError  func(*http.Request, error) // logError, swapped out by tests
	mapError func(error) error
}

// Option configures a Binder at construction.
type Option func(*Binder)

// WithErrorMapper translates errors returned by handlers before they are
// answered, so domain errors map to statuses in one place rather than in every
// handler. Return a *StatusError to pick the response; returning err unchanged
// (or nil) keeps the default handling, an opaque 500 for anything that is not
// already a StatusError.
//
// Only handler errors go through fn. Binding and validation failures are
// answered with a 400 (413, 415) before the handler runs and never reach it.
//
//	binder.WithErrorMapper(func(err error) error {
//		if database.IsNotFound(err) {
//			return binder.Error(http.StatusNotFound, "", err)
//		}
//		if _, ok := database.UniqueViolation(err); ok {
//			return binder.Error(http.StatusConflict, "", err)
//		}
//		return err
//	})
func WithErrorMapper(fn func(error) error) Option {
	return func(b *Binder) { b.mapError = fn }
}

// WithValidation registers a custom `binding` tag. It panics when the tag is
// empty or fn is nil.
//
//	binder.WithValidation("is_upper", func(fl validator.FieldLevel) bool {
//		s := fl.Field().String()
//		return s == strings.ToUpper(s)
//	})
func WithValidation(tag string, fn validator.Func) Option {
	return func(b *Binder) {
		if err := b.validate.RegisterValidation(tag, fn); err != nil {
			panic(fmt.Sprintf("binder: WithValidation(%q): %v", tag, err))
		}
	}
}

// NewBinder builds a Binder.
func NewBinder(opts ...Option) *Binder {
	b := &Binder{
		validate: validator.New(validator.WithRequiredStructEnabled()),
		onError:  logError,
	}
	for _, opt := range opts {
		opt(b)
	}

	b.validate.SetTagName("binding")

	// Report the wire name (path/query/json tag) rather than the Go field name,
	// so clients see "email" instead of "Body.Email".
	b.validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		for _, key := range []string{"json", "query", "path"} {
			if name := tagName(f, key); name != "" {
				return name
			}
		}
		return f.Name
	})

	return b
}

// Bind wraps a typed handler into an http.HandlerFunc, so it registers on an
// http.ServeMux and composes with standard middleware unchanged.
func (b *Binder) Bind[T any](h Handler[T]) http.HandlerFunc {
	if h == nil {
		panic(fmt.Sprintf("binder: nil handler for %s", reflect.TypeFor[T]()))
	}
	p := buildPlan[T]() // once, at registration

	return func(w http.ResponseWriter, r *http.Request) {
		req := new(T)
		v := reflect.ValueOf(req).Elem()

		var fails []FieldError

		if s := p.sections[kindPath]; s.present {
			fails = append(fails, decodePath(v.Field(s.index), s.params, r)...)
		}
		if s := p.sections[kindQuery]; s.present {
			fails = append(fails, decodeQuery(v.Field(s.index), s, r)...)
		}
		if s := p.sections[kindBody]; s.present {
			bodyFails, status := decodeBody(v, s, w, r)
			// 413 and 415 describe the request as a whole; field errors from
			// the other sections would only distract from them.
			if status != 0 {
				httperr.Write(w, status, "", bodyFails...)
				return
			}
			fails = append(fails, bodyFails...)
		}

		// Only validate once decoding succeeded; validating half-populated
		// structs produces errors that contradict the decode errors.
		if len(fails) == 0 {
			if err := b.validate.Struct(req); err != nil {
				ve, ok := errors.AsType[validator.ValidationErrors](err)
				if !ok { // InvalidValidationError: a bug, not bad input
					b.internalError(w, r, fmt.Errorf("binder: validating %s: %w", reflect.TypeFor[T](), err))
					return
				}
				fails = append(fails, p.validationErrors(ve)...)
			}
		}

		if len(fails) > 0 {
			httperr.Write(w, http.StatusBadRequest, "request validation failed", fails...)
			return
		}

		tw := &httpx.TrackingWriter{ResponseWriter: w}
		out, err := h(req, tw, r)
		if err != nil {
			if b.mapError != nil {
				if mapped := b.mapError(err); mapped != nil {
					err = mapped
				}
			}
			b.handleError(tw, r, err)
			return
		}
		b.writeResult(tw, r, out)
	}
}

// internalError answers a request that failed through no fault of its own
// with an opaque 500, reporting err to the error handler.
func (b *Binder) internalError(w http.ResponseWriter, r *http.Request, err error) {
	b.onError(r, err)
	httperr.Write(w, http.StatusInternalServerError, "internal server error")
}
