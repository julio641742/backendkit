// Package session provides cookie sessions backed by a Store, with CSRF
// protection, sliding and absolute expiry, user impersonation, and logins
// handed off to service hosts on another site.
//
// A session always belongs to a user; see Session. Every cookie is
// host-only (__Host-), Secure (use localtls in development) and
// SameSite=Lax. On the portal, install Middleware, protect everything but the
// public routes with RequireUser, log the user in from the OAuth callback
// with SetUser and Save, and mount Handoff:
//
//	store := pgstore.New[User](db, "SELECT id, email FROM users WHERE id = $1")
//	r.Use(session.Middleware(store))
//	r.Get("/oauth/callback", callback) // s.SetUser(user); s.Save()
//	r.Get(session.HandoffPath, session.Handoff(svc, allow))
//	r.With(session.RequireUser[User]).Mount("/api", api)
//
// On the service hosts, install ServiceMiddleware and read the user with
// ServiceUser; see ServiceConfig for how the two meet:
//
//	svcRouter.Use(session.ServiceMiddleware(store, svc))
package session
