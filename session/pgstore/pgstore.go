// Package pgstore is the Postgres session.Store, keeping sessions in the
// http_sessions and http_service_sessions tables of schema.sql: copy it into
// your migrations and match the user_id types and foreign keys to your user
// table.
//
// Expired sessions and codes are never loaded, but their rows stay until
// PurgeExpired deletes them: call it from a ticker.
package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/julio641742/backendkit/database"
	"github.com/julio641742/backendkit/session"
)

// timeout bounds each database call.
const timeout = 5 * time.Second

// Every session_id argument is the hashed id (session.Data.ID), never the
// cookie value. Only live sessions match, so an expired row that wasn't
// purged yet can't be loaded or revived.
const (
	loadQuery    = "SELECT session_id, user_id, impersonated_user_id, created_on, expires_on FROM http_sessions WHERE session_id = $1 AND expires_on > now()"
	insertQuery  = "INSERT INTO http_sessions (session_id, user_id, impersonated_user_id, created_on, expires_on) VALUES ($1, $2, $3, $4, $5)"
	touchQuery   = "UPDATE http_sessions SET expires_on = $2 WHERE session_id = $1 AND expires_on > now()"
	destroyQuery = "DELETE FROM http_sessions WHERE session_id = $1"
	purgeQuery   = "DELETE FROM http_sessions WHERE expires_on < now()"

	// A row of http_service_sessions is a code while redeem_by is set, and
	// a service session once redeemed. Service sessions go with their
	// http_sessions row (ON DELETE CASCADE).
	insertCodeQuery  = "INSERT INTO http_service_sessions (id, session_id, host, redeem_by) VALUES ($1, $2, $3, $4)"
	redeemCodeQuery  = "UPDATE http_service_sessions SET id = $3, redeem_by = NULL WHERE id = $1 AND host = $2 AND redeem_by > now()"
	loadServiceQuery = "SELECT s.session_id, s.user_id, s.impersonated_user_id, s.created_on, s.expires_on " +
		"FROM http_service_sessions v JOIN http_sessions s ON s.session_id = v.session_id " +
		"WHERE v.id = $1 AND v.host = $2 AND v.redeem_by IS NULL AND s.expires_on > now()"
	purgeCodesQuery = "DELETE FROM http_service_sessions WHERE redeem_by < now()"
)

var _ session.Store[session.User] = (*Store[session.User])(nil)

type Store[T session.User] struct {
	db      database.Querier
	getUser string
}

// New returns a Store whose sessions load their users with getUser: $1 is
// the user id, and the query must return exactly one row, scanned by name
// into T strictly as database.SelectRow does: every exported field of T needs
// a column, so tag fields the query doesn't fill `db:"-"`. It panics when
// getUser is empty.
func New[T session.User](db database.Querier, getUser string) *Store[T] {
	if getUser == "" {
		panic("pgstore: the user query is required")
	}
	return &Store[T]{db: db, getUser: getUser}
}

// Load returns the stored session. A missing or expired session (or a deleted
// user) is reported as session.ErrNotFound.
func (store *Store[T]) Load(ctx context.Context, id string) (*session.Data[T], error) {
	return store.load(ctx, loadQuery, id)
}

// LoadService returns the session behind a service session in one query,
// with its users. A missing service session, or a missing or expired
// session, is reported as session.ErrNotFound.
func (store *Store[T]) LoadService(ctx context.Context, serviceID, host string) (*session.Data[T], error) {
	return store.load(ctx, loadServiceQuery, serviceID, host)
}

// load runs a query returning one http_sessions row, then loads its users.
func (store *Store[T]) load(ctx context.Context, query string, args ...any) (*session.Data[T], error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data := &session.Data[T]{}

	// ids are scanned into `any` so they round-trip to the user query with
	// whatever type the user table uses (int8, uuid, text...)
	var userID, impersonatedUserID any
	err := store.db.QueryRow(ctx, query, args...).Scan(&data.ID, &userID, &impersonatedUserID, &data.CreatedOn, &data.ExpiresOn)
	if err != nil {
		return nil, loadError("load session", err)
	}

	data.User, err = database.SelectRow[T](ctx, store.db, store.getUser, userID)
	if err != nil {
		return nil, loadError("load user", err)
	}

	if impersonatedUserID != nil {
		data.ImpersonatedUser, err = database.SelectRow[T](ctx, store.db, store.getUser, impersonatedUserID)
		if err != nil {
			return nil, loadError("load impersonated user", err)
		}
	}

	return data, nil
}

func loadError(msg string, err error) error {
	if database.IsNotFound(err) {
		err = session.ErrNotFound
	}
	return fmt.Errorf("pgstore: %s: %w", msg, err)
}

func (store *Store[T]) Insert(ctx context.Context, data *session.Data[T]) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var impersonatedUserID any
	if data.ImpersonatedUser != nil {
		impersonatedUserID = (*data.ImpersonatedUser).GetID()
	}

	_, err := store.db.Exec(ctx, insertQuery, data.ID, (*data.User).GetID(), impersonatedUserID, data.CreatedOn, data.ExpiresOn)
	if err != nil {
		return fmt.Errorf("pgstore: insert session: %w", err)
	}
	return nil
}

// Touch returns session.ErrNotFound when the session no longer exists, e.g.
// after a logout in another tab.
func (store *Store[T]) Touch(ctx context.Context, id string, expiresOn time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tag, err := store.db.Exec(ctx, touchQuery, id, expiresOn)
	if err == nil && tag.RowsAffected() == 0 {
		err = session.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("pgstore: touch session: %w", err)
	}
	return nil
}

func (store *Store[T]) Destroy(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := store.db.Exec(ctx, destroyQuery, id); err != nil {
		return fmt.Errorf("pgstore: delete session: %w", err)
	}
	return nil
}

func (store *Store[T]) InsertCode(ctx context.Context, codeID, sessionID, host string, expiresOn time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := store.db.Exec(ctx, insertCodeQuery, codeID, sessionID, host, expiresOn); err != nil {
		return fmt.Errorf("pgstore: insert code: %w", err)
	}
	return nil
}

// RedeemCode returns session.ErrNotFound for a code that is missing,
// expired, already redeemed or for another host.
func (store *Store[T]) RedeemCode(ctx context.Context, codeID, host, serviceID string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tag, err := store.db.Exec(ctx, redeemCodeQuery, codeID, host, serviceID)
	if err == nil && tag.RowsAffected() == 0 {
		err = session.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("pgstore: redeem code: %w", err)
	}
	return nil
}

// PurgeExpired deletes the rows of expired sessions, with their service
// sessions, and of expired codes.
func (store *Store[T]) PurgeExpired(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, q := range []string{purgeQuery, purgeCodesQuery} {
		if _, err := store.db.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: purge expired sessions: %w", err)
		}
	}
	return nil
}
