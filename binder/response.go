package binder

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/julio641742/backendkit/internal/httpx"
)

// Result lets a handler choose the success status; return it as a *Result. Status must be a 2xx, or 0
// for the default; anything else is answered with a 500, as is a 204 that
// carries a Body. A nil Body sends the status with no body. The default status
// is 200, or 204 when Body is nil; an explicit 200 is always sent as is.
type Result struct {
	Status int
	Body   any
}

// Created answers with 201 and body as JSON.
func Created(body any) *Result { return &Result{Status: http.StatusCreated, Body: body} }

// writeResult sends what the handler returned. The body is marshalled before
// any header goes out, so an encoding failure still becomes a clean 500 rather
// than a truncated 2xx.
func (b *Binder) writeResult(w *httpx.TrackingWriter, r *http.Request, v any) {
	if w.Started() {
		// The handler answered on its own; a returned value has nowhere to go.
		if !isNil(v) {
			b.onError(r, errors.New("binder: handler returned a result after writing a response"))
		}
		return
	}

	status, body := 0, v
	switch res := v.(type) {
	case *Result:
		if res != nil {
			status, body = res.Status, res.Body
		}
	}
	if status == 0 {
		status = http.StatusOK
		if isNil(body) {
			status = http.StatusNoContent
		}
	}
	if status < 200 || status > 299 {
		b.handleError(w, r, fmt.Errorf("binder: result status %d is not a 2xx", status))
		return
	}

	if isNil(body) {
		w.WriteHeader(status)
		return
	}
	if status == http.StatusNoContent {
		b.handleError(w, r, errors.New("binder: a 204 result cannot carry a body"))
		return
	}

	data, err := json.Marshal(body)
	if err != nil {
		b.handleError(w, r, fmt.Errorf("binder: encoding result: %w", err))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// isNil also catches typed nil pointers: (*User)(nil) in an interface is
// non-nil and would otherwise be sent as a 200 "null". Nil slices and maps are
// left alone, since they encode as [] and {}.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}
