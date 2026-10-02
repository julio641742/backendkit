package binder

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/go-playground/validator/v10"

	"github.com/julio641742/backendkit/httperr"
)

// maxBodyBytes caps request bodies before they reach the JSON decoder.
const maxBodyBytes = 1 << 20 // 1 MiB

// Binder holds providers, decoding and validation configuration. It is
// immutable once NewBinder returns, so one Binder can be shared by every
// route.
type Binder struct {
	validate  *validator.Validate
	providers map[reflect.Type]func(*http.Request) (reflect.Value, error)
	onError   func(*http.Request, error) // logError, swapped out by tests
	mapError  func(error) error
}

// Option configures a Binder at construction.
type Option func(*Binder)

// Provide registers fn as the source of handler arguments of type T, such as
// the logged in user. fn runs once per request, only for handlers that take a
// T, and before the input struct is decoded. An error it returns is answered
// like a handler error, so return a *StatusError for anything but a 500:
//
//	binder.Provide(func(r *http.Request) (*User, error) {
//		if u := session.From[User](r).GetUser(); u != nil {
//			return u, nil
//		}
//		return nil, binder.Error(http.StatusUnauthorized, "authentication required", nil)
//	})
//
// It panics when T is context.Context, *http.Request or http.ResponseWriter,
// which Bind fills itself, or fn is nil.
func Provide[T any](fn func(*http.Request) (T, error)) Option {
	t := reflect.TypeFor[T]()
	if reserved(t) {
		panic(fmt.Sprintf("binder: Provide(%s): Bind fills it already", t))
	}
	if fn == nil {
		panic(fmt.Sprintf("binder: Provide(%s): nil func", t))
	}
	return func(b *Binder) {
		b.providers[t] = func(r *http.Request) (reflect.Value, error) {
			v, err := fn(r)
			return reflect.ValueOf(&v).Elem(), err
		}
	}
}

// WithErrorMapper translates errors returned by handlers and providers before
// they are answered, so domain errors map to statuses in one place rather
// than in every handler. Return a *StatusError to pick the response;
// returning err unchanged (or nil) keeps the default handling, an opaque 500
// for anything that is not already a StatusError.
//
// Binding and validation failures are answered with a 400 (413, 415) before
// the handler runs and never reach fn.
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
		validate:  validator.New(validator.WithRequiredStructEnabled()),
		providers: make(map[reflect.Type]func(*http.Request) (reflect.Value, error)),
		onError:   logError,
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

// Bind turns fn into an http.HandlerFunc, so it registers on an
// http.ServeMux or chi and composes with standard middleware unchanged. fn's
// arguments are filled by type, in any order, each type at most once:
//
//   - context.Context, *http.Request, http.ResponseWriter: the request's own
//   - a type registered with Provide: what its provider returns
//   - any other struct: the input struct, decoded from the request
//
// fn returns error, answered with 204 when nil, or (T, error), answered with
// 200 and T as JSON, with 204 for a nil pointer, or with the status a *Result
// picks (see Created). Returning an error routes it through the same JSON
// envelope used for binding failures, so handlers never hand-roll error
// responses.
//
// Handlers may set response headers on w before returning, such as Location
// for a Created result; they are sent along with it. Handlers may also write
// to w themselves, for streaming and the like; once they have, the returned
// result is ignored.
//
// Bind panics when fn's signature breaks these rules; see the package doc for
// the input struct's.
func (b *Binder) Bind(fn any) http.HandlerFunc {
	p := b.buildPlan(fn) // once, at registration

	return func(w http.ResponseWriter, r *http.Request) {
		args := make([]reflect.Value, len(p.args))

		// Providers first, so a request without a user is a 401 rather
		// than a 400 for its input.
		for i, k := range p.args {
			if k != argProvided {
				continue
			}
			v, err := b.providers[p.types[i]](r)
			if err != nil {
				b.fail(w, r, err)
				return
			}
			args[i] = v
		}

		for i, k := range p.args {
			switch k {
			case argContext:
				args[i] = reflect.ValueOf(r.Context())
			case argRequest:
				args[i] = reflect.ValueOf(r)
			case argInput:
				in, ok := b.decode(p.input, w, r)
				if !ok {
					return
				}
				args[i] = in
			}
		}

		out := p.fn.Call(args)
		var result any
		if p.returns {
			result = out[0].Interface()
		}
		if err, _ := out[len(out)-1].Interface().(error); err != nil {
			b.fail(w, r, err)
			return
		}
		b.writeResult(w, r, result)
	}
}

// decode fills and validates the input struct, answering the request itself
// and reporting false when that fails.
func (b *Binder) decode(in *input, w http.ResponseWriter, r *http.Request) (reflect.Value, bool) {
	v := reflect.New(in.typ).Elem()

	var fails []FieldError
	fails = append(fails, decodePath(v, in.params, r)...)
	fails = append(fails, decodeQuery(v, in.params, r)...)
	if in.body != nil {
		bodyFails, status := decodeBody(v, in.body, w, r)
		// 413 and 415 describe the request as a whole; field errors from
		// the parameters would only distract from them.
		if status != 0 {
			httperr.Write(w, status, "", bodyFails...)
			return v, false
		}
		fails = append(fails, bodyFails...)
	}

	// Only validate once decoding succeeded; validating half-populated
	// structs produces errors that contradict the decode errors.
	if len(fails) == 0 {
		if ve, ok := errors.AsType[validator.ValidationErrors](b.validate.Struct(v.Interface())); ok {
			fails = append(fails, in.validationErrors(ve)...)
		}
	}

	if len(fails) > 0 {
		httperr.Write(w, http.StatusBadRequest, "request validation failed", fails...)
		return v, false
	}
	return v, true
}

// fail answers a handler or provider error, after the error mapper.
func (b *Binder) fail(w http.ResponseWriter, r *http.Request, err error) {
	if b.mapError != nil {
		if mapped := b.mapError(err); mapped != nil {
			err = mapped
		}
	}
	b.handleError(w, r, err)
}
