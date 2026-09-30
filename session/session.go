package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/julio641742/backendkit/httperr"
	"github.com/julio641742/backendkit/internal/httpx"
)

const (
	// cookieName's __Host- prefix keeps the cookie on the portal host: it
	// can't carry a Domain, so the service hosts can neither receive nor
	// plant it.
	cookieName = "__Host-session"
	// lifetime is how long a session lives without activity. Sessions are
	// renewed (sliding expiry) once less than half of it remains.
	lifetime = 24 * time.Hour
	// absoluteLifetime caps how long renewals can keep a session alive,
	// counted from login.
	absoluteLifetime = 30 * 24 * time.Hour

	// The CSRF cookie and header names Axios uses by default.
	csrfCookieName = "XSRF-TOKEN"
	csrfHeaderName = "X-XSRF-TOKEN"
)

type ctxKey struct{}

var csrfIgnoreMethods = []string{"GET", "HEAD", "OPTIONS"}

// User is implemented by the user struct T (value receiver). GetID returns the
// id the Store persists; pgstore passes it to pgx as the user id argument.
type User interface {
	GetID() any
}

// Session is the per-request session. A session always belongs to a user:
// there are no anonymous sessions. A request without a valid session cookie
// gets an empty session with no user, which can only be logged in (SetUser)
// or destroyed. Wrap routes that need a user in RequireUser. A Session is not
// safe for concurrent use.
type Session[T User] struct {
	req      *http.Request // for its cookies and context
	w        *httpx.TrackingWriter
	storeCtx context.Context // the request context without its cancellation, for writes
	store    Store[T]
	report   func(ctx context.Context, err error)

	once    sync.Once
	rawID   string // the id in the cookie; data.ID is its hash
	data    *Data[T]
	written bool   // data has changes Save must persist
	unsaved bool   // changed since Save or Destroy was last called
	loaded  bool   // data.ID is in the store
	oldID   string // stored id replaced by rotate or SetUser, deleted by Save or Destroy

	sentCookies bool // a cookie was set on this response
}

// From returns the request's session. It panics when the session middleware
// is missing or was built for another user type.
func From[T User](req *http.Request) *Session[T] {
	s, ok := req.Context().Value(ctxKey{}).(*Session[T])
	if !ok {
		panic("session: no Session[T] in the request context, is Middleware[T] installed?")
	}
	return s
}

// RequireUser answers 401 with the httperr envelope unless the request's
// session has a user, so every route behind it can rely on GetUser being
// non-nil. Install it inside Middleware, around everything but the public
// routes (login, OAuth callback, health checks). It panics, like From, when
// Middleware is missing.
func RequireUser[T User](next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if From[T](r).GetUser() == nil {
			httperr.Write(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware serves the portal: it stores a Session[T] in each request
// context and checks CSRF on writes and WebSocket handshakes: a
// Sec-Fetch-Site/Origin check for every request, plus the X-XSRF-TOKEN
// header for logged in sessions. Both failures are answered with a 403.
//
// The session cookie holds the raw session id: 130 random bits that the Store
// only knows hashed, so it needs no signature. Errors that can't be returned
// to a caller (failed loads, which leave the request without a user, failed
// renewals and ErrUnsaved) are logged with slog.
func Middleware[T User](store Store[T]) func(next http.Handler) http.Handler {
	return middleware(store, logError)
}

func middleware[T User](store Store[T], report func(ctx context.Context, err error)) func(next http.Handler) http.Handler {
	crossOrigin := http.NewCrossOriginProtection()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rw := &httpx.TrackingWriter{ResponseWriter: w}
			session := &Session[T]{
				req: req,
				w:   rw,
				// A client disconnecting mid-request must not cancel a
				// logout or rotation half way; the Store's own timeout
				// still bounds each call.
				storeCtx: context.WithoutCancel(req.Context()),
				store:    store,
				report:   report,
			}

			req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, session))

			// Checked when the headers go out, or after the handler if it
			// wrote nothing.
			rw.BeforeStart = func() { session.setCacheControl(rw.Header()) }
			defer func() {
				if !rw.Started() {
					session.setCacheControl(rw.Header())
				}
			}()

			// Changes a handler made but never saved are lost; say so
			// rather than failing silently.
			defer func() {
				if session.unsaved {
					report(req.Context(), ErrUnsaved)
				}
			}()

			if !sameOrigin(crossOrigin, rw, req) {
				return
			}

			// Only logged in sessions carry a CSRF token. A WebSocket
			// handshake, which can't send the header, is a GET.
			if !slices.Contains(csrfIgnoreMethods, req.Method) {
				session.once.Do(session.loadSessionData)
				if session.loaded && !session.validCsrfToken(req.Header.Get(csrfHeaderName)) {
					httperr.Write(rw, http.StatusForbidden, "CSRF token mismatch")
					return
				}
			}

			next.ServeHTTP(rw, req)
		})
	}
}

// sameOrigin answers a write or WebSocket handshake from another origin with
// a 403 and reports false. SameSite=Lax doesn't cover these: it lets
// same-site requests from sibling hosts through. A WebSocket handshake is a
// GET, which CrossOriginProtection always allows, so it is checked as a
// write.
func sameOrigin(crossOrigin *http.CrossOriginProtection, w http.ResponseWriter, req *http.Request) bool {
	check := req
	if isWebSocket(req) {
		post := *req
		post.Method = http.MethodPost
		check = &post
	}
	if err := crossOrigin.Check(check); err != nil {
		httperr.Write(w, http.StatusForbidden, "cross-origin request rejected")
		return false
	}
	return true
}

func isWebSocket(req *http.Request) bool {
	return req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

// setCacheControl keeps shared caches from storing a response that carries
// the session cookies or was built for a logged in user. Cookies override
// the handler's Cache-Control, since the CSRF cookie can be resent without
// the handler knowing; for a logged in user the handler's own Cache-Control
// is kept.
func (s *Session[T]) setCacheControl(h http.Header) {
	switch {
	case s.sentCookies:
		h.Set("Cache-Control", "no-store")
	case s.data != nil && s.data.User != nil && h.Get("Cache-Control") == "":
		h.Set("Cache-Control", "private, no-store")
	}
}

// reset starts a new, unsaved session.
func (s *Session[T]) reset() {
	s.rawID = rand.Text()
	s.data = &Data[T]{ID: hashID(s.rawID)}
}

func (s *Session[T]) loadSessionData() {
	s.reset()

	cookie, err := s.req.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	rawID := cookie.Value

	data, err := s.store.Load(s.req.Context(), hashID(rawID))
	if err != nil {
		// A missing session is served without a user but its cookies are
		// left alone: it may have just been rotated by a parallel request,
		// whose new cookie an expiring Set-Cookie arriving later would
		// delete. A client that went away is not an error worth reporting.
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, context.Canceled) {
			s.report(s.req.Context(), err)
		}
		return
	}
	// Stores filter expired sessions, but the database clock may lag ours:
	// renew must not bring one back to life.
	if !data.ExpiresOn.After(time.Now()) {
		return
	}

	s.rawID, s.data = rawID, data
	s.loaded = true
	if err := renew(s.storeCtx, s.store, data); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Deleted since Load: a logout in another tab, or a rotation by
			// a parallel request, so the cookies are left alone.
			s.reset()
			s.loaded = false
			return
		}
		s.report(s.req.Context(), err)
	}
	s.resendCSRFCookie()
}

// resendCSRFCookie sends the CSRF cookie again when the request didn't carry
// the current token, e.g. because it was cleared or overwritten by another
// script on the portal. Without it every write would fail until the next
// login.
func (s *Session[T]) resendCSRFCookie() {
	if s.responseStarted() {
		return
	}
	token := s.csrfToken()
	if c, err := s.req.Cookie(csrfCookieName); err == nil && c.Value == token {
		return
	}
	s.setCookie(csrfCookieName, token, false, s.cookieMaxAge(time.Now()))
}

// renew slides data's expiry forward once less than half of the lifetime is
// left, up to the absolute lifetime. The cookies outlive any expiry it can
// set (see cookieMaxAge), so it sends none and can run at any time, also for
// a service host. A session deleted since it was loaded returns an error
// wrapping ErrNotFound.
func renew[T User](ctx context.Context, store Store[T], data *Data[T]) error {
	now := time.Now()
	if data.ExpiresOn.Sub(now) > lifetime/2 {
		return nil
	}
	expiresOn := expiry(data.CreatedOn, now)
	if !expiresOn.After(data.ExpiresOn) {
		return nil // absolute lifetime reached
	}
	if err := store.Touch(ctx, data.ID, expiresOn); err != nil {
		return fmt.Errorf("session: renew: %w", err)
	}
	data.ExpiresOn = expiresOn
	return nil
}

// expiry is one lifetime from now, capped by the absolute lifetime of a
// session created on createdOn.
func expiry(createdOn, now time.Time) time.Time {
	if limit := createdOn.Add(absoluteLifetime); limit.Before(now.Add(lifetime)) {
		return limit
	}
	return now.Add(lifetime)
}

// GetUser returns the impersonated user when impersonating, otherwise the
// logged in user, or nil when nobody is logged in. The pointer is the
// session's own copy: changes to it are seen by the rest of the request but
// never persisted, since Stores only keep the user's id.
func (s *Session[T]) GetUser() *T {
	s.once.Do(s.loadSessionData)
	if s.data.ImpersonatedUser != nil {
		return s.data.ImpersonatedUser
	}

	return s.data.User
}

// GetRealUser always returns the logged in user, even while impersonating.
// The same sharing rules as GetUser apply.
func (s *Session[T]) GetRealUser() *T {
	s.once.Do(s.loadSessionData)
	return s.data.User
}

// SetUser logs the user in. It always starts a fresh session with a new id
// (prevents session fixation). Like rotate, the previous session is deleted
// by Save, so a login that is never saved leaves it untouched. A nil user
// returns ErrNoUser.
func (s *Session[T]) SetUser(user *T) error {
	if user == nil {
		return ErrNoUser
	}
	s.once.Do(s.loadSessionData)

	s.retire()
	s.reset()
	s.data.User = user
	s.markDirty()

	return nil
}

// IsImpersonating reports whether GetUser returns an impersonated user.
func (s *Session[T]) IsImpersonating() bool {
	s.once.Do(s.loadSessionData)
	return s.data.ImpersonatedUser != nil
}

// ImpersonateUser makes GetUser return user until StopImpersonatingUser is
// called. No authorization is done here, callers must check permissions.
// Like logging in, it is a privilege change: Save moves the session to a new
// id and CSRF token.
func (s *Session[T]) ImpersonateUser(user *T) error {
	s.once.Do(s.loadSessionData)
	if user == nil || s.data.User == nil {
		return ErrNoUser
	}

	s.data.ImpersonatedUser = user
	s.rotate()
	return nil
}

// StopImpersonatingUser goes back to the logged in user. Save moves the
// session to a new id and CSRF token, as for ImpersonateUser.
func (s *Session[T]) StopImpersonatingUser() {
	s.once.Do(s.loadSessionData)
	if s.data.ImpersonatedUser == nil {
		return
	}
	s.data.ImpersonatedUser = nil
	s.rotate()
}

// rotate gives the session a new id, and so a new CSRF token, keeping its
// users and creation time. The stored session is replaced on Save rather than
// here, so an unsaved rotation leaves the old session untouched.
func (s *Session[T]) rotate() {
	s.retire()
	s.rawID = rand.Text()
	s.data.ID = hashID(s.rawID)
	s.markDirty()
}

// retire marks the stored session for deletion by Save or Destroy. At most
// one id is ever pending: once retired, the session is not loaded, and the id
// replacing it is only stored by Save, which clears oldID.
func (s *Session[T]) retire() {
	if s.loaded {
		s.oldID = s.data.ID
		s.loaded = false
	}
}

func (s *Session[T]) markDirty() {
	s.written = true
	s.unsaved = true
}

// Save persists the session and (re)sets the cookies. Call it before writing
// the response; afterwards it returns ErrResponseStarted and persists nothing.
// A session past its absolute lifetime is destroyed and ErrExpired returned.
// The middleware reports changes that were never saved as ErrUnsaved.
func (s *Session[T]) Save() error {
	s.once.Do(s.loadSessionData)
	s.unsaved = false
	if !s.written {
		return nil
	}
	if s.responseStarted() {
		return ErrResponseStarted
	}

	now := time.Now()
	if s.data.CreatedOn.IsZero() { // kept by rotate
		s.data.CreatedOn = now
	}
	expiresOn := expiry(s.data.CreatedOn, now)
	if !expiresOn.After(now) {
		if err := s.Destroy(); err != nil {
			return err
		}
		return ErrExpired
	}

	s.data.ExpiresOn = expiresOn

	// A rotated or replaced session drops its old id first: a failed save must not leave
	// the pre-rotation session usable.
	if s.oldID != "" {
		if err := s.store.Destroy(s.storeCtx, s.oldID); err != nil {
			return err
		}
		s.oldID = ""
	}

	if err := s.store.Insert(s.storeCtx, s.data); err != nil {
		return err
	}
	s.loaded = true
	s.setCookies(now)
	s.written = false

	return nil
}

// Destroy logs the user out. The stored session is deleted even after the
// response has started, but the cookies can then no longer be cleared and
// ErrResponseStarted is returned.
func (s *Session[T]) Destroy() error {
	s.once.Do(s.loadSessionData)
	s.unsaved = false
	if err := s.destroyStored(); err != nil {
		return err
	}

	s.reset()
	s.written = false
	if s.responseStarted() {
		return ErrResponseStarted
	}
	s.expireCookies()

	return nil
}

// destroyStored deletes the stored session, and the one it was rotated from.
func (s *Session[T]) destroyStored() error {
	if s.oldID != "" {
		if err := s.store.Destroy(s.storeCtx, s.oldID); err != nil {
			return err
		}
		s.oldID = ""
	}
	if s.loaded {
		if err := s.store.Destroy(s.storeCtx, s.data.ID); err != nil {
			return err
		}
		s.loaded = false
	}
	return nil
}

func (s *Session[T]) expireCookies() {
	s.setCookie(cookieName, "", true, -1)
	s.setCookie(csrfCookieName, "", false, -1)
}

func (s *Session[T]) responseStarted() bool {
	return s.w.Started()
}

// setCookies sends the session and CSRF cookies.
func (s *Session[T]) setCookies(now time.Time) {
	maxAge := s.cookieMaxAge(now)
	s.setCookie(cookieName, s.rawID, true, maxAge)
	s.setCookie(csrfCookieName, s.csrfToken(), false, maxAge)
}

// cookieMaxAge keeps the cookies until the session's absolute lifetime
// ends. The Store enforces the sliding expiry, so renewals never need to
// resend them.
func (s *Session[T]) cookieMaxAge(now time.Time) int {
	return int(s.data.CreatedOn.Add(absoluteLifetime).Sub(now) / time.Second)
}

func (s *Session[T]) setCookie(name, value string, httpOnly bool, maxAge int) {
	s.sentCookies = true
	setCookie(s.w, name, value, httpOnly, maxAge)
}

// setCookie sets a host-only cookie, which every cookie of this package is.
// A maxAge of 0 makes a browser-session cookie.
func setCookie(w http.ResponseWriter, name, value string, httpOnly bool, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: httpOnly,
		// Lax, not Strict: the cookie set by the OAuth callback must be
		// sent on the redirect that follows it, and on links into the app.
		SameSite: http.SameSiteLaxMode,
	})
}

// logError is the error handler outside tests.
func logError(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "session: error", "err", err)
}

// csrfToken is derived from the raw session id, so it changes whenever the
// session is rotated (login, impersonation) and needs no storage of its own.
// It can't be computed from the Store, which only has the id's other hash,
// nor can the id be recovered from it.
func (s *Session[T]) csrfToken() string {
	sum := sha256.Sum256([]byte("csrf|" + s.rawID))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Session[T]) validCsrfToken(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrfToken())) == 1
}
