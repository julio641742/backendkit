package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/julio641742/backendkit/session"
	"github.com/julio641742/backendkit/session/sessiontest"
)

type testUser struct {
	ID    int
	Email string
	Name  string
}

func (u testUser) GetID() any { return u.ID }

// fakeIdP is an OpenID provider that approves every login.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	challenges map[string]string // code -> PKCE challenge
	claims     map[string]any    // ID token claims besides iss, aud, iat, exp
	signWith   *rsa.PrivateKey   // signs with a key not in the JWKS when set
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{
		t:          t,
		key:        key,
		challenges: map[string]string{},
		claims:     map[string]any{"sub": "sub-alice", "email": "alice@example.com", "email_verified": true, "name": "Alice"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(p.key.N.Bytes()), "e": b64(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// authorize plays the user approving the login: it reads the URL Login
// redirected to and returns the query the provider redirects back with.
func (p *fakeIdP) authorize(location string) url.Values {
	p.t.Helper()
	u, err := url.Parse(location)
	q := u.Query()
	if err != nil || !strings.HasPrefix(location, p.srv.URL+"/authorize") || q.Get("code_challenge_method") != "S256" {
		p.t.Fatalf("Login redirected to %q", location)
	}
	code := rand.Text()
	p.mu.Lock()
	p.challenges[code] = q.Get("code_challenge")
	p.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func (p *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = r.ParseForm()
	challenge, ok := p.challenges[r.PostForm.Get("code")]
	delete(p.challenges, r.PostForm.Get("code"))
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || b64(sum[:]) != challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	claims := map[string]any{"iss": p.srv.URL, "aud": "client", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range p.claims {
		claims[k] = v
	}
	writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": p.sign(claims)})
}

func (p *fakeIdP) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signed := b64(header) + "." + b64(payload)
	key := p.key
	if p.signWith != nil {
		key = p.signWith
	}
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

func (p *fakeIdP) set(fn func(p *fakeIdP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// app serves the login routes behind the session middleware and is driven
// like a browser: cookies from one response go with the next request.
type app struct {
	t       *testing.T
	idp     *fakeIdP
	handler http.Handler
	cookies map[string]*http.Cookie
}

// knowsAlice logs in alice@example.com only.
func knowsAlice(ctx context.Context, email, name string) (*testUser, error) {
	if email != "alice@example.com" {
		return nil, fmt.Errorf("%w: no account for %s", ErrDenied, email)
	}
	return &testUser{ID: 1, Email: email, Name: name}, nil
}

func newApp(t *testing.T, auth Authenticate[testUser]) *app {
	t.Helper()
	a := &app{t: t, idp: newFakeIdP(t), cookies: map[string]*http.Cookie{}}
	h, err := New(t.Context(), Config{
		Issuer:       a.idp.srv.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "https://portal.example.com/oauth/callback",
	}, auth)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/login", h.Login)
	mux.HandleFunc("GET /oauth/callback", h.Callback)
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		if u := session.From[testUser](r).GetUser(); u != nil {
			_ = json.NewEncoder(w).Encode(u)
		}
	})
	store := sessiontest.NewStore[testUser]()
	a.handler = session.Middleware(store)(mux)
	return a
}

func (a *app) get(target string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://portal.example.com"+target, nil)
	for _, c := range a.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(a.cookies, c.Name)
		} else {
			a.cookies[c.Name] = c
		}
	}
	return rec
}

// login runs Login and has the provider approve it, returning the callback
// query.
func (a *app) login() url.Values {
	a.t.Helper()
	return a.loginFrom("/oauth/login")
}

// loginFrom is login through target, a Login URL.
func (a *app) loginFrom(target string) url.Values {
	a.t.Helper()
	rec := a.get(target)
	if rec.Code != http.StatusFound {
		a.t.Fatalf("Login: status %d", rec.Code)
	}
	return a.idp.authorize(rec.Header().Get("Location"))
}

// callback runs Callback and returns where it redirected.
func (a *app) callback(q url.Values) string {
	a.t.Helper()
	rec := a.get("/oauth/callback?" + q.Encode())
	if rec.Code != http.StatusSeeOther {
		a.t.Fatalf("Callback: status %d", rec.Code)
	}
	return rec.Header().Get("Location")
}

func (a *app) me() string {
	return strings.TrimSpace(a.get("/me").Body.String())
}

func TestLogin(t *testing.T) {
	a := newApp(t, knowsAlice)
	if loc := a.callback(a.login()); loc != "/" {
		t.Fatalf("redirected to %q, want /", loc)
	}
	if got := a.me(); got != `{"ID":1,"Email":"alice@example.com","Name":"Alice"}` {
		t.Fatalf("logged in user = %s", got)
	}
	if _, ok := a.cookies[cookieName]; ok {
		t.Fatal("the login cookie outlived the callback")
	}
}

// Login's ?return= path is where a successful login lands; anything that
// could leave the host lands on "/".
func TestLoginReturn(t *testing.T) {
	for ret, want := range map[string]string{
		"/session/handoff?host=a.svc&state=s": "/session/handoff?host=a.svc&state=s",
		"//evil.com":                          "/",
		"https://evil.com":                    "/",
		"/\\evil.com":                         "/",
	} {
		a := newApp(t, knowsAlice)
		if loc := a.callback(a.loginFrom("/oauth/login?return=" + url.QueryEscape(ret))); loc != want {
			t.Errorf("return %q: redirected to %q, want %q", ret, loc, want)
		}
	}
}

func TestLoginCookie(t *testing.T) {
	a := newApp(t, knowsAlice)
	a.get("/oauth/login")
	c := a.cookies[cookieName]
	if c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie = %+v", c)
	}
}

func TestFailedLogins(t *testing.T) {
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		auth  Authenticate[testUser]
		idp   func(p *fakeIdP)
		query func(a *app, q url.Values)
		want  string
	}{
		"no cookie":      {query: func(a *app, q url.Values) { clear(a.cookies) }, want: "expired"},
		"other state":    {query: func(a *app, q url.Values) { q.Set("state", rand.Text()) }, want: "expired"},
		"no state":       {query: func(a *app, q url.Values) { q.Del("state") }, want: "expired"},
		"cancelled":      {query: func(a *app, q url.Values) { q.Del("code"); q.Set("error", "access_denied") }, want: "cancelled"},
		"provider error": {query: func(a *app, q url.Values) { q.Del("code"); q.Set("error", "server_error") }, want: "failed"},
		"bad code":       {query: func(a *app, q url.Values) { q.Set("code", "forged") }, want: "failed"},
		"forged token":   {idp: func(p *fakeIdP) { p.signWith = forger }, want: "failed"},
		"no email":       {idp: func(p *fakeIdP) { delete(p.claims, "email") }, want: "failed"},
		"unverified":     {idp: func(p *fakeIdP) { p.claims["email_verified"] = false }, want: "denied"},
		// Without the claim the provider vouches for nothing (Entra's "nOAuth").
		"verification missing": {idp: func(p *fakeIdP) { delete(p.claims, "email_verified") }, want: "denied"},
		"unknown email":        {idp: func(p *fakeIdP) { p.claims["email"] = "mallory@example.com" }, want: "denied"},
		"auth error": {
			auth: func(context.Context, string, string) (*testUser, error) { return nil, errors.New("db down") },
			want: "failed",
		},
		"nil user": {
			auth: func(context.Context, string, string) (*testUser, error) { return nil, nil },
			want: "failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			auth := tc.auth
			if auth == nil {
				auth = knowsAlice
			}
			a := newApp(t, auth)
			if tc.idp != nil {
				a.idp.set(tc.idp)
			}
			q := a.login()
			if tc.query != nil {
				tc.query(a, q)
			}
			if loc := a.callback(q); loc != "/?login_error="+tc.want {
				t.Fatalf("redirected to %q, want login_error=%s", loc, tc.want)
			}
			if a.me() != "" {
				t.Fatal("a failed login logged someone in")
			}
			if _, ok := a.cookies[cookieName]; ok {
				t.Fatal("the login cookie outlived the callback")
			}
		})
	}
}

func TestReplayedCallback(t *testing.T) {
	a := newApp(t, knowsAlice)
	q := a.login()
	a.callback(q)
	a.get("/oauth/login") // a new login is in flight
	if loc := a.callback(q); loc != "/?login_error=expired" {
		t.Fatalf("replay redirected to %q", loc)
	}
}

func TestNewProviderDown(t *testing.T) {
	p := newFakeIdP(t)
	p.srv.Close()
	if _, err := New(t.Context(), Config{Issuer: p.srv.URL, ClientID: "c"}, knowsAlice); err == nil {
		t.Fatal("New succeeded with the provider down")
	}
}
