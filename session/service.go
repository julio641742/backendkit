package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/julio641742/backendkit/httperr"
	"github.com/julio641742/backendkit/internal/httpx"
	"github.com/julio641742/backendkit/shortuuid"
)

const (
	// HandoffPath is the portal route that logs service hosts in; mount
	// Handoff on it.
	HandoffPath = "/session/handoff"
	// RedeemPath is where a service host takes the code back from the
	// portal. ServiceMiddleware answers it, so it never reaches the app.
	RedeemPath = "/__session/redeem"

	// serviceCookieName holds the raw service session id. __Host- keeps it
	// on its own service host: sibling hosts can't plant one there.
	serviceCookieName = "__Host-svc"
	// stateCookiePrefix, followed by the state, names the cookie that ties a
	// redeemed code to the browser that went to the portal for it, as OAuth's
	// state does: without it, anyone could log a victim into a service host
	// as themselves with their own code. One cookie per state, so tabs
	// logging in at the same time don't overwrite each other's.
	stateCookiePrefix = "__Host-svc-state-"

	codeLifetime  = time.Minute
	stateLifetime = 10 * time.Minute
)

type serviceKey struct{}

// ServiceConfig ties the portal to its service hosts, <shortuuid>.<Suffix>. They
// live on another site than the portal, so the apps they proxy can't reach
// the portal's cookies, and each one logs in with a code from the portal:
//
//  1. A visitor without a service session is sent to the portal's
//     HandoffPath (ServiceMiddleware).
//  2. The portal checks the visitor may use the host and sends back a
//     one-time code (Handoff), logging the visitor in first if needed.
//  3. The service host redeems the code for a host-only service session
//     cookie (ServiceMiddleware).
//
// A service session lives as long as the portal session it came from:
// logging out, or changing who is impersonated, logs out of every service
// host too.
type ServiceConfig struct {
	// PortalOrigin is where the portal is served: "https://portal.domain.com",
	// or "https://portal.i.qip.sh:3000" in development.
	PortalOrigin string
	// LoginPath is the portal route that starts a login and returns to its
	// ?return= path afterwards, such as oauth.Handler.Login's.
	LoginPath string
	// Suffix is the domain the service hosts are under, with the port they
	// are served on when it isn't the default: "svc-domain.net", or
	// "i.qip.sh:3001" in development.
	Suffix string
}

// ServiceID returns the id a <shortuuid>.<Suffix> host encodes. Any other
// host, such as the portal's, reports false. Handoff redirects to the host it
// is given, so nothing but a shortuuid label in front of Suffix gets through:
// "<label>.<Suffix>:1@evil.com" would send the code to evil.com, and another
// port to whatever else listens there.
func (c ServiceConfig) ServiceID(host string) (uuid.UUID, bool) {
	label, ok := strings.CutSuffix(strings.ToLower(host), "."+strings.ToLower(c.Suffix))
	if !ok {
		return uuid.UUID{}, false
	}
	id, err := shortuuid.Decode(label)
	return id, err == nil
}

// Handoff serves HandoffPath on the portal, behind Middleware and outside
// RequireUser: a visitor who isn't logged in is sent to cfg.LoginPath first.
// allow reports whether user may use the service host with this id (see
// ServiceID); a host they may not use, or that doesn't exist, is answered
// with 404, so ids can't be probed.
//
//	r.Get(session.HandoffPath, session.Handoff(svc, func(r *http.Request, user *User, id uuid.UUID) (bool, error) {
//		return database.SelectValue[bool](r.Context(), db, canUseService, id, user.ID)
//	}))
func Handoff[T User](cfg ServiceConfig, allow func(r *http.Request, user *T, id uuid.UUID) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		host, state, ret := strings.ToLower(q.Get("host")), q.Get("state"), q.Get("return")
		id, ok := cfg.ServiceID(host)
		if !ok || state == "" || !httpx.IsLocalPath(ret) {
			httperr.Write(w, http.StatusBadRequest, "invalid service login request")
			return
		}

		s := From[T](r)
		user := s.GetUser()
		if user == nil {
			http.Redirect(w, r, cfg.LoginPath+"?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		allowed, err := allow(r, user, id)
		if err != nil {
			s.report(r.Context(), fmt.Errorf("session: handoff to %s: %w", host, err))
			httperr.Write(w, http.StatusInternalServerError, "")
			return
		}
		if !allowed {
			httperr.Write(w, http.StatusNotFound, "")
			return
		}
		code, err := s.newCode(host)
		if err != nil {
			s.report(r.Context(), fmt.Errorf("session: handoff to %s: %w", host, err))
			httperr.Write(w, http.StatusInternalServerError, "")
			return
		}
		// The code must not reach the service app through a Referer.
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		redeem := url.Values{"code": {code}, "state": {state}, "return": {ret}}
		http.Redirect(w, r, "https://"+host+RedeemPath+"?"+redeem.Encode(), http.StatusSeeOther)
	}
}

// newCode stores a one-time code for host that logs into this session.
func (s *Session[T]) newCode(host string) (string, error) {
	if !s.loaded {
		return "", ErrNoUser // a login that isn't saved yet
	}
	code := rand.Text()
	err := s.store.InsertCode(s.storeCtx, hashID(code), s.data.ID, host, time.Now().Add(codeLifetime))
	return code, err
}

// ServiceMiddleware serves the service hosts, letting through only requests
// with a service session; get its user with ServiceUser. It answers requests
// for a host that isn't <shortuuid>.<cfg.Suffix> with 404, and writes and
// WebSocket handshakes from another origin (such as a sibling service host)
// with 403. Without a service session, a page navigation is sent to the
// portal to get one, and anything else is answered with 401.
//
// Each request also slides the portal session's expiry, so using only
// service hosts keeps the login alive. Errors are logged with slog.
func ServiceMiddleware[T User](store Store[T], cfg ServiceConfig) func(next http.Handler) http.Handler {
	return serviceMiddleware(store, cfg, logError)
}

func serviceMiddleware[T User](store Store[T], cfg ServiceConfig, report func(ctx context.Context, err error)) func(next http.Handler) http.Handler {
	crossOrigin := http.NewCrossOriginProtection()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.ToLower(r.Host)
			if _, ok := cfg.ServiceID(host); !ok {
				httperr.Write(w, http.StatusNotFound, "")
				return
			}
			if r.URL.Path == RedeemPath {
				redeem(w, r, store, host, report)
				return
			}
			if !sameOrigin(crossOrigin, w, r) {
				return
			}

			user := loadServiceUser(r, store, host, report)
			if user == nil {
				askPortal(w, r, cfg, host)
				return
			}
			next.ServeHTTP(w, r.WithContext(NewServiceContext(r.Context(), user)))
		})
	}
}

// ServiceUser returns the user of a request ServiceMiddleware let through:
// the impersonated user while impersonating, as Session.GetUser does. It
// panics when ServiceMiddleware is missing or was built for another user
// type.
func ServiceUser[T User](r *http.Request) *T {
	user, ok := r.Context().Value(serviceKey{}).(*T)
	if !ok {
		panic("session: no service user in the request context, is ServiceMiddleware installed?")
	}
	return user
}

// NewServiceContext returns ctx carrying user, where ServiceUser finds it.
// ServiceMiddleware does this; use it to test service host handlers.
func NewServiceContext[T User](ctx context.Context, user *T) context.Context {
	return context.WithValue(ctx, serviceKey{}, user)
}

// loadServiceUser returns the user of the request's service session, or nil.
func loadServiceUser[T User](r *http.Request, store Store[T], host string, report func(context.Context, error)) *T {
	c, err := r.Cookie(serviceCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	ctx := r.Context()
	data, err := store.LoadService(ctx, hashID(c.Value), host)
	if err != nil {
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, context.Canceled) {
			report(ctx, err)
		}
		return nil
	}
	// Stores filter expired sessions, but the database clock may lag ours.
	if !data.ExpiresOn.After(time.Now()) {
		return nil
	}
	if err := renew(context.WithoutCancel(ctx), store, data); errors.Is(err, ErrNotFound) {
		return nil // deleted since it was loaded: a logout elsewhere
	} else if err != nil {
		report(ctx, err) // the session still holds for this request
	}
	if data.ImpersonatedUser != nil {
		return data.ImpersonatedUser
	}
	return data.User
}

// askPortal sends a page navigation to the portal for a code, and answers
// anything else, which couldn't follow the redirect, with 401.
func askPortal(w http.ResponseWriter, r *http.Request, cfg ServiceConfig, host string) {
	mode := r.Header.Get("Sec-Fetch-Mode")
	if r.Method != http.MethodGet || isWebSocket(r) || (mode != "" && mode != "navigate") {
		httperr.Write(w, http.StatusUnauthorized, "authentication required")
		return
	}
	state := rand.Text()
	setCookie(w, stateCookiePrefix+state, "1", true, int(stateLifetime/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	q := url.Values{"host": {host}, "state": {state}, "return": {r.URL.RequestURI()}}
	http.Redirect(w, r, cfg.PortalOrigin+HandoffPath+"?"+q.Encode(), http.StatusSeeOther)
}

// redeem answers RedeemPath: it checks the state, swaps the code for a
// service session and goes on to the page the visitor first asked for.
func redeem[T User](w http.ResponseWriter, r *http.Request, store Store[T], host string, report func(context.Context, error)) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	q := r.URL.Query()
	stateCookie := stateCookiePrefix + q.Get("state")
	if _, err := r.Cookie(stateCookie); err != nil || r.Method != http.MethodGet || q.Get("state") == "" || q.Get("code") == "" {
		httperr.Write(w, http.StatusForbidden, "service login expired, reload the page")
		return
	}
	setCookie(w, stateCookie, "", true, -1) // one use only

	id := rand.Text()
	if err := store.RedeemCode(context.WithoutCancel(r.Context()), hashID(q.Get("code")), host, hashID(id)); err != nil {
		if !errors.Is(err, ErrNotFound) {
			report(r.Context(), fmt.Errorf("session: redeem: %w", err))
		}
		httperr.Write(w, http.StatusForbidden, "service login expired, reload the page")
		return
	}
	// A browser-session cookie: the portal session it hangs off sets the
	// real expiry.
	setCookie(w, serviceCookieName, id, true, 0)

	ret := q.Get("return")
	if !httpx.IsLocalPath(ret) {
		ret = "/"
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}
