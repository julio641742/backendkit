package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"
)

const svcHost = "qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net"

// svcID is what svcHost's label encodes.
var svcID = uuid.MustParse("85fedf4b-5148-47b3-a996-ae5b9dc3c5f5")

var svcConfig = ServiceConfig{
	PortalOrigin: "https://portal.example.com",
	LoginPath:    "/oauth/login",
	Suffix:       "svc.example.net",
}

// svcHarness is a browser using the portal (the embedded harness) and the
// service hosts, which keep their own cookies, as host-only cookies are.
type svcHarness struct {
	*harness
	mw      func(http.Handler) http.Handler
	cookies map[string]map[string]*http.Cookie // by host
	allow   func(r *http.Request, user *testUser, id uuid.UUID) (bool, error)
}

func newSvcHarness(t *testing.T) *svcHarness {
	h := newHarness(t)
	return &svcHarness{
		harness: h,
		mw:      serviceMiddleware(h.store, svcConfig, h.report),
		cookies: map[string]map[string]*http.Cookie{},
		allow: func(_ *http.Request, _ *testUser, id uuid.UUID) (bool, error) {
			return id == svcID, nil
		},
	}
}

// get requests target on a service host, as a page navigation unless
// opts say otherwise, and reports the service user the app saw.
func (h *svcHarness) get(target string, opts ...requestOpt) (*httptest.ResponseRecorder, *testUser) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	for _, opt := range opts {
		opt(req)
	}
	jar := h.cookies[req.Host]
	for _, c := range jar {
		req.AddCookie(c)
	}

	var user *testUser
	rec := httptest.NewRecorder()
	h.mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = ServiceUser[testUser](r)
	})).ServeHTTP(rec, req)

	if jar == nil {
		jar = map[string]*http.Cookie{}
		h.cookies[req.Host] = jar
	}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(jar, c.Name)
		} else {
			jar[c.Name] = c
		}
	}
	return rec, user
}

// handoff requests target on the portal's Handoff route.
func (h *svcHarness) handoff(target string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.serve(httptest.NewRequest(http.MethodGet, target, nil), Handoff(svcConfig, h.allow))
}

// login goes through the whole flow for path on svcHost and returns the
// final response of the service host.
func (h *svcHarness) login(path string) (*httptest.ResponseRecorder, *testUser) {
	h.t.Helper()
	rec, _ := h.get("https://" + svcHost + path)
	toPortal := location(h.t, rec)
	rec = h.handoff(toPortal)
	toRedeem := location(h.t, rec)
	rec, _ = h.get(toRedeem)
	return h.get("https://" + svcHost + location(h.t, rec))
}

// stateCookie returns the service login state cookie in jar, or nil.
func stateCookie(jar map[string]*http.Cookie) *http.Cookie {
	for name, c := range jar {
		if strings.HasPrefix(name, stateCookiePrefix) {
			return c
		}
	}
	return nil
}

func location(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want 303; body %s", rec.Code, rec.Body)
	}
	return rec.Header().Get("Location")
}

func TestServiceLogin(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)

	// The service host sends the visitor to the portal with a fresh state.
	rec, _ := h.get("https://" + svcHost + "/app/page?x=1")
	toPortal, err := url.Parse(location(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	q := toPortal.Query()
	if toPortal.Scheme+"://"+toPortal.Host != svcConfig.PortalOrigin || toPortal.Path != HandoffPath ||
		q.Get("host") != svcHost || q.Get("return") != "/app/page?x=1" {
		t.Fatalf("sent to %s", toPortal)
	}
	if state := stateCookie(h.cookies[svcHost]); state == nil || state.Name != stateCookiePrefix+q.Get("state") || !state.HttpOnly {
		t.Fatalf("state cookie = %+v, want the state sent to the portal", state)
	}

	// The portal sends a code back, never through a Referer.
	rec = h.handoff(toPortal.String())
	toRedeem, err := url.Parse(location(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	if toRedeem.Host != svcHost || toRedeem.Path != RedeemPath || toRedeem.Query().Get("code") == "" {
		t.Fatalf("sent to %s", toRedeem)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("handoff without Referrer-Policy: no-referrer")
	}

	// The service host swaps it for a host-only session cookie and goes on
	// to the page first asked for.
	rec, _ = h.get(toRedeem.String())
	if got := location(t, rec); got != "/app/page?x=1" {
		t.Errorf("redeemed to %q", got)
	}
	jar := h.cookies[svcHost]
	if c := jar[serviceCookieName]; c == nil || !c.HttpOnly || !c.Secure || c.Domain != "" {
		t.Fatalf("service cookie = %+v", c)
	}
	if stateCookie(jar) != nil {
		t.Error("state cookie outlived the redeem")
	}

	_, user := h.get("https://" + svcHost + "/app/page")
	if user == nil || *user != alice {
		t.Errorf("service user = %v, want alice", user)
	}
	if errs := h.handledErrors(); len(errs) != 0 {
		t.Errorf("errors = %v", errs)
	}
}

func TestServiceCodeWorksOnce(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	rec, _ := h.get("https://" + svcHost + "/")
	toRedeem := location(t, h.handoff(location(t, rec)))
	state := *stateCookie(h.cookies[svcHost])

	h.get(toRedeem)
	delete(h.cookies[svcHost], serviceCookieName)
	h.cookies[svcHost][state.Name] = &state
	if rec, _ := h.get(toRedeem); rec.Code != http.StatusForbidden {
		t.Errorf("replayed code: code = %d, want 403", rec.Code)
	}
}

// An attacker's own code, sent to a victim, must not log the victim in as
// the attacker: the victim's browser has no matching state.
func TestServiceRedeemNeedsState(t *testing.T) {
	attacker := newSvcHarness(t)
	attacker.harness.login(bob)
	rec, _ := attacker.get("https://" + svcHost + "/")
	toRedeem := location(t, attacker.handoff(location(t, rec)))

	victim := &svcHarness{harness: attacker.harness, mw: attacker.mw, cookies: map[string]map[string]*http.Cookie{}}
	if rec, user := victim.get(toRedeem); rec.Code != http.StatusForbidden || user != nil {
		t.Errorf("redeem without state: code = %d user = %v, want 403", rec.Code, user)
	}
	// A state cookie of the victim's own doesn't match either.
	victim.get("https://" + svcHost + "/")
	if rec, _ := victim.get(toRedeem); rec.Code != http.StatusForbidden {
		t.Errorf("redeem with another state: code = %d, want 403", rec.Code)
	}
}

func TestServiceCodeIsBoundToHost(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	rec, _ := h.get("https://" + svcHost + "/")
	toRedeem, _ := url.Parse(location(t, h.handoff(location(t, rec))))

	// The same code and state, at another service host.
	toRedeem.Host = "aaaaaaaaaaaaaaaaaaaaaaaaaa.svc.example.net" // the nil UUID
	h.cookies[toRedeem.Host] = h.cookies[svcHost]
	if rec, _ := h.get(toRedeem.String()); rec.Code != http.StatusForbidden {
		t.Errorf("code redeemed at another host: code = %d, want 403", rec.Code)
	}
}

func TestServiceCodeExpires(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	rec, _ := h.get("https://" + svcHost + "/")
	toRedeem := location(t, h.handoff(location(t, rec)))
	for id, c := range h.store.codes {
		c.expiresOn = time.Now().Add(-time.Second)
		h.store.codes[id] = c
	}
	if rec, _ := h.get(toRedeem); rec.Code != http.StatusForbidden {
		t.Errorf("expired code: code = %d, want 403", rec.Code)
	}
}

func TestServiceSessionFollowsPortalSession(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	h.login("/")

	// Impersonating moves the portal session, logging the service host out;
	// the next handoff brings the new user.
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) {
		_ = s.ImpersonateUser(&bob)
		h.mustSave(s)
	}, h.withCsrf())
	if rec, user := h.get("https://"+svcHost+"/", withHeader("Sec-Fetch-Mode", "cors")); rec.Code != http.StatusUnauthorized || user != nil {
		t.Fatalf("after impersonating: code = %d user = %v, want 401", rec.Code, user)
	}
	if _, user := h.login("/"); user == nil || *user != bob {
		t.Fatalf("service user = %v, want the impersonated bob", user)
	}

	// Logging out of the portal logs out of the service hosts.
	h.do(http.MethodPost, func(s *Session[testUser], w http.ResponseWriter) { _ = s.Destroy() }, h.withCsrf())
	if rec, _ := h.get("https://"+svcHost+"/", withHeader("Sec-Fetch-Mode", "cors")); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: code = %d, want 401", rec.Code)
	}
}

func TestServiceRequestRenewsPortalSession(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	h.login("/")
	key := h.sessionKey()
	h.store.sessions[key].ExpiresOn = time.Now().Add(time.Hour)

	h.get("https://" + svcHost + "/")
	if d := time.Until(h.store.sessions[key].ExpiresOn); d < 23*time.Hour {
		t.Errorf("portal session expires in %v, want renewed", d)
	}

	// An expired portal session is not served, nor renewed.
	h.store.sessions[key].ExpiresOn = time.Now().Add(-time.Second)
	if _, user := h.get("https://" + svcHost + "/"); user != nil {
		t.Errorf("served %v from an expired session", user)
	}
}

func TestServiceMiddlewareRejects(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	h.login("/")

	tests := []struct {
		name   string
		target string
		opts   []requestOpt
		want   int
	}{
		{"not a service host", "https://portal.example.com/", nil, http.StatusNotFound},
		{"nested label", "https://a.qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net/", nil, http.StatusNotFound},
		{"sibling host write", "https://" + svcHost + "/", []requestOpt{
			func(r *http.Request) { r.Method = http.MethodPost },
			withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", "https://evil.svc.example.net"),
		}, http.StatusForbidden},
		{"sibling host websocket", "https://" + svcHost + "/", []requestOpt{
			withHeader("Upgrade", "websocket"), withHeader("Connection", "Upgrade"),
			withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", "https://evil.svc.example.net"),
		}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec, user := h.get(tt.target, tt.opts...); rec.Code != tt.want || user != nil {
				t.Errorf("code = %d user = %v, want %d", rec.Code, user, tt.want)
			}
		})
	}
}

// Requests that can't follow a redirect to the portal, such as the app's
// own fetches, get a 401 instead.
func TestServiceWithoutSession(t *testing.T) {
	h := newSvcHarness(t)
	for _, opts := range [][]requestOpt{
		{withHeader("Sec-Fetch-Mode", "cors")},
		{func(r *http.Request) { r.Method = http.MethodPost }, withHeader("Sec-Fetch-Site", "same-origin")},
		{withHeader("Upgrade", "websocket"), withHeader("Sec-Fetch-Site", "same-origin")},
	} {
		if rec, _ := h.get("https://"+svcHost+"/api", opts...); rec.Code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", rec.Code)
		}
	}
	if stateCookie(h.cookies[svcHost]) != nil {
		t.Error("state cookie set without a redirect")
	}
}

func TestHandoff(t *testing.T) {
	boom := errors.New("db down")
	tests := []struct {
		name   string
		login  bool
		target string
		allow  func(*http.Request, *testUser, uuid.UUID) (bool, error)
		want   int
	}{
		{"not allowed", true, "/session/handoff?host=qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net&state=s&return=/", func(*http.Request, *testUser, uuid.UUID) (bool, error) { return false, nil }, http.StatusNotFound},
		{"allow fails", true, "/session/handoff?host=qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net&state=s&return=/", func(*http.Request, *testUser, uuid.UUID) (bool, error) { return false, boom }, http.StatusInternalServerError},
		{"other site", true, "/session/handoff?host=evil.com&state=s&return=/", nil, http.StatusBadRequest},
		{"portal host", true, "/session/handoff?host=svc.example.net&state=s&return=/", nil, http.StatusBadRequest},
		{"no state", true, "/session/handoff?host=qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net&return=/", nil, http.StatusBadRequest},
		{"absolute return", true, "/session/handoff?host=qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net&state=s&return=https://evil.com", nil, http.StatusBadRequest},
		{"logged out", false, "/session/handoff?host=qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net&state=s&return=/", nil, http.StatusSeeOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSvcHarness(t)
			if tt.login {
				h.harness.login(alice)
			}
			if tt.allow != nil {
				h.allow = tt.allow
			}
			rec := h.handoff(tt.target)
			if rec.Code != tt.want {
				t.Fatalf("code = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusSeeOther {
				if got, want := rec.Header().Get("Location"), "/oauth/login?return="+url.QueryEscape(tt.target); got != want {
					t.Errorf("sent to %q, want %q", got, want)
				}
			}
			if len(h.store.codes) != 0 {
				t.Error("code minted for a refused handoff")
			}
			if tt.name == "allow fails" {
				if errs := h.handledErrors(); len(errs) != 1 || !errors.Is(errs[0], boom) {
					t.Errorf("errors = %v, want %v", errs, boom)
				}
			}
		})
	}
}

// A service host's port, as in development, is part of Suffix and binds the
// code.
func TestServiceLoginWithPort(t *testing.T) {
	h := newSvcHarness(t)
	cfg := svcConfig
	cfg.Suffix += ":3001"
	h.mw = serviceMiddleware(h.store, cfg, h.report)
	h.harness.login(alice)
	rec, _ := h.get("https://qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net:3001/")
	toRedeem := location(t, h.serve(httptest.NewRequest(http.MethodGet, location(t, rec), nil), Handoff(cfg, h.allow)))
	if !strings.HasPrefix(toRedeem, "https://qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net:3001"+RedeemPath+"?") {
		t.Fatalf("sent to %s", toRedeem)
	}
	h.get(toRedeem)
	if _, user := h.get("https://qx7n6s2rjbd3hkmwvznz3q6f6u.svc.example.net:3001/"); user == nil || *user != alice {
		t.Errorf("service user = %v, want alice", user)
	}
}

func TestServiceConfigServiceID(t *testing.T) {
	tests := []struct {
		host string
		ok   bool
	}{
		{svcHost, true},
		{strings.ToUpper(svcHost), true},
		{svcHost + ":3001", false}, // not Suffix's port
		{"svc.example.net", false},
		{"portal.svc.example.net", false}, // the portal's host in development
		{"a." + svcHost, false},
		{"abc.svc.example.net", false},
		{"qx7n6s2rjbd3hkmwvznz3q6f6usvc.example.net", false},
		{svcHost + ".evil.com", false},
		{svcHost + ":1@evil.com", false},
		{svcHost + ":", false},
		{"[" + svcHost + "]:1", false},
	}
	for _, tt := range tests {
		if id, ok := svcConfig.ServiceID(tt.host); ok != tt.ok || ok && id != svcID {
			t.Errorf("ServiceID(%q) = %v, %v; want ok %v", tt.host, id, ok, tt.ok)
		}
	}
}

// A host whose port hides a userinfo ("<label>.<Suffix>:1@evil.com") made
// Handoff mint a code for an allowed label and redirect it to evil.com.
func TestHandoffRejectsForeignRedirect(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	for _, host := range []string{svcHost + ":1@evil.com", "3232235777/.svc.example.net"} {
		h.allow = func(*http.Request, *testUser, uuid.UUID) (bool, error) { return true, nil }
		q := url.Values{"host": {host}, "state": {"s"}, "return": {"/"}}
		rec := h.handoff(HandoffPath + "?" + q.Encode())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("host %q: code = %d, want 400; Location %q", host, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// A port other than Suffix's would send the code to whatever else listens
// on the service host's IP, where it could be read and redeemed by whoever
// chose the state.
func TestHandoffRejectsOtherPort(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	q := url.Values{"host": {svcHost + ":8443"}, "state": {"s"}, "return": {"/"}}
	if rec := h.handoff(HandoffPath + "?" + q.Encode()); rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400; Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(h.store.codes) != 0 {
		t.Error("code minted for another port")
	}
}

// Two tabs logging in to the same service host at once must both succeed.
func TestServiceParallelLogins(t *testing.T) {
	h := newSvcHarness(t)
	h.harness.login(alice)
	rec1, _ := h.get("https://" + svcHost + "/one")
	rec2, _ := h.get("https://" + svcHost + "/two")
	toRedeem1 := location(t, h.handoff(location(t, rec1)))
	toRedeem2 := location(t, h.handoff(location(t, rec2)))
	for _, toRedeem := range []string{toRedeem1, toRedeem2} {
		delete(h.cookies[svcHost], serviceCookieName)
		if rec, _ := h.get(toRedeem); rec.Code != http.StatusSeeOther {
			t.Errorf("redeem: code = %d, want 303", rec.Code)
		}
	}
}

func TestServiceUser(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	defer func() {
		if recover() == nil {
			t.Error("ServiceUser without the middleware didn't panic")
		}
	}()
	req = req.WithContext(NewServiceContext(context.Background(), &alice))
	if u := ServiceUser[testUser](req); u == nil || *u != alice {
		t.Errorf("ServiceUser = %v, want alice", u)
	}
	ServiceUser[testUser](httptest.NewRequest(http.MethodGet, "/", nil))
}
