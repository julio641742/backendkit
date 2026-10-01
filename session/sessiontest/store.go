// Package sessiontest provides an in-memory session.Store for tests.
//
//	store := sessiontest.NewStore[User]()
//	h := session.Middleware[User](store)(app)
package sessiontest

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/julio641742/backendkit/session"
)

var _ session.Store[session.User] = (*Store[session.User])(nil)

// Store keeps sessions in maps. It follows the session.Store contract like
// pgstore does: expired sessions and codes are not found, inserting an id
// that is already stored fails, touching a deleted or expired session fails
// with session.ErrNotFound, and destroying a session destroys its service
// sessions and codes. Safe for concurrent use.
//
// Unlike pgstore, Load returns the users as they were saved rather than
// reloading them, so a stale or deleted user goes unnoticed.
type Store[T session.User] struct {
	mu       sync.Mutex
	sessions map[string]*session.Data[T]
	codes    map[string]code
	services map[string]code // redeemed codes, keyed by service id
}

func NewStore[T session.User]() *Store[T] {
	return &Store[T]{
		sessions: map[string]*session.Data[T]{},
		codes:    map[string]code{},
		services: map[string]code{},
	}
}

func (s *Store[T]) Load(_ context.Context, id string) (*session.Data[T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.live(id)
	if !ok {
		return nil, session.ErrNotFound
	}
	return clone(d), nil
}

func (s *Store[T]) Insert(_ context.Context, data *session.Data[T]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Like the SQL INSERT, which fails on the primary key.
	if _, ok := s.sessions[data.ID]; ok {
		return fmt.Errorf("sessiontest: insert session: id %q already exists", data.ID)
	}
	s.sessions[data.ID] = clone(data)
	return nil
}

func (s *Store[T]) Touch(_ context.Context, id string, expiresOn time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.live(id)
	if !ok {
		return session.ErrNotFound
	}
	d.ExpiresOn = expiresOn
	return nil
}

// live returns the stored session unless it is missing or expired, as the
// pgstore queries match only expires_on > now(). s.mu must be held.
func (s *Store[T]) live(id string) (*session.Data[T], bool) {
	d, ok := s.sessions[id]
	if !ok || !d.ExpiresOn.After(time.Now()) {
		return nil, false
	}
	return d, true
}

func (s *Store[T]) Destroy(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	for _, m := range []map[string]code{s.codes, s.services} {
		maps.DeleteFunc(m, func(_ string, c code) bool { return c.sessionID == id })
	}
	return nil
}

func (s *Store[T]) InsertCode(_ context.Context, codeID, sessionID, host string, expiresOn time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[codeID] = code{sessionID: sessionID, host: host, expiresOn: expiresOn}
	return nil
}

func (s *Store[T]) RedeemCode(_ context.Context, codeID, host, serviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[codeID]
	if !ok || c.host != host || !c.expiresOn.After(time.Now()) {
		return session.ErrNotFound
	}
	delete(s.codes, codeID)
	s.services[serviceID] = c
	return nil
}

func (s *Store[T]) LoadService(_ context.Context, serviceID, host string) (*session.Data[T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.services[serviceID]
	if !ok || c.host != host {
		return nil, session.ErrNotFound
	}
	d, ok := s.sessions[c.sessionID]
	if !ok || !d.ExpiresOn.After(time.Now()) {
		return nil, session.ErrNotFound
	}
	return clone(d), nil
}

// code is a one-time code, or once redeemed a service session.
type code struct {
	sessionID, host string
	expiresOn       time.Time
}

// clone copies d, so the store never shares the users with its callers.
func clone[T session.User](d *session.Data[T]) *session.Data[T] {
	c := *d
	if d.User != nil {
		u := *d.User
		c.User = &u
	}
	if d.ImpersonatedUser != nil {
		u := *d.ImpersonatedUser
		c.ImpersonatedUser = &u
	}
	return &c
}
