package binder_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-playground/validator/v10"

	"github.com/julio641742/backendkit/binder"
	"github.com/julio641742/backendkit/httperr"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type result struct {
	status int
	body   httperr.Response
	raw    string
}

func serve(t *testing.T, h http.Handler, req *http.Request) result {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := result{status: rec.Code, raw: rec.Body.String()}
	if rec.Code >= 400 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.body); err != nil {
			t.Fatalf("decoding error envelope %q: %v", rec.Body.String(), err)
		}
	}
	return res
}

func jsonReq(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func wantStatus(t *testing.T, res result, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("status = %d, want %d; body: %s", res.status, want, res.raw)
	}
}

func wantErrors(t *testing.T, res result, want ...binder.FieldError) {
	t.Helper()
	if !reflect.DeepEqual(res.body.Errors, want) {
		t.Fatalf("errors = %+v\nwant     %+v", res.body.Errors, want)
	}
}

func noop[T any](T) error { return nil }

var defaultBinder = binder.NewBinder()

// ---------------------------------------------------------------------------
// Input types
// ---------------------------------------------------------------------------

type CreateUserInput struct {
	OrgID  int  `path:"org_id" binding:"required,min=1"`
	Notify bool `query:"notify"`
	Body   struct {
		Name  string `json:"name"  binding:"required"`
		Email string `json:"email" binding:"required,email"`
	}
}

type OrgOnly struct {
	OrgID int `path:"org_id" binding:"required"`
}

type NamePath struct {
	Name string `path:"name"`
}

type RequiredBody struct {
	Body struct {
		Name string `json:"name" binding:"required"`
	}
}

type ThreeInts struct {
	A int `query:"a"`
	B int `query:"b"`
	C int `query:"c"`
}

type QueryRequired struct {
	Q string `query:"q" binding:"required"`
}

type Nested struct {
	Body struct {
		Address struct {
			Street string `json:"street" binding:"required"`
		} `json:"address"`
	}
}

type Typed struct {
	Body struct {
		Age   int      `json:"age"`
		Tags  []string `json:"tags"`
		Score float64  `json:"score"`
	}
}

type Sizes struct {
	Body struct {
		Name  string   `json:"name"  binding:"min=3"`
		Tags  []string `json:"tags"  binding:"max=1"`
		Count int      `json:"count" binding:"min=10"`
	}
}

type Custom struct {
	Body struct {
		Code string `json:"code" binding:"is_upper"`
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestBindAllSources(t *testing.T) {
	var got CreateUserInput
	r := http.NewServeMux()
	r.HandleFunc("POST /orgs/{org_id}/users", defaultBinder.Bind(func(in CreateUserInput) (*binder.Result, error) {
		got = in
		return binder.Created(map[string]int{"id": 1}), nil
	}))

	res := serve(t, r, jsonReq("POST", "/orgs/7/users?notify=true", `{"name":"Ada","email":"ada@example.com"}`))
	wantStatus(t, res, http.StatusCreated)
	if got.OrgID != 7 || !got.Notify || got.Body.Name != "Ada" {
		t.Fatalf("bound request = %+v", got)
	}
	if res.raw != `{"id":1}` {
		t.Fatalf("body = %q, want {\"id\":1}", res.raw)
	}
}

// ---------------------------------------------------------------------------
// Handler results
// ---------------------------------------------------------------------------

type user struct {
	ID int `json:"id"`
}

func TestHandlerResult(t *testing.T) {
	tests := []struct {
		name       string
		result     any
		wantStatus int
		wantBody   string
	}{
		{"nil is a 204", nil, http.StatusNoContent, ""},
		{"typed nil pointer is a 204", (*user)(nil), http.StatusNoContent, ""},
		{"value is a 200", user{ID: 1}, http.StatusOK, `{"id":1}`},
		{"pointer is a 200", &user{ID: 1}, http.StatusOK, `{"id":1}`},
		{"nil slice is an empty array", []user(nil), http.StatusOK, `[]`},
		{"created", binder.Created(user{ID: 1}), http.StatusCreated, `{"id":1}`},
		{"result without body", &binder.Result{Status: http.StatusAccepted}, http.StatusAccepted, ""},
		{"nil result is a 204", (*binder.Result)(nil), http.StatusNoContent, ""},
		{"zero status is a 200", &binder.Result{Body: user{ID: 1}}, http.StatusOK, `{"id":1}`},
		{"zero status without body is a 204", &binder.Result{}, http.StatusNoContent, ""},
		{"explicit 200 without body stays a 200", &binder.Result{Status: http.StatusOK}, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := http.NewServeMux()
			r.HandleFunc("GET /", defaultBinder.Bind(func() (any, error) {
				return tt.result, nil
			}))

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			wantCT := ""
			if tt.wantBody != "" {
				wantCT = "application/json; charset=utf-8"
			}
			if got := rec.Header().Get("Content-Type"); got != wantCT {
				t.Fatalf("Content-Type = %q, want %q", got, wantCT)
			}
		})
	}
}

func TestHandlerResultFailures(t *testing.T) {
	tests := []struct {
		name   string
		result any
	}{
		{"unencodable value", make(chan int)},
		{"non-2xx status", &binder.Result{Status: http.StatusFound}},
		{"204 with body", &binder.Result{Status: http.StatusNoContent, Body: user{ID: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reported error
			b := binder.NewBinder(
				binder.WithErrorHandler(func(_ *http.Request, err error) { reported = err }),
			)
			r := http.NewServeMux()
			r.HandleFunc("GET /", b.Bind(func() (any, error) {
				return tt.result, nil
			}))

			res := serve(t, r, httptest.NewRequest("GET", "/", nil))
			wantStatus(t, res, http.StatusInternalServerError)
			if res.body.Error != "internal server error" {
				t.Fatalf("error = %q, want the opaque 500 message", res.body.Error)
			}
			if reported == nil {
				t.Fatal("failure was not reported")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bug 1: parameters from parent routes must not fail binding
// ---------------------------------------------------------------------------

func TestPathIgnoresUndeclaredParams(t *testing.T) {
	var got int
	r := http.NewServeMux()
	r.HandleFunc("GET /orgs/{org_id}/users/{user_id}", defaultBinder.Bind(func(in OrgOnly) error {
		got = in.OrgID
		return nil
	}))

	res := serve(t, r, httptest.NewRequest("GET", "/orgs/1/users/2", nil))
	wantStatus(t, res, http.StatusNoContent)
	if got != 1 {
		t.Fatalf("OrgID = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Bug 2: percent-encoded path parameters are unescaped exactly once
// ---------------------------------------------------------------------------

func TestPathUnescape(t *testing.T) {
	tests := []struct {
		name, target, want string
	}{
		// %2F forces a RawPath; the escaped slash must not split the segment.
		{"routed on RawPath", "/u/a%2Fb", "a/b"},
		// No RawPath: a second unescape would fail on the bare "%".
		{"routed on Path", "/u/100%25", "100%"},
		{"space", "/u/john%20doe", "john doe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			r := http.NewServeMux()
			r.HandleFunc("GET /u/{name}", defaultBinder.Bind(func(in NamePath) error {
				got = in.Name
				return nil
			}))

			res := serve(t, r, httptest.NewRequest("GET", tt.target, nil))
			wantStatus(t, res, http.StatusNoContent)
			if got != tt.want {
				t.Fatalf("Name = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bug 3: strict query mode rejects unknown parameters
// ---------------------------------------------------------------------------

func TestStrictQuery(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("GET /", defaultBinder.Bind(noop[ThreeInts]))
	res := serve(t, r, httptest.NewRequest("GET", "/?utm_source=x", nil))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "query", Field: "utm_source", Message: "is not allowed"})
}

// ---------------------------------------------------------------------------
// Bug 4: body emptiness does not depend on Content-Length
// ---------------------------------------------------------------------------

func TestRequiredBodyEmpty(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[RequiredBody]))

	req := httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader("")))
	req.ContentLength = -1
	res := serve(t, r, req)
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "body", Message: "body is empty"})
}

// ---------------------------------------------------------------------------
// Bug 5: strict body mode names the unknown key
// ---------------------------------------------------------------------------

func TestStrictBodyUnknownField(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", binder.NewBinder().Bind(noop[RequiredBody]))

	res := serve(t, r, jsonReq("POST", "/", `{"name":"x","nmae":"y"}`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "body", Field: "nmae", Message: "is not allowed"})
}

// ---------------------------------------------------------------------------
// Bug 6: decode errors come back in a stable order
// ---------------------------------------------------------------------------

func TestQueryErrorsSorted(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("GET /", defaultBinder.Bind(noop[ThreeInts]))

	want := []binder.FieldError{
		{Source: "query", Field: "a", Message: "is not a valid value"},
		{Source: "query", Field: "b", Message: "is not a valid value"},
		{Source: "query", Field: "c", Message: "is not a valid value"},
	}
	// Map iteration is randomised per range, so a handful of runs is enough
	// to catch an unsorted implementation.
	for range 20 {
		res := serve(t, r, httptest.NewRequest("GET", "/?c=z&a=x&b=y", nil))
		wantStatus(t, res, http.StatusBadRequest)
		wantErrors(t, res, want...)
	}
}

// ---------------------------------------------------------------------------
// Bug 7: 5xx reporting
// ---------------------------------------------------------------------------

func TestHandlerErrorReporting(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantReport bool
	}{
		{"plain error is a reported 500", errors.New("db down"), http.StatusInternalServerError, true},
		{"4xx status error is not reported", &binder.StatusError{Status: 404, Message: "not found"}, http.StatusNotFound, false},
		{"5xx status error is reported", &binder.StatusError{Status: 503, Message: "busy"}, http.StatusServiceUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reported error
			b := binder.NewBinder(
				binder.WithErrorHandler(func(_ *http.Request, err error) { reported = err }),
			)
			r := http.NewServeMux()
			r.HandleFunc("POST /", b.Bind(func() error { return tt.err }))

			res := serve(t, r, httptest.NewRequest("POST", "/", nil))
			wantStatus(t, res, tt.wantStatus)
			if got := reported != nil; got != tt.wantReport {
				t.Fatalf("reported = %v, want reported: %v", reported, tt.wantReport)
			}
		})
	}
}

func TestStatusErrorEdgeCases(t *testing.T) {
	cause := errors.New("row missing")
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{"zero status is a 500", &binder.StatusError{}, http.StatusInternalServerError, "internal server error"},
		{"non-error status is a 500", &binder.StatusError{Status: 302, Message: "moved"}, http.StatusInternalServerError, "internal server error"},
		{"empty message uses status text", &binder.StatusError{Status: 404}, http.StatusNotFound, "Not Found"},
		{"cause is not sent", &binder.StatusError{Status: 404, Message: "no user", Err: cause}, http.StatusNotFound, "no user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := binder.NewBinder(binder.WithErrorHandler(nil))
			r := http.NewServeMux()
			r.HandleFunc("POST /", b.Bind(func() error { return tt.err }))

			res := serve(t, r, httptest.NewRequest("POST", "/", nil))
			wantStatus(t, res, tt.wantStatus)
			if res.body.Error != tt.wantMsg {
				t.Fatalf("error = %q, want %q", res.body.Error, tt.wantMsg)
			}
		})
	}

	if err := (&binder.StatusError{Status: 404, Err: cause}); !errors.Is(err, cause) {
		t.Fatal("StatusError does not unwrap to its cause")
	}
}

func TestErrorConstructor(t *testing.T) {
	cause := errors.New("row missing")
	err := binder.Error(http.StatusNotFound, "no user", cause)

	se, ok := errors.AsType[*binder.StatusError](err)
	if !ok || se.Status != http.StatusNotFound || se.Message != "no user" || !errors.Is(err, cause) {
		t.Fatalf("Error() = %#v", err)
	}

	r := http.NewServeMux()
	r.HandleFunc("GET /", binder.NewBinder(binder.WithErrorHandler(nil)).Bind(func() error { return err }))
	res := serve(t, r, httptest.NewRequest("GET", "/", nil))
	wantStatus(t, res, http.StatusNotFound)
	if res.body.Error != "no user" {
		t.Fatalf("error = %q, want %q", res.body.Error, "no user")
	}
}

func TestFieldErr(t *testing.T) {
	serveErr := func(err error) result {
		r := http.NewServeMux()
		r.HandleFunc("POST /", binder.NewBinder(binder.WithErrorHandler(nil)).Bind(func() error { return err }))
		return serve(t, r, httptest.NewRequest("POST", "/", nil))
	}

	res := serveErr(binder.FieldErr(http.StatusConflict, "email", "is already taken"))
	wantStatus(t, res, http.StatusConflict)
	if res.body.Error != "Conflict" {
		t.Fatalf("error = %q, want the status text", res.body.Error)
	}
	wantErrors(t, res, binder.FieldError{Source: "body", Field: "email", Message: "is already taken"})

	fields := []binder.FieldError{
		{Source: "query", Field: "org", Message: "is archived"},
		{Source: "body", Field: "name", Message: "is reserved"},
	}
	res = serveErr(&binder.StatusError{Status: http.StatusUnprocessableEntity, Message: "cannot create", Fields: fields})
	wantStatus(t, res, http.StatusUnprocessableEntity)
	if res.body.Error != "cannot create" {
		t.Fatalf("error = %q", res.body.Error)
	}
	wantErrors(t, res, fields...)

	// Fields on an invalid status are dropped with the rest of the error.
	res = serveErr(&binder.StatusError{Status: 302, Fields: fields})
	wantStatus(t, res, http.StatusInternalServerError)
	if len(res.body.Errors) != 0 {
		t.Fatalf("errors = %+v, want none", res.body.Errors)
	}
}

func TestErrorEnvelopeHeaders(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("GET /", defaultBinder.Bind(noop[QueryRequired]))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// ---------------------------------------------------------------------------
// Suggestion 1: oversize bodies are a 413
// ---------------------------------------------------------------------------

func TestMaxBodyBytes(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[RequiredBody]))

	res := serve(t, r, jsonReq("POST", "/", `{"name":"`+strings.Repeat("x", 1<<20)+`"}`))
	wantStatus(t, res, http.StatusRequestEntityTooLarge)
	wantErrors(t, res, binder.FieldError{Source: "body", Message: "request body too large"})
}

// The limit reaches the server's own writer, which closes the connection
// rather than keep reading an oversize upload.
func TestMaxBodyBytesClosesConnection(t *testing.T) {
	srv := httptest.NewServer(defaultBinder.Bind(noop[RequiredBody]))
	defer srv.Close()

	body := `{"name":"` + strings.Repeat("x", 1<<20) + `"}`
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge || !res.Close {
		t.Fatalf("status = %d, Connection: close = %v; want 413 and close", res.StatusCode, res.Close)
	}
}

func TestMaxBodyBytesTrailingData(t *testing.T) {
	// The value itself fits; the limit trips while checking for trailing data.
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[RequiredBody]))

	res := serve(t, r, jsonReq("POST", "/", `{"name":"x"}`+strings.Repeat(" ", 1<<20)+`{}`))
	wantStatus(t, res, http.StatusRequestEntityTooLarge)
}

// ---------------------------------------------------------------------------
// Suggestion 2: custom validations can be registered
// ---------------------------------------------------------------------------

func TestWithValidation(t *testing.T) {
	b := binder.NewBinder(binder.WithValidation("is_upper", func(fl validator.FieldLevel) bool {
		s := fl.Field().String()
		return s == strings.ToUpper(s)
	}))

	r := http.NewServeMux()
	r.HandleFunc("POST /", b.Bind(noop[Custom]))

	wantStatus(t, serve(t, r, jsonReq("POST", "/", `{"code":"ABC"}`)), http.StatusNoContent)

	res := serve(t, r, jsonReq("POST", "/", `{"code":"abc"}`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "body", Field: "code", Message: "failed is_upper"})
}

func TestWithValidationPanics(t *testing.T) {
	for name, fn := range map[string]func(){
		"empty tag": func() { binder.NewBinder(binder.WithValidation("", func(validator.FieldLevel) bool { return true })) },
		"nil func":  func() { binder.NewBinder(binder.WithValidation("x", nil)) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			fn()
		})
	}
}

// ---------------------------------------------------------------------------
// Suggestion 3: nested fields are reported by their full wire path
// ---------------------------------------------------------------------------

func TestNestedFieldPath(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[Nested]))

	res := serve(t, r, jsonReq("POST", "/", `{"address":{}}`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "body", Field: "address.street", Message: "is required"})
}

// Parameters report names from their own tag key, whatever other tags a
// field carries.
func TestFieldNamesUseSourceTag(t *testing.T) {
	type Base struct {
		Limit int `query:"limit" json:"page_limit" binding:"max=10"`
	}
	type req struct {
		Base
		Sort string `query:"sort" json:"order" binding:"required"`
	}
	r := http.NewServeMux()
	r.HandleFunc("GET /", defaultBinder.Bind(noop[req]))

	res := serve(t, r, httptest.NewRequest("GET", "/?limit=50", nil))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res,
		binder.FieldError{Source: "query", Field: "limit", Message: "must be at most 10"},
		binder.FieldError{Source: "query", Field: "sort", Message: "is required"},
	)
}

func TestFieldNamesInsideCollections(t *testing.T) {
	type Item struct {
		Name string `json:"name" query:"n" binding:"required"`
	}
	type req struct {
		Body struct {
			Items []*Item         `json:"items" binding:"dive"`
			ByKey map[string]Item `json:"by_key" binding:"dive"`
			Grid  [][]Item        `json:"grid" binding:"dive,dive"`
		}
	}
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[req]))

	res := serve(t, r, jsonReq("POST", "/", `{"items":[{"name":"a"},{}],"by_key":{"k":{}},"grid":[[{}]]}`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res,
		binder.FieldError{Source: "body", Field: "items[1].name", Message: "is required"},
		binder.FieldError{Source: "body", Field: "by_key[k].name", Message: "is required"},
		binder.FieldError{Source: "body", Field: "grid[0][0].name", Message: "is required"},
	)
}

// ---------------------------------------------------------------------------
// Suggestion 4: non-JSON bodies are a 415
// ---------------------------------------------------------------------------

func TestContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        int
	}{
		{"json", "application/json", http.StatusNoContent},
		{"json with charset", "application/json; charset=utf-8", http.StatusNoContent},
		{"structured suffix", "application/merge-patch+json", http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"missing", "", http.StatusUnsupportedMediaType},
		{"malformed", "application/", http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := http.NewServeMux()
			r.HandleFunc("POST /", defaultBinder.Bind(noop[RequiredBody]))

			req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"x"}`))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			res := serve(t, r, req)
			wantStatus(t, res, tt.want)
			if tt.want == http.StatusUnsupportedMediaType {
				wantErrors(t, res, binder.FieldError{Source: "body", Message: "Content-Type must be application/json"})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Suggestion 5: type errors name the field, not Go types
// ---------------------------------------------------------------------------

func TestBodyTypeErrors(t *testing.T) {
	tests := []struct {
		body string
		want binder.FieldError
	}{
		{`{"age":"old"}`, binder.FieldError{Source: "body", Field: "age", Message: "is not a valid value"}},
		{`{"age":1.5}`, binder.FieldError{Source: "body", Field: "age", Message: "is not a valid value"}},
		{`{"tags":"a"}`, binder.FieldError{Source: "body", Field: "tags", Message: "is not a valid value"}},
		{`{"score":true}`, binder.FieldError{Source: "body", Field: "score", Message: "is not a valid value"}},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			r := http.NewServeMux()
			r.HandleFunc("POST /", defaultBinder.Bind(noop[Typed]))

			res := serve(t, r, jsonReq("POST", "/", tt.body))
			wantStatus(t, res, http.StatusBadRequest)
			wantErrors(t, res, tt.want)
		})
	}
}

func TestTruncatedJSON(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[Typed]))

	res := serve(t, r, jsonReq("POST", "/", `{"age":1`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "body", Message: "malformed JSON: unexpected end of input"})
}

// ---------------------------------------------------------------------------
// Suggestion 6: query parameters decode into flat fields
// ---------------------------------------------------------------------------

func TestQueryRequired(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("GET /", defaultBinder.Bind(noop[QueryRequired]))

	res := serve(t, r, httptest.NewRequest("GET", "/", nil))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "query", Field: "q", Message: "is required"})
}

type Page struct {
	Limit int `query:"limit" binding:"max=100"`
}

type Params struct {
	ID uuid.UUID `path:"id"`
	Page
	Since  time.Time `query:"since"`
	Active *bool     `query:"active"`
	Min    uint8     `query:"min"`
	Ratio  float32   `query:"ratio"`
	Skip   string    `query:"-"`
}

func TestParamTypes(t *testing.T) {
	var got Params
	r := http.NewServeMux()
	r.HandleFunc("GET /items/{id}", defaultBinder.Bind(func(in Params) error {
		got = in
		return nil
	}))

	id := uuid.MustParse("019cfa3a-f6a2-7eba-b1da-c7f1bac023f5")
	res := serve(t, r, httptest.NewRequest("GET", "/items/"+id.String()+
		"?limit=5&since=2026-01-02T03:04:05Z&active=false&min=7&ratio=0.5", nil))
	wantStatus(t, res, http.StatusNoContent)
	q := got
	if got.ID != id || q.Limit != 5 || !q.Since.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) ||
		q.Active == nil || *q.Active || q.Min != 7 || q.Ratio != 0.5 {
		t.Fatalf("bound = %+v", got)
	}

	// Absent and empty values leave the field zero.
	res = serve(t, r, httptest.NewRequest("GET", "/items/"+id.String()+"?limit=&active=", nil))
	wantStatus(t, res, http.StatusNoContent)
	if got.Limit != 0 || got.Active != nil {
		t.Fatalf("bound = %+v", got)
	}
}

func TestParamErrors(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("GET /items/{id}", defaultBinder.Bind(noop[Params]))

	res := serve(t, r, httptest.NewRequest("GET", "/items/nope?min=256&limit=1&limit=2&Skip=x", nil))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res,
		binder.FieldError{Source: "path", Field: "id", Message: "is not a valid value"},
		binder.FieldError{Source: "query", Field: "Skip", Message: "is not allowed"},
		binder.FieldError{Source: "query", Field: "limit", Message: "must not be repeated"},
		binder.FieldError{Source: "query", Field: "min", Message: "is not a valid value"},
	)

	// Validation names fields of embedded structs without the embedding.
	res = serve(t, r, httptest.NewRequest("GET", "/items/019cfa3a-f6a2-7eba-b1da-c7f1bac023f5?limit=500", nil))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res, binder.FieldError{Source: "query", Field: "limit", Message: "must be at most 100"})
}

// ---------------------------------------------------------------------------
// Suggestion 7: size messages say what they measure
// ---------------------------------------------------------------------------

func TestSizeMessages(t *testing.T) {
	r := http.NewServeMux()
	r.HandleFunc("POST /", defaultBinder.Bind(noop[Sizes]))

	res := serve(t, r, jsonReq("POST", "/", `{"name":"ab","tags":["a","b"],"count":3}`))
	wantStatus(t, res, http.StatusBadRequest)
	wantErrors(t, res,
		binder.FieldError{Source: "body", Field: "name", Message: "must be at least 3 characters long"},
		binder.FieldError{Source: "body", Field: "tags", Message: "must contain at most 1 items"},
		binder.FieldError{Source: "body", Field: "count", Message: "must be at least 10"},
	)
}

// ---------------------------------------------------------------------------
// Registration panics carry the package name
// ---------------------------------------------------------------------------

func TestPlanPanics(t *testing.T) {
	type Page struct {
		Limit int `json:"limit"`
	}
	tests := []struct {
		name string
		bind func()
		want string
	}{
		{"not a func", func() { defaultBinder.Bind(42) }, "handler must be a non-nil func"},
		{"nil handler", func() { defaultBinder.Bind(nil) }, "handler must be a non-nil func"},
		{"typed nil handler", func() { defaultBinder.Bind((func() error)(nil)) }, "handler must be a non-nil func"},
		{"variadic", func() { defaultBinder.Bind(func(...int) error { return nil }) }, "must not be variadic"},
		{"no result", func() { defaultBinder.Bind(func() {}) }, "must return error or (T, error)"},
		{"no error result", func() { defaultBinder.Bind(func() int { return 0 }) }, "must return error or (T, error)"},
		{"unknown argument", func() { defaultBinder.Bind(func(int) error { return nil }) },
			"takes int, which is neither built in, provided nor an input struct"},
		{"duplicate argument", func() {
			defaultBinder.Bind(func(context.Context, context.Context) error { return nil })
		}, "takes context.Context twice"},
		{"two input structs", func() {
			defaultBinder.Bind(func(OrgOnly, NamePath) error { return nil })
		}, "takes two input structs"},
		{"empty input struct", func() { defaultBinder.Bind(noop[struct{}]) },
			"has no path or query fields and no Body"},
		{"untagged field", func() { defaultBinder.Bind(noop[struct{ A int }]) },
			"A has no `path` or `query` tag"},
		{"untagged field in embedded struct", func() {
			defaultBinder.Bind(noop[struct {
				Page
				Q string `query:"q"`
			}])
		}, "Limit has no `path` or `query` tag"},
		{"two sources", func() {
			defaultBinder.Bind(noop[struct {
				A int `path:"a" query:"a"`
			}])
		}, "has both a `path` and a `query` tag"},
		{"option in a query tag", func() {
			defaultBinder.Bind(noop[struct {
				Q string `query:"q,required"`
			}])
		}, "the `query` tag must be just a name"},
		{"unnamed path tag", func() {
			defaultBinder.Bind(noop[struct {
				ID int `path:""`
			}])
		}, "the `path` tag must be just a name"},
		{"nested struct in the query", func() {
			defaultBinder.Bind(noop[struct {
				Page struct{ N int } `query:"page"`
			}])
		}, "can't be decoded from a query parameter"},
		{"slice in the query", func() {
			defaultBinder.Bind(noop[struct {
				IDs []int `query:"id"`
			}])
		}, "can't be decoded from a query parameter"},
		{"unexported path field", func() {
			defaultBinder.Bind(noop[struct {
				Q  string `query:"q"`
				id string `path:"id"`
			}])
		}, "id is unexported, so its `path` tag is ignored"},
		{"unexported query field", func() {
			defaultBinder.Bind(noop[struct {
				Q string `query:"q"`
				n int    `query:"n"`
			}])
		}, "n is unexported, so its `query` tag is ignored"},
		{"parameter declared twice", func() {
			defaultBinder.Bind(noop[struct {
				ThreeInts
				Also int `query:"b"`
			}])
		}, `two fields for the query parameter "b"`},
		{"pointer body", func() { defaultBinder.Bind(noop[struct{ Body *Page }]) },
			"Body is a *binder_test.Page; it must be a struct"},
		{"slice body", func() { defaultBinder.Bind(noop[struct{ Body []Page }]) },
			"Body is a []binder_test.Page; it must be a struct"},
		{"query tag in the body", func() {
			defaultBinder.Bind(noop[struct {
				Body struct {
					A int `query:"a"`
				}
			}])
		}, "Body.A has a `path` or `query` tag but no `json` tag"},
		{"provide a built-in type", func() {
			binder.Provide(func(r *http.Request) (*http.Request, error) { return r, nil })
		}, "Provide(*http.Request): Bind fills it already"},
		{"provide nil", func() { binder.Provide[*user](nil) }, "Provide(*binder_test.user): nil func"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.HasPrefix(msg, "binder: ") || !strings.Contains(msg, tt.want) {
					t.Fatalf("panic = %q, want binder-prefixed message containing %q", msg, tt.want)
				}
			}()
			tt.bind()
		})
	}
}

// ---------------------------------------------------------------------------
// Arguments are filled by type
// ---------------------------------------------------------------------------

type ctxKey struct{}

func TestInjection(t *testing.T) {
	type tenant struct{ Name string } // a struct, but provided: not an input
	me := &user{ID: 7}
	b := binder.NewBinder(
		binder.Provide(func(*http.Request) (*user, error) { return me, nil }),
		binder.Provide(func(*http.Request) (tenant, error) { return tenant{"acme"}, nil }),
	)

	var called bool
	r := http.NewServeMux()
	// Any order; the input struct last but one, to show position doesn't matter.
	r.HandleFunc("GET /orgs/{org_id}", b.Bind(func(u *user, ctx context.Context, in OrgOnly, req *http.Request, tn tenant) error {
		called = true
		if u != me || in.OrgID != 3 || tn.Name != "acme" || req.URL.Path != "/orgs/3" || ctx.Value(ctxKey{}) != "v" {
			t.Errorf("args = %v %+v %+v %v", u, in, tn, ctx.Value(ctxKey{}))
		}
		return nil
	}))

	req := httptest.NewRequest("GET", "/orgs/3", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "v"))
	wantStatus(t, serve(t, r, req), http.StatusNoContent)
	if !called {
		t.Fatal("handler not called")
	}
}

func TestProviderRunsOnlyWhenAsked(t *testing.T) {
	calls := 0
	b := binder.NewBinder(binder.Provide(func(*http.Request) (*user, error) { calls++; return &user{}, nil }))

	wantStatus(t, serve(t, b.Bind(func() error { return nil }), httptest.NewRequest("GET", "/", nil)), http.StatusNoContent)
	if calls != 0 {
		t.Fatalf("provider ran %d times for a handler that doesn't take its type", calls)
	}
	wantStatus(t, serve(t, b.Bind(func(*user) error { return nil }), httptest.NewRequest("GET", "/", nil)), http.StatusNoContent)
	if calls != 1 {
		t.Fatalf("provider ran %d times, want 1", calls)
	}
}

// A provider's error is answered before the input is decoded, through the
// error mapper, and the handler never runs.
func TestProviderError(t *testing.T) {
	errAnon := errors.New("anonymous")
	b := binder.NewBinder(
		binder.WithErrorHandler(nil),
		binder.Provide(func(*http.Request) (*user, error) { return nil, errAnon }),
		binder.WithErrorMapper(func(err error) error {
			if errors.Is(err, errAnon) {
				return binder.Error(http.StatusUnauthorized, "log in", err)
			}
			return err
		}),
	)
	h := b.Bind(func(*user, QueryRequired) error {
		t.Error("handler called")
		return nil
	})

	res := serve(t, h, httptest.NewRequest("GET", "/", nil)) // q is missing too
	wantStatus(t, res, http.StatusUnauthorized)
	if res.body.Error != "log in" {
		t.Fatalf("error = %q, want the mapped message", res.body.Error)
	}
}

var errNotFound = errors.New("not found")

func TestErrorMapper(t *testing.T) {
	b := binder.NewBinder(
		binder.WithErrorHandler(nil),
		binder.WithErrorMapper(func(err error) error {
			if errors.Is(err, errNotFound) {
				return binder.Error(http.StatusNotFound, "", err)
			}
			if err.Error() == "nil" {
				return nil
			}
			return err
		}),
	)
	tests := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("loading user: %w", errNotFound), http.StatusNotFound},
		{errors.New("boom"), http.StatusInternalServerError},
		{errors.New("nil"), http.StatusInternalServerError},
		{binder.Error(http.StatusConflict, "", nil), http.StatusConflict},
	}
	for _, tt := range tests {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /", b.Bind(func() error { return tt.err }))
		wantStatus(t, serve(t, mux, httptest.NewRequest("GET", "/", nil)), tt.want)
	}
}
