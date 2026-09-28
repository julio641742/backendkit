package binder

import "net/http"

// WithErrorHandler lets the tests see the errors a Binder reports instead of
// logging them; nil discards them.
func WithErrorHandler(fn func(*http.Request, error)) Option {
	if fn == nil {
		fn = func(*http.Request, error) {}
	}
	return func(b *Binder) { b.onError = fn }
}
