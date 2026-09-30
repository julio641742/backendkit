package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned by Store.Load for a missing or expired session,
	// or one whose user no longer exists.
	ErrNotFound = errors.New("session: not found")

	// ErrNoUser is returned by SetUser and ImpersonateUser when given a nil
	// user, and by ImpersonateUser without a logged in user. Call Destroy to
	// log out and StopImpersonatingUser to stop impersonating.
	ErrNoUser = errors.New("session: no user")

	// ErrResponseStarted is returned by Save and Destroy once the response
	// headers have been written, since the cookies can no longer be sent.
	ErrResponseStarted = errors.New("session: response already started, cookies can't be set")

	// ErrExpired is returned by Save when the session reached its absolute
	// lifetime. The session is destroyed; log the user in again.
	ErrExpired = errors.New("session: session expired")

	// ErrUnsaved is passed to the error handler when a handler changed the
	// session (SetUser, ImpersonateUser...) and returned without calling Save
	// or Destroy, so the changes were lost.
	ErrUnsaved = errors.New("session: changes were never saved")
)

// Data is what a Store persists for one session.
type Data[T User] struct {
	// ID is the hex SHA-256 of the session id. The raw id only lives in the
	// cookie, so a leaked store can't be used to hijack sessions.
	ID string

	User             *T
	ImpersonatedUser *T

	CreatedOn time.Time
	ExpiresOn time.Time
}

// Store persists portal sessions and the service sessions that hang off
// them. pgstore.Store is the Postgres implementation and
// sessiontest.Store an in-memory one for tests.
type Store[T User] interface {
	// Load returns the session with this ID, or an error wrapping ErrNotFound.
	Load(ctx context.Context, id string) (*Data[T], error)
	// Insert stores a new session. Sessions are never updated: every change
	// is a privilege change that moves the session to a new ID. It must not
	// modify data.
	Insert(ctx context.Context, data *Data[T]) error
	// Touch moves the expiry of an existing session without rewriting it. A
	// missing session returns an error wrapping ErrNotFound.
	Touch(ctx context.Context, id string, expiresOn time.Time) error
	// Destroy deletes the session, and the service sessions and codes made
	// from it. A missing session is not an error.
	Destroy(ctx context.Context, id string) error

	// InsertCode stores the one-time code codeID, which logs a visitor of
	// host into the session sessionID until expiresOn.
	InsertCode(ctx context.Context, codeID, sessionID, host string, expiresOn time.Time) error
	// RedeemCode turns the unexpired code codeID, minted for host, into the
	// service session serviceID, in one step so a code works only once. A
	// missing, expired, redeemed or other host's code returns an error
	// wrapping ErrNotFound.
	RedeemCode(ctx context.Context, codeID, host, serviceID string) error
	// LoadService returns the session the service session serviceID on host
	// belongs to, as Load does, or an error wrapping ErrNotFound.
	LoadService(ctx context.Context, serviceID, host string) (*Data[T], error)
}

// hashID turns the raw session id from the cookie into the Store ID.
func hashID(rawID string) string {
	sum := sha256.Sum256([]byte(rawID))
	return hex.EncodeToString(sum[:])
}
