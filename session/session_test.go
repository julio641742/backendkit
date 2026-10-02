package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/julio641742/backendkit/httperr"
)

// cookieMaxAge is the Max-Age of the cookies of a session just logged in.
const cookieMaxAge = int(absoluteLifetime / time.Second)

const sessionCookie = cookieName

var (
	alice = testUser{ID: 1, Email: "alice@example.com"}
	bob   = testUser{ID: 2, Email: "bob@example.com"}
)

type testCtxKey string

// harness runs requests through the middleware like a browser would: cookies
// set by one response are sent with the next request.
type harness struct {
	t       *testing.T
	store   *memStore
	mw      func(http.Handler) http.Handler
	cookies map[string]*http.Cookie

	mu     sync.Mutex
	errs   []error
	errCtx []context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{t: t, store: newMemStore(), cookies: map[string]*http.Cookie{}}
	h.mw = middleware(h.store, h.report)
	return h
}

// report records the errors the middleware reports, in place of logging.
func (h *harness) report(ctx context.Context, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, err)
	h.errCtx = append(h.errCtx, ctx)
}

func (h *harness) handledErrors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

type requestOpt func(*http.Request)

// withCsrf sends the CSRF cookie value back as the header, like the frontend would.
func (h *harness) withCsrf() requestOpt {
	return func(r *http.Request) {
		if c, ok := h.cookies[csrfCookieName]; ok {
			r.Header.Set(csrfHeaderName, c.Value)
		}
	}
}

func withHeader(key, value string) requestOpt {
	return func(r *http.Request) { r.Header.Set(key, value) }
}

func (h *harness) do(method string, handler func(s *Session[testUser], w http.ResponseWriter), opts ...requestOpt) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(method, "https://example.com/", nil)
	req = req.WithContext(context.WithValue(req.Context(), testCtxKey("request_id"), "req-1"))
	for _, opt := range opts {
		opt(req)
	}
	return h.serve(req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(From[testUser](r), w)
	}))
}

// serve runs req through the middleware to handler with the harness's
// cookies, and keeps the cookies the response sets.
func (h *harness) serve(req *http.Request, handler http.Handler) *httptest.ResponseRecorder {
	h.t.Helper()
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.mw(handler).ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(h.cookies, c.Name)
		} else {
			h.cookies[c.Name] = c
		}
	}
	return rec
}

func (h *harness) mustSave(s *Session[testUser]) {
	h.t.Helper()
	if err := s.Save(); err != nil {
		h.t.Fatalf("Save: %v", err)
	}
}

func (h *harness) login(user testUser) {
	h.t.Helper()
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.SetUser(&user); err != nil {
			h.t.Fatalf("SetUser: %v", err)
		}
		h.mustSave(s)
	})
}

// sessionKey is the stored (hashed) id of the current session cookie.
func (h *harness) sessionKey() string {
	h.t.Helper()
	return hashID(h.sessionId())
}

// sessionId is the raw session id: the session cookie's value.
func (h *harness) sessionId() string {
	h.t.Helper()
	c, ok := h.cookies[sessionCookie]
	if !ok {
		h.t.Fatal("no session cookie")
	}
	return c.Value
}

func (h *harness) currentUser() (user, realUser *testUser, impersonating bool) {
	h.t.Helper()
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		user, realUser, impersonating = s.GetUser(), s.GetRealUser(), s.IsImpersonating()
	})
	return
}

func TestAnonymousRequest(t *testing.T) {
	h := newHarness(t)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		called := false
		rec := h.do(method, func(s *Session[testUser], w http.ResponseWriter) {
			called = true
			if u := s.GetUser(); u != nil {
				t.Errorf("GetUser = %+v, want nil", u)
			}
			if err := s.Save(); err != nil { // nothing written, no-op
				t.Errorf("Save: %v", err)
			}
		})

		if !called || rec.Code != http.StatusOK {
			t.Errorf("%s: called=%v code=%d, anonymous requests should pass", method, called, rec.Code)
		}
	}
	if len(h.cookies) != 0 {
		t.Errorf("anonymous requests set cookies: %v", h.cookies)
	}
	if n := h.store.writeCount(); n != 0 {
		t.Errorf("anonymous requests ran %d execs", n)
	}
}

func TestLogin(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	sc, cc := h.cookies[sessionCookie], h.cookies[csrfCookieName]
	if sc == nil || cc == nil {
		t.Fatalf("missing cookies: %v", h.cookies)
	}
	if !sc.HttpOnly || !sc.Secure || sc.Domain != "" || sc.MaxAge < cookieMaxAge-1 || sc.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v", sc)
	}
	if cc.HttpOnly {
		t.Error("CSRF cookie must be readable by JS")
	}

	row, ok := h.store.get(h.sessionKey())
	if !ok {
		t.Fatal("session not stored")
	}
	if *row.User != alice || row.ImpersonatedUser != nil {
		t.Errorf("row = %+v", row)
	}
	if d := time.Until(row.ExpiresOn); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("expires in %v, want ~24h", d)
	}

	user, realUser, impersonating := h.currentUser()
	if user == nil || *user != alice || *realUser != alice || impersonating {
		t.Errorf("user=%v real=%v impersonating=%v", user, realUser, impersonating)
	}
}

func TestLoginRotatesSessionID(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	first := h.sessionKey()

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.SetUser(&bob); err != nil {
			t.Fatalf("SetUser: %v", err)
		}
		h.mustSave(s)
	}, h.withCsrf())

	if h.sessionKey() == first {
		t.Error("session id was not rotated")
	}
	if _, ok := h.store.get(first); ok {
		t.Error("old session was not deleted")
	}
	if n := h.store.count(); n != 1 {
		t.Errorf("sessions = %d, want 1", n)
	}
	if user, _, _ := h.currentUser(); user == nil || *user != bob {
		t.Errorf("user = %v, want bob", user)
	}
}

func TestCsrf(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	tests := []struct {
		name string
		opts []requestOpt
		want int
	}{
		{"missing token", nil, http.StatusForbidden},
		{"wrong token", []requestOpt{withHeader(csrfHeaderName, "nope")}, http.StatusForbidden},
		{"valid token", []requestOpt{h.withCsrf()}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				called := false
				rec := h.do(method, func(s *Session[testUser], w http.ResponseWriter) { called = true }, tt.opts...)
				if rec.Code != tt.want || called != (tt.want == http.StatusOK) {
					t.Errorf("%s: code=%d called=%v, want %d", method, rec.Code, called, tt.want)
				}
			}
		})
	}

	t.Run("safe methods skip the check", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			if rec := h.do(method, func(*Session[testUser], http.ResponseWriter) {}); rec.Code != http.StatusOK {
				t.Errorf("%s: code=%d", method, rec.Code)
			}
		}
	})
}

func TestRequireUser(t *testing.T) {
	h := newHarness(t)
	called := false
	handler := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.mw(RequireUser[testUser](http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))).ServeHTTP(rec, r)
		return rec
	}

	rec := handler(httptest.NewRequest(http.MethodGet, "https://example.com/", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Errorf("no session: code=%d called=%v, want 401", rec.Code, called)
	}
	var body httperr.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("body = %s (%v), want the httperr envelope", rec.Body, err)
	}

	h.login(alice)
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.AddCookie(h.cookies[sessionCookie])
	if rec := handler(req); rec.Code != http.StatusOK || !called {
		t.Errorf("logged in: code=%d called=%v, want 200", rec.Code, called)
	}
}

func TestImpersonation(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	oldToken := h.cookies[csrfCookieName].Value

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.ImpersonateUser(&bob); err != nil {
			t.Fatalf("ImpersonateUser: %v", err)
		}
		if *s.GetUser() != bob {
			t.Error("impersonation not visible in the same request")
		}
		h.mustSave(s)
	}, h.withCsrf())

	if row, _ := h.store.get(h.sessionKey()); *row.User != alice || row.ImpersonatedUser == nil || *row.ImpersonatedUser != bob {
		t.Errorf("row = %+v", row)
	}

	user, realUser, impersonating := h.currentUser()
	if *user != bob || *realUser != alice || !impersonating {
		t.Errorf("user=%v real=%v impersonating=%v", user, realUser, impersonating)
	}

	if h.cookies[csrfCookieName].Value == oldToken {
		t.Error("CSRF token was not rotated")
	}
	if rec := h.do(http.MethodPost, func(*Session[testUser], http.ResponseWriter) {}, withHeader(csrfHeaderName, oldToken)); rec.Code != http.StatusForbidden {
		t.Errorf("old CSRF token accepted: %d", rec.Code)
	}

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		s.StopImpersonatingUser()
		h.mustSave(s)
	}, h.withCsrf())

	user, realUser, impersonating = h.currentUser()
	if *user != alice || *realUser != alice || impersonating {
		t.Errorf("after stop: user=%v real=%v impersonating=%v", user, realUser, impersonating)
	}
	if row, _ := h.store.get(h.sessionKey()); row.ImpersonatedUser != nil {
		t.Errorf("impersonated user = %v after stop", row.ImpersonatedUser)
	}
}

func TestImpersonateWithoutUser(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.ImpersonateUser(&bob); !errors.Is(err, ErrNoUser) {
			t.Errorf("ImpersonateUser = %v, want ErrNoUser", err)
		}
	})
}

func TestImpersonateNil(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.ImpersonateUser(nil); !errors.Is(err, ErrNoUser) {
			t.Errorf("ImpersonateUser(nil) = %v, want ErrNoUser", err)
		}
		if s.IsImpersonating() {
			t.Error("impersonating after a rejected nil user")
		}
	})
}

func TestSetUserNil(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	id := h.sessionKey()
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.SetUser(nil); !errors.Is(err, ErrNoUser) {
			t.Errorf("SetUser(nil) = %v, want ErrNoUser", err)
		}
		if u := s.GetUser(); u == nil || *u != alice {
			t.Errorf("user = %v after a rejected nil user, want alice", u)
		}
	}, h.withCsrf())
	if _, ok := h.store.get(id); !ok {
		t.Error("SetUser(nil) destroyed the stored session")
	}
}

func TestDestroy(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	id := h.sessionId()

	rec := h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.Destroy(); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if s.GetUser() != nil {
			t.Error("user still set after Destroy")
		}
		h.mustSave(s) // no-op after Destroy
	}, h.withCsrf())

	if _, ok := h.store.get(id); ok {
		t.Error("session row not deleted")
	}
	expired := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		expired[c.Name] = c.MaxAge < 0
	}
	if !expired[sessionCookie] || !expired[csrfCookieName] {
		t.Errorf("cookies not expired: %v", rec.Result().Cookies())
	}
	if n := h.store.count(); n != 0 {
		t.Errorf("sessions = %d, want 0", n)
	}
}

func TestDestroyAfterResponseStarted(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		s.GetUser()
		w.WriteHeader(http.StatusOK)
		if err := s.Destroy(); !errors.Is(err, ErrResponseStarted) {
			t.Errorf("Destroy = %v, want ErrResponseStarted", err)
		}
	}, h.withCsrf())
	if _, ok := h.store.get(key); ok {
		t.Error("session row not deleted")
	}
}

func TestDestroyWithoutSession(t *testing.T) {
	h := newHarness(t)
	// A leftover cookie is cleared, even without a session behind it.
	h.cookies[csrfCookieName] = &http.Cookie{Name: csrfCookieName, Value: "stale"}
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.Destroy(); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	})
	if len(h.cookies) != 0 {
		t.Errorf("cookies = %v, want them cleared", h.cookies)
	}
}

func TestInvalidCookies(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()
	valid := h.cookies[sessionCookie].Value

	tests := []struct {
		name  string
		value string
		setup func()
	}{
		{"tampered", valid[:len(valid)-2] + "AA", nil},
		{"garbage", "!!!", nil},
		{"empty", "", nil},
		{"hashed id as cookie", key, nil},
		{"expired", valid, func() { h.store.sessions[key].ExpiresOn = time.Now().Add(-time.Minute) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
			}
			h.cookies = map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: tt.value}}
			if user, _, _ := h.currentUser(); user != nil {
				t.Errorf("user = %v, want nil", user)
			}
		})
	}
	if errs := h.handledErrors(); len(errs) != 0 {
		t.Errorf("invalid cookies should not be reported: %v", errs)
	}
}

func TestErrorHandler(t *testing.T) {
	t.Run("load error", func(t *testing.T) {
		h := newHarness(t)
		h.login(alice)
		boom := errors.New("connection refused")
		h.store.loadErr = boom

		called := false
		rec := h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
			called = true
			if s.GetUser() != nil {
				t.Error("user set after load error")
			}
		}, h.withCsrf())

		if !called || rec.Code != http.StatusOK {
			t.Errorf("request should continue as anonymous: called=%v code=%d", called, rec.Code)
		}
		errs := h.handledErrors()
		if len(errs) != 1 || !errors.Is(errs[0], boom) {
			t.Fatalf("errors = %v, want %v", errs, boom)
		}
		if got := h.errCtx[0].Value(testCtxKey("request_id")); got != "req-1" {
			t.Errorf("handler ctx request_id = %v, want the request context", got)
		}
	})
}

func TestSaveErrorIsReturned(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("disk full")
	h.store.writeErr = boom

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.SetUser(&alice)
		if err := s.Save(); !errors.Is(err, boom) {
			t.Errorf("Save = %v, want %v", err, boom)
		}
	})
	if len(h.cookies) != 0 {
		t.Errorf("cookies set after failed save: %v", h.cookies)
	}
}

func TestSessionIDIsHashedInStore(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	if _, ok := h.store.get(h.sessionId()); ok {
		t.Error("raw session id stored in the database")
	}
	if key := h.sessionKey(); len(key) != 64 {
		t.Errorf("stored id = %q, want 64 hex chars", key)
	}
	if _, ok := h.store.get(h.sessionKey()); !ok {
		t.Error("session not stored under the hashed id")
	}
	// The CSRF cookie is readable by scripts: it must not reveal either id.
	if token := h.cookies[csrfCookieName].Value; token == h.sessionId() || token == h.sessionKey() || token == "" {
		t.Errorf("CSRF token %q reveals the session id", token)
	}
}

func TestCsrfCookieMatchesAxiosDefaults(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	if _, ok := h.cookies["XSRF-TOKEN"]; !ok {
		t.Errorf("no XSRF-TOKEN cookie: %v", h.cookies)
	}
	if rec := h.do(http.MethodPost, func(*Session[testUser], http.ResponseWriter) {},
		withHeader("X-XSRF-TOKEN", h.cookies["XSRF-TOKEN"].Value)); rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCrossOriginProtection(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name string
		opts []requestOpt
		want int
	}{
		{"no browser headers", nil, http.StatusOK},
		{"same-origin", []requestOpt{withHeader("Sec-Fetch-Site", "same-origin")}, http.StatusOK},
		{"sibling subdomain", []requestOpt{withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", "https://evil.example.com")}, http.StatusForbidden},
		{"cross-site", []requestOpt{withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", "https://evil.test")}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// anonymous, like a login form
			called := false
			rec := h.do(http.MethodPost, func(*Session[testUser], http.ResponseWriter) { called = true }, tt.opts...)
			if rec.Code != tt.want || called != (tt.want == http.StatusOK) {
				t.Errorf("code=%d called=%v, want %d", rec.Code, called, tt.want)
			}
		})
	}

	t.Run("safe methods skip the check", func(t *testing.T) {
		rec := h.do(http.MethodGet, func(*Session[testUser], http.ResponseWriter) {}, withHeader("Sec-Fetch-Site", "cross-site"))
		if rec.Code != http.StatusOK {
			t.Errorf("code = %d", rec.Code)
		}
	})
}

func TestCsrfFailureResponse(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	for want, opts := range map[string][]requestOpt{
		"CSRF token mismatch":           nil,
		"cross-origin request rejected": {withHeader("Sec-Fetch-Site", "cross-site")},
	} {
		rec := h.do(http.MethodPost, func(*Session[testUser], http.ResponseWriter) {}, opts...)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: code = %d", want, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q", want, ct)
		}
		var body httperr.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != want || body.Errors == nil {
			t.Errorf("body = %s (%v), want %q", rec.Body, err, want)
		}
	}
}

func TestCookieOptions(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	for _, name := range []string{"__Host-session", csrfCookieName} {
		c := h.cookies[name]
		if c == nil {
			t.Fatalf("no %s cookie: %v", name, h.cookies)
		}
		// Host-only and kept until the absolute lifetime: the Store slides
		// the expiry.
		if !c.Secure || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode || c.MaxAge < cookieMaxAge-1 {
			t.Errorf("%s cookie = %+v", name, c)
		}
	}
	if !h.cookies[sessionCookie].HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if user, _, _ := h.currentUser(); user == nil || *user != alice {
		t.Errorf("user = %v, want alice", user)
	}
}

func TestSlidingRenewal(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()

	// More than half of the lifetime left: nothing is written.
	execs := h.store.writeCount()
	rec := h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
	if len(rec.Result().Cookies()) != 0 || h.store.writeCount() != execs {
		t.Errorf("renewed too early: cookies=%v execs=%d", rec.Result().Cookies(), h.store.writeCount()-execs)
	}

	// Past half: the expiry slides in the Store alone. The cookies already
	// last until the absolute lifetime, so none are sent.
	h.store.sessions[key].ExpiresOn = time.Now().Add(time.Hour)
	rec = h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })

	if d := time.Until(h.store.sessions[key].ExpiresOn); d < 23*time.Hour {
		t.Errorf("expires in %v after renewal, want ~24h", d)
	}
	if c := rec.Result().Cookies(); len(c) != 0 {
		t.Errorf("renewal sent cookies: %v", c)
	}
	if user, _, _ := h.currentUser(); user == nil || *user != alice {
		t.Errorf("user = %v after renewal", user)
	}

	t.Run("renewal error is reported, request continues", func(t *testing.T) {
		h.store.sessions[key].ExpiresOn = time.Now().Add(time.Hour)
		boom := errors.New("read only")
		h.store.writeErr = boom
		defer func() { h.store.writeErr = nil }()

		var user *testUser
		h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { user = s.GetUser() })
		if user == nil {
			t.Error("user lost when renewal failed")
		}
		if errs := h.handledErrors(); len(errs) != 1 || !errors.Is(errs[0], boom) {
			t.Errorf("errors = %v", errs)
		}
	})
}

func TestAbsoluteLifetime(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()

	// Logged in 30 days minus 1h ago: renewal stops at the 30 day mark.
	row := h.store.sessions[key]
	row.CreatedOn = time.Now().Add(-absoluteLifetime + time.Hour)
	row.ExpiresOn = time.Now().Add(10 * time.Minute)
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
	if d := time.Until(row.ExpiresOn); d > time.Hour || d < 50*time.Minute {
		t.Errorf("expires in %v, want capped at ~1h", d)
	}

	// Save caps too.
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.ImpersonateUser(&bob)
		h.mustSave(s)
	}, h.withCsrf())
	row, _ = h.store.get(h.sessionKey())
	if d := time.Until(row.ExpiresOn); d > time.Hour {
		t.Errorf("expires in %v after Save, want capped at ~1h", d)
	}
	if c := h.cookies[sessionCookie]; c.MaxAge > 3600 {
		t.Errorf("cookie MaxAge = %d, want capped", c.MaxAge)
	}
}

func TestFrom(t *testing.T) {
	assertPanicMsg := func(want string, fn func()) {
		t.Helper()
		defer func() {
			if msg, _ := recover().(string); !strings.Contains(msg, want) {
				t.Errorf("panic = %q, want it to mention %q", msg, want)
			}
		}()
		fn()
	}

	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	assertPanicMsg("Middleware", func() { From[testUser](bare) })

	s := &Session[testUser]{}
	req := bare.WithContext(context.WithValue(bare.Context(), ctxKey{}, s))
	if From[testUser](req) != s {
		t.Error("From did not return the session")
	}
	assertPanicMsg("Middleware", func() { From[otherUser](req) })
}

type otherUser struct{}

func (otherUser) GetID() any { return 0 }

// The cookies outlive any expiry renewal sets, so a read after the response
// started renews too.
func TestLateReadRenews(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()
	h.store.sessions[key].ExpiresOn = time.Now().Add(time.Hour)

	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		if s.GetUser() == nil {
			t.Error("user lost on late read")
		}
	})
	if d := time.Until(h.store.sessions[key].ExpiresOn); d < 23*time.Hour {
		t.Errorf("expires in %v, want renewed on a late read", d)
	}
	if errs := h.handledErrors(); len(errs) != 0 {
		t.Errorf("errors = %v", errs)
	}
}

func TestSaveAfterResponseStarted(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.SetUser(&alice)
		_, _ = w.Write([]byte("hi"))
		if err := s.Save(); !errors.Is(err, ErrResponseStarted) {
			t.Errorf("Save = %v, want ErrResponseStarted", err)
		}
	})
	if n := h.store.count(); n != 0 {
		t.Errorf("%d sessions stored without a cookie", n)
	}
	if len(h.cookies) != 0 {
		t.Errorf("cookies = %v", h.cookies)
	}
}

func TestSaveAfterAbsoluteLifetime(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()

	// Still loadable (e.g. clock skew or a long request) but past the cap.
	row := h.store.sessions[key]
	row.CreatedOn = time.Now().Add(-absoluteLifetime - time.Hour)
	row.ExpiresOn = time.Now().Add(time.Hour)

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.ImpersonateUser(&bob)
		if err := s.Save(); !errors.Is(err, ErrExpired) {
			t.Errorf("Save = %v, want ErrExpired", err)
		}
	}, h.withCsrf())
	if _, ok := h.store.get(key); ok {
		t.Error("expired session still stored")
	}
	if _, ok := h.cookies[sessionCookie]; ok {
		t.Error("session cookie not cleared")
	}
}

func TestUnsavedChangesAreReported(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) { _ = s.ImpersonateUser(&bob) }, h.withCsrf())
	if errs := h.handledErrors(); len(errs) != 1 || !errors.Is(errs[0], ErrUnsaved) {
		t.Fatalf("errors = %v, want ErrUnsaved", errs)
	}

	// Saved, destroyed or failed saves are the handler's business.
	for _, fn := range []func(*Session[testUser]){
		func(s *Session[testUser]) { _ = s.ImpersonateUser(&bob); h.mustSave(s) },
		func(s *Session[testUser]) { _ = s.ImpersonateUser(&bob); _ = s.Destroy() },
		func(s *Session[testUser]) { s.GetUser() },
	} {
		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) { fn(s) }, h.withCsrf())
	}
	if errs := h.handledErrors(); len(errs) != 1 {
		t.Errorf("errors = %v, want only the first", errs)
	}
}

func TestImpersonationRotatesSessionID(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	first := h.sessionKey()
	created := h.store.sessions[first].CreatedOn

	for _, fn := range []func(*Session[testUser]){
		func(s *Session[testUser]) { _ = s.ImpersonateUser(&bob) },
		func(s *Session[testUser]) { s.StopImpersonatingUser() },
	} {
		before := h.sessionKey()
		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
			fn(s)
			h.mustSave(s)
		}, h.withCsrf())

		if h.sessionKey() == before {
			t.Error("session id was not rotated")
		}
		if _, ok := h.store.get(before); ok {
			t.Error("old session was not deleted")
		}
		if n := h.store.count(); n != 1 {
			t.Errorf("sessions = %d, want 1", n)
		}
		row, _ := h.store.get(h.sessionKey())
		if !row.CreatedOn.Equal(created) {
			t.Errorf("CreatedOn = %v, want %v kept for the absolute lifetime", row.CreatedOn, created)
		}
	}

	t.Run("unsaved rotation keeps the old session", func(t *testing.T) {
		before := h.sessionKey()
		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) { _ = s.ImpersonateUser(&bob) }, h.withCsrf())
		if _, ok := h.store.get(before); !ok || h.sessionKey() != before {
			t.Error("session changed without Save")
		}
	})

	t.Run("destroy after rotation deletes the old session", func(t *testing.T) {
		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
			_ = s.ImpersonateUser(&bob)
			if err := s.Destroy(); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		}, h.withCsrf())
		if n := h.store.count(); n != 0 {
			t.Errorf("sessions = %d after Destroy", n)
		}
	})
}

func TestSessionDeletedElsewhere(t *testing.T) {
	t.Run("renewal", func(t *testing.T) {
		h := newHarness(t)
		h.login(alice)
		h.store.sessions[h.sessionKey()].ExpiresOn = time.Now().Add(time.Hour)
		h.store.writeErr = ErrNotFound // deleted between Load and Touch

		var user *testUser
		h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { user = s.GetUser() })
		if user != nil {
			t.Errorf("user = %v, want anonymous", user)
		}
		if errs := h.handledErrors(); len(errs) != 0 {
			t.Errorf("errors = %v, want none", errs)
		}
		if _, ok := h.cookies[sessionCookie]; !ok {
			t.Error("the session cookie was expired; it may hold a rotated session")
		}
	})

	t.Run("load", func(t *testing.T) {
		h := newHarness(t)
		h.login(alice)
		delete(h.store.sessions, h.sessionKey())

		var user *testUser
		rec := h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { user = s.GetUser() })
		if user != nil {
			t.Errorf("user = %v, want anonymous", user)
		}
		if c := rec.Result().Cookies(); len(c) != 0 {
			t.Errorf("cookies = %v, want them left alone", c)
		}
	})

	t.Run("destroy still clears the cookies", func(t *testing.T) {
		h := newHarness(t)
		h.login(alice)
		delete(h.store.sessions, h.sessionKey())

		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
			if err := s.Destroy(); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		}, h.withCsrf())
		if len(h.cookies) != 0 {
			t.Errorf("cookies = %v, want them cleared", h.cookies)
		}
	})
}

// A request still carrying the pre-rotation cookie must not log the browser
// out of the rotated session when its response arrives last.
func TestParallelRequestDuringRotation(t *testing.T) {
	clone := func(m map[string]*http.Cookie) map[string]*http.Cookie {
		c := make(map[string]*http.Cookie, len(m))
		for k, v := range m {
			c[k] = v
		}
		return c
	}
	impersonate := func(h *harness) {
		h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
			_ = s.ImpersonateUser(&bob)
			h.mustSave(s)
		}, h.withCsrf())
	}

	for _, tt := range []struct {
		name string
		// run sends request B with the old cookies. It calls rotate (request
		// A, sent with the same old cookies) at the chosen point.
		run func(h *harness, rotate func()) *httptest.ResponseRecorder
	}{
		{"B loads after A saved", func(h *harness, rotate func()) *httptest.ResponseRecorder {
			rotate()
			return h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
		}},
		{"B renews after A saved", func(h *harness, rotate func()) *httptest.ResponseRecorder {
			// Due for renewal, so B's first read goes on to Touch the
			// session A deleted meanwhile.
			h.store.sessions[h.sessionKey()].ExpiresOn = time.Now().Add(time.Hour)
			h.store.loadHook = func() { h.store.loadHook = nil; rotate() }
			return h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.login(alice)
			old := clone(h.cookies)

			var rotated map[string]*http.Cookie
			rotate := func() {
				saved := h.cookies
				h.cookies = clone(old)
				impersonate(h)
				rotated = clone(h.cookies)
				h.cookies = saved
			}
			rec := tt.run(h, rotate)
			if rotated == nil {
				t.Fatal("request A never ran")
			}

			// The browser applies B's response last.
			h.cookies = rotated
			for _, c := range rec.Result().Cookies() {
				if c.MaxAge < 0 {
					delete(h.cookies, c.Name)
				} else {
					h.cookies[c.Name] = c
				}
			}
			if user, _, impersonating := h.currentUser(); user == nil || user.ID != bob.ID || !impersonating {
				t.Errorf("user = %v, impersonating = %v; the rotated session was lost", user, impersonating)
			}
		})
	}
}

// laxStore is a Store whose Load forgets to filter expired sessions.
type laxStore struct {
	*memStore
}

func (l *laxStore) Load(_ context.Context, id string) (*Data[testUser], error) {
	d, ok := l.get(id)
	if !ok {
		return nil, ErrNotFound
	}
	return cloneData(d), nil
}

func TestExpiredSessionIsNotRevived(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	key := h.sessionKey()
	d, _ := h.store.get(key)
	d.ExpiresOn = time.Now().Add(-time.Minute)

	lax := &laxStore{memStore: h.store}
	h.mw = middleware(lax, h.report)
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		if s.GetUser() != nil {
			t.Error("an expired session was loaded")
		}
	})
	if d, _ := h.store.get(key); d.ExpiresOn.After(time.Now()) {
		t.Error("an expired session was renewed")
	}
}

func TestCookieResponsesAreNotCached(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.SetUser(&alice)
		h.mustSave(s)
	})
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("login Cache-Control = %q, want no-store", got)
	}

	// Cookies win over the handler's own Cache-Control.
	rec = h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		if err := s.Destroy(); err != nil {
			t.Fatal(err)
		}
	}, h.withCsrf())
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("logout Cache-Control = %q, want no-store", got)
	}

	rec = h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q on an anonymous response without cookies", got)
	}
}

// A renewal sends the session cookie without the handler knowing, so even a
// handler that marks its response public (a static asset behind RequireUser)
// must not let a shared cache store, and replay, that cookie.
func TestResentCookieIsNeverPubliclyCached(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	delete(h.cookies, csrfCookieName)

	rec := h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		s.GetUser()
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write([]byte("asset"))
	})
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("CSRF cookie was not resent")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on a response carrying a session cookie, want no-store", got)
	}
}

// A response for a logged in user is personal even without a cookie, so it
// must not be stored by a shared cache either.
func TestUserResponsesAreNotCached(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	for name, handler := range map[string]func(*Session[testUser], http.ResponseWriter){
		"written": func(s *Session[testUser], w http.ResponseWriter) {
			s.GetUser()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"email":"alice@example.com"}`))
		},
		"not written": func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() },
	} {
		rec := h.do(http.MethodGet, handler)
		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("%s: Cache-Control = %q, want private, no-store", name, got)
		}
	}

	rec := h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) {
		s.GetUser()
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
	})
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=60" {
		t.Errorf("handler Cache-Control was replaced: %q", got)
	}

	// A handler that never reads the session sends nothing personal.
	rec = h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { w.WriteHeader(http.StatusOK) })
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q on a response that never read the session", got)
	}
}

// ctxStore records whether the context each write got was already canceled.
type ctxStore struct {
	*memStore
	canceled []bool
}

func (c *ctxStore) Insert(ctx context.Context, data *Data[testUser]) error {
	c.canceled = append(c.canceled, ctx.Err() != nil)
	return c.memStore.Insert(ctx, data)
}

func (c *ctxStore) Destroy(ctx context.Context, id string) error {
	c.canceled = append(c.canceled, ctx.Err() != nil)
	return c.memStore.Destroy(ctx, id)
}

func (c *ctxStore) Touch(ctx context.Context, id string, expiresOn time.Time) error {
	c.canceled = append(c.canceled, ctx.Err() != nil)
	return c.memStore.Touch(ctx, id, expiresOn)
}

// A client that goes away is neither an error to report nor a reason to skip
// the sliding renewal.
func TestClientDisconnectDuringLoad(t *testing.T) {
	canceled := func(r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		*r = *r.WithContext(ctx)
	}

	h := newHarness(t)
	h.login(alice)
	d, _ := h.store.get(h.sessionKey())
	d.ExpiresOn = time.Now().Add(time.Minute) // due for renewal
	store := &ctxStore{memStore: h.store}
	h.mw = middleware(store, func(_ context.Context, err error) {
		t.Errorf("reported %v", err)
	})
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() }, canceled)
	if len(store.canceled) != 1 || store.canceled[0] {
		t.Errorf("Touch contexts canceled = %v, want one live context", store.canceled)
	}

	h.store.loadErr = fmt.Errorf("load: %w", context.Canceled)
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() }, canceled)
}

// A client disconnecting mid-logout must not leave the session alive.
func TestStoreWritesSurviveClientDisconnect(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	store := &ctxStore{memStore: h.store}
	h.mw = middleware(store, h.report)

	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		cancel := s.req.Context().Value(testCtxKey("cancel")).(context.CancelFunc)
		cancel() // the client went away
		_ = s.ImpersonateUser(&bob)
		h.mustSave(s)
		if err := s.Destroy(); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	}, h.withCsrf(), func(r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		*r = *r.WithContext(context.WithValue(ctx, testCtxKey("cancel"), cancel))
	})

	if len(store.canceled) == 0 {
		t.Fatal("no store writes")
	}
	for i, c := range store.canceled {
		if c {
			t.Errorf("store write %d got a canceled context", i)
		}
	}
	if n := h.store.count(); n != 0 {
		t.Errorf("sessions = %d after Destroy, want 0", n)
	}
}

func TestUnsavedSetUserKeepsOldSession(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	oldKey := h.sessionKey()

	h.store.writeErr = errors.New("db down")
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		if err := s.SetUser(&bob); err != nil {
			t.Fatalf("SetUser: %v", err)
		}
		if err := s.Save(); err == nil {
			t.Error("Save succeeded with a failing store")
		}
	}, h.withCsrf())
	h.store.writeErr = nil

	if _, ok := h.store.get(oldKey); !ok {
		t.Fatal("a failed login deleted the previous session")
	}
	if user, _, _ := h.currentUser(); user == nil || user.ID != alice.ID {
		t.Errorf("user = %v, want alice still logged in", user)
	}

	// A saved login does replace it.
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.SetUser(&bob)
		h.mustSave(s)
	}, h.withCsrf())
	if _, ok := h.store.get(oldKey); ok {
		t.Error("the previous session survived a saved login")
	}
	if h.store.count() != 1 {
		t.Errorf("stored sessions = %d, want 1", h.store.count())
	}
}

func TestLostCsrfCookieIsResent(t *testing.T) {
	h := newHarness(t)
	h.login(alice)
	token := h.cookies[csrfCookieName].Value
	delete(h.cookies, csrfCookieName)

	// The write fails, but its response carries the cookie again...
	rec := h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		t.Error("handler ran without a CSRF token")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if c, ok := h.cookies[csrfCookieName]; !ok || c.Value != token {
		t.Fatalf("CSRF cookie not resent: %v", c)
	}

	// ...so the retry succeeds.
	rec = h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {}, h.withCsrf())
	if rec.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200", rec.Code)
	}

	// An intact cookie isn't sent again.
	rec = h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("cookies resent needlessly: %v", rec.Result().Cookies())
	}

	// An overwritten one is.
	h.cookies[csrfCookieName].Value = "other-app"
	h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { s.GetUser() })
	if c := h.cookies[csrfCookieName]; c.Value != token {
		t.Errorf("overwritten CSRF cookie not replaced: %q", c.Value)
	}
}

// A WebSocket handshake is a GET, but one a sibling subdomain can send with
// the cookie: it gets the origin check, and no CSRF token, which it can't
// carry.
func TestWebSocketOriginCheck(t *testing.T) {
	h := newHarness(t)
	h.login(alice)

	tests := []struct {
		name string
		opts []requestOpt
		want int
	}{
		{"same-origin", []requestOpt{withHeader("Sec-Fetch-Site", "same-origin")}, http.StatusOK},
		{"no browser headers", nil, http.StatusOK},
		{"sibling subdomain", []requestOpt{withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", "https://evil.example.com")}, http.StatusForbidden},
		{"origin only", []requestOpt{withHeader("Origin", "https://evil.example.com")}, http.StatusForbidden},
	}
	for _, tt := range tests {
		opts := append([]requestOpt{withHeader("Connection", "Upgrade"), withHeader("Upgrade", "websocket")}, tt.opts...)
		var user *testUser
		rec := h.do(http.MethodGet, func(s *Session[testUser], w http.ResponseWriter) { user = s.GetUser() }, opts...)
		if rec.Code != tt.want {
			t.Errorf("%s: code = %d, want %d", tt.name, rec.Code, tt.want)
		}
		if tt.want == http.StatusOK && (user == nil || *user != alice) {
			t.Errorf("%s: user = %v, want alice", tt.name, user)
		}
	}
}
