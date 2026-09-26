// Package httperr writes the JSON error envelope shared by binder, session and
// any middleware that rejects a request. It depends on the standard library
// only, so importing it pulls in nothing else.
//
//	{"error": "request validation failed", "errors": [{"source": "body", "field": "email", "message": "is required"}]}
package httperr

import (
	"encoding/json/v2"
	"net/http"
)

// FieldError describes one failing input. Source is where it came from
// ("path", "query", "body", "request"); Field is empty when the failure is
// about the source as a whole.
type FieldError struct {
	Source  string `json:"source"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Response is the envelope. Errors is always present, [] when empty.
type Response struct {
	Error  string       `json:"error"`
	Errors []FieldError `json:"errors"`
}

// Write answers with the envelope. An empty msg falls back to the status text.
// It sets Cache-Control: no-store unless the response already has a
// Cache-Control, so shared caches never keep an error (a 401 or 403
// especially) and serve it to other clients.
func Write(w http.ResponseWriter, status int, msg string, fields ...FieldError) {
	if msg == "" {
		msg = http.StatusText(status)
	}
	if fields == nil {
		fields = []FieldError{}
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, Response{Error: msg, Errors: fields})
}
