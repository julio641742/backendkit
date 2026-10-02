package session

import (
	"context"
	"maps"
	"sync"
	"time"
)

type testUser struct {
	ID    int64
	Email string
}

func (u testUser) GetID() any { return u.ID }

var _ Store[testUser] = (*memStore)(nil)

// memStore is an in-memory Store with error injection and a write counter.
// sessiontest.Store can't be used here: it imports this package.
type memStore struct {
	mu       sync.Mutex
	sessions map[string]*Data[testUser]
	codes    map[string]code
	services map[string]code // redeemed codes, keyed by service id

	// loadErr / writeErr make every Load / write fail.
	loadErr  error
	writeErr error
	writes   int

	// loadHook, when set, runs after Load has read the session.
	loadHook func()
}

func newMemStore() *memStore {
	return &memStore{sessions: map[string]*Data[testUser]{}, codes: map[string]code{}, services: map[string]code{}}
}

// get returns the stored session itself, so tests can move its times.
func (m *memStore) get(id string) (*Data[testUser], bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.sessions[id]
	return d, ok
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *memStore) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *memStore) Load(_ context.Context, id string) (*Data[testUser], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	d, ok := m.sessions[id]
	if !ok || !d.ExpiresOn.After(time.Now()) {
		return nil, ErrNotFound
	}
	c := cloneData(d)
	if hook := m.loadHook; hook != nil {
		m.mu.Unlock()
		defer m.mu.Lock()
		hook()
	}
	return c, nil
}

func (m *memStore) Insert(_ context.Context, data *Data[testUser]) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	m.sessions[data.ID] = cloneData(data)
	return nil
}

func (m *memStore) Touch(_ context.Context, id string, expiresOn time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	d, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	d.ExpiresOn = expiresOn
	return nil
}

func (m *memStore) Destroy(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	delete(m.sessions, id)
	for _, c := range []map[string]code{m.codes, m.services} {
		maps.DeleteFunc(c, func(_ string, c code) bool { return c.sessionID == id })
	}
	return nil
}

func (m *memStore) InsertCode(_ context.Context, codeID, sessionID, host string, expiresOn time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	m.codes[codeID] = code{sessionID: sessionID, host: host, expiresOn: expiresOn}
	return nil
}

func (m *memStore) RedeemCode(_ context.Context, codeID, host, serviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	c, ok := m.codes[codeID]
	if !ok || c.host != host || !c.expiresOn.After(time.Now()) {
		return ErrNotFound
	}
	delete(m.codes, codeID)
	m.services[serviceID] = c
	return nil
}

func (m *memStore) LoadService(_ context.Context, serviceID, host string) (*Data[testUser], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	c, ok := m.services[serviceID]
	if !ok || c.host != host {
		return nil, ErrNotFound
	}
	d, ok := m.sessions[c.sessionID]
	if !ok || !d.ExpiresOn.After(time.Now()) {
		return nil, ErrNotFound
	}
	return cloneData(d), nil
}

// code is a one-time code, or once redeemed a service session.
type code struct {
	sessionID, host string
	expiresOn       time.Time
}

// cloneData copies d, so callers never share the stored users.
func cloneData(d *Data[testUser]) *Data[testUser] {
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
