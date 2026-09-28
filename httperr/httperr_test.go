package httperr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name   string
		msg    string
		fields []FieldError
		want   string
	}{
		{"no fields", "nope", nil, `{"error":"nope","errors":[]}`},
		{"default message", "", nil, `{"error":"Forbidden","errors":[]}`},
		{"fields", "bad", []FieldError{{Source: "body", Field: "email", Message: "is taken"}, {Source: "query", Message: "x"}},
			`{"error":"bad","errors":[{"source":"body","field":"email","message":"is taken"},{"source":"query","message":"x"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, http.StatusForbidden, tt.msg, tt.fields...)

			if rec.Code != http.StatusForbidden {
				t.Errorf("code = %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if !json.Valid(rec.Body.Bytes()) || rec.Body.String() != tt.want {
				t.Errorf("body = %s, want %s", rec.Body, tt.want)
			}
		})
	}
}

func TestWriteKeepsCacheControl(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "private, max-age=10")
	Write(rec, http.StatusNotFound, "")

	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=10" {
		t.Errorf("Cache-Control = %q, want the caller's value kept", got)
	}
}
