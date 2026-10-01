// Package oauth logs users in with an OpenID Connect provider and stores them
// in a session.Session.
//
// Login sends the browser to the provider. Callback verifies the ID token,
// hands its email (which the provider must mark email_verified) and name to
// Authenticate, logs in the user it returns, and redirects to the path Login
// got as ?return=, or "/". A failed login lands on "/", with ?login_error=
// set to expired, cancelled, denied or failed, for the SPA to show.
//
//	auth, err := oauth.New(ctx, cfg, func(ctx context.Context, email, name string) (*User, error) {
//		user, err := database.SelectRow[User](ctx, db, selectUserByEmail, email)
//		if database.IsNotFound(err) {
//			return nil, oauth.ErrDenied
//		}
//		return user, err
//	})
//	r.Get("/oauth/login", auth.Login)
//	r.Get("/oauth/callback", auth.Callback) // behind session.Middleware
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/julio641742/backendkit/internal/httpx"
	"github.com/julio641742/backendkit/session"
)

const (
	// cookieName carries "<state>.<PKCE verifier>.<base64 return path>"
	// from Login to Callback, on the portal host only (__Host-).
	cookieName = "__Host-oauth"
	// loginLifetime is how long the user has to get through the provider.
	loginLifetime = 10 * time.Minute
	// providerTimeout bounds the code exchange, key fetch included.
	providerTimeout = 10 * time.Second
)

// ErrDenied refuses a login: return it, or wrap it, from Authenticate for an
// unknown email or an inactive account. The SPA gets login_error=denied.
var ErrDenied = errors.New("oauth: login denied")

var (
	errExpired   = errors.New("oauth: login state missing, expired or mismatched")
	errCancelled = errors.New("oauth: login cancelled at the provider")
)

// Config is the app's registration with the provider.
type Config struct {
	// Issuer is the provider's URL, from which its endpoints and keys are
	// discovered, such as https://dex.example.com.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the Callback route's absolute URL, exactly as
	// registered with the provider.
	RedirectURL string
}

// Authenticate returns the user the email belongs to, or an error wrapping
// ErrDenied. Any other error is a failed login, logged. name may be empty.
type Authenticate[T session.User] func(ctx context.Context, email, name string) (*T, error)

// Handler serves the Login and Callback routes. It is safe for concurrent use.
type Handler[T session.User] struct {
	oauth2   oauth2.Config
	verifier *oidc.IDTokenVerifier
	auth     Authenticate[T]
}

// New discovers the provider's endpoints from cfg.Issuer, so it fails when
// the provider is unreachable; ctx bounds that request.
func New[T session.User](ctx context.Context, cfg Config, auth Authenticate[T]) (*Handler[T], error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth: discovering %q: %w", cfg.Issuer, err)
	}
	return &Handler[T]{
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		auth:     auth,
	}, nil
}

// Login redirects to the provider. Callback returns to the ?return= path, a
// path on this host such as session.Handoff's, or to "/" without one.
// Starting another login before finishing this one replaces it.
func (h *Handler[T]) Login(w http.ResponseWriter, r *http.Request) {
	ret := r.URL.Query().Get("return")
	if !httpx.IsLocalPath(ret) {
		ret = "/"
	}
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	setCookie(w, state+"."+verifier+"."+base64.RawURLEncoding.EncodeToString([]byte(ret)), int(loginLifetime.Seconds()))
	http.Redirect(w, r, h.oauth2.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// Callback finishes the login and redirects to Login's return path, or to
// "/?login_error=..." when it failed. It must run behind
// session.Middleware[T].
func (h *Handler[T]) Callback(w http.ResponseWriter, r *http.Request) {
	ret, err := h.callback(w, r)
	if err != nil {
		slog.WarnContext(r.Context(), "oauth: login failed", "err", err)
		http.Redirect(w, r, "/?login_error="+errorCode(err), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

// callback logs the user in and returns the path to go on to.
func (h *Handler[T]) callback(w http.ResponseWriter, r *http.Request) (string, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", errExpired
	}
	setCookie(w, "", -1) // one use only, whatever happens next

	q := r.URL.Query()
	parts := strings.SplitN(c.Value, ".", 3)
	if len(parts) != 3 || parts[0] == "" || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(q.Get("state"))) != 1 {
		return "", errExpired
	}
	verifier := parts[1]
	ret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !httpx.IsLocalPath(string(ret)) {
		return "", errExpired
	}
	if e := q.Get("error"); e != "" {
		if e == "access_denied" {
			return "", errCancelled
		}
		return "", fmt.Errorf("oauth: provider error %s: %s", e, q.Get("error_description"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	token, err := h.oauth2.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		return "", fmt.Errorf("oauth: exchanging the code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return "", errors.New("oauth: no id_token in the token response")
	}
	// Checks the signature, issuer, audience and expiry.
	idToken, err := h.verifier.Verify(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("oauth: %w", err)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", fmt.Errorf("oauth: decoding the ID token: %w", err)
	}
	if claims.Email == "" {
		return "", errors.New("oauth: the ID token has no email claim")
	}
	// The email is what logs the user in, so it must be verified; a missing
	// claim counts as unverified (some providers hand out any email).
	if !claims.EmailVerified {
		return "", fmt.Errorf("%w: email not verified", ErrDenied)
	}

	user, err := h.auth(r.Context(), claims.Email, claims.Name)
	if err != nil {
		return "", err
	}
	// SetUser starts a new session id (no fixation) and refuses a nil user.
	s := session.From[T](r)
	if err := s.SetUser(user); err != nil {
		return "", err
	}
	return string(ret), s.Save()
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, errExpired):
		return "expired"
	case errors.Is(err, errCancelled):
		return "cancelled"
	case errors.Is(err, ErrDenied):
		return "denied"
	}
	return "failed"
}

func setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		// Lax: the provider's redirect back is a cross-site top-level GET,
		// which Strict would send without the cookie.
		SameSite: http.SameSiteLaxMode,
	})
}
