package pgstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/julio641742/backendkit/database"
)

var _ database.Querier = (*fakeDB)(nil)

const getUserQuery = "SELECT id, email FROM users WHERE id = $1"

type testUser struct {
	ID    int64
	Email string
}

func (u testUser) GetID() any { return u.ID }

type fakeSession struct {
	userID             any
	impersonatedUserID any
	createdOn          time.Time
	expiresOn          time.Time
}

// fakeService is a row of http_service_sessions: a code while redeemBy is
// set.
type fakeService struct {
	sessionID string
	host      string
	redeemBy  *time.Time
}

// fakeDB is an in-memory http_sessions + http_service_sessions + users
// table that understands the session queries and getUserQuery.
type fakeDB struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession
	services map[string]*fakeService
	users    map[int64]testUser

	// execErr / queryErr make every Exec / Query fail.
	execErr  error
	queryErr error
}

func newFakeDB(users ...testUser) *fakeDB {
	db := &fakeDB{sessions: map[string]*fakeSession{}, services: map[string]*fakeService{}, users: map[int64]testUser{}}
	for _, u := range users {
		db.users[u.ID] = u
	}
	return db
}

func (db *fakeDB) session(id string) (*fakeSession, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.sessions[id]
	return s, ok
}

func (db *fakeDB) sessionCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.sessions)
}

// deleteSession deletes a session with its service sessions, as the ON
// DELETE CASCADE does. db.mu must be held.
func (db *fakeDB) deleteSession(id string) {
	delete(db.sessions, id)
	for sid, c := range db.services {
		if c.sessionID == id {
			delete(db.services, sid)
		}
	}
}

func (db *fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.execErr != nil {
		return pgconn.CommandTag{}, db.execErr
	}

	switch sql {
	case insertQuery:
		db.sessions[args[0].(string)] = &fakeSession{userID: args[1], impersonatedUserID: args[2], createdOn: args[3].(time.Time), expiresOn: args[4].(time.Time)}
	case touchQuery:
		s, ok := db.sessions[args[0].(string)]
		if !ok || !s.expiresOn.After(time.Now()) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		s.expiresOn = args[1].(time.Time)
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case destroyQuery:
		db.deleteSession(args[0].(string))
	case purgeQuery:
		for id, s := range db.sessions {
			if s.expiresOn.Before(time.Now()) {
				db.deleteSession(id)
			}
		}
	case insertCodeQuery:
		redeemBy := args[3].(time.Time)
		db.services[args[0].(string)] = &fakeService{sessionID: args[1].(string), host: args[2].(string), redeemBy: &redeemBy}
	case redeemCodeQuery:
		c, ok := db.services[args[0].(string)]
		if !ok || c.host != args[1].(string) || c.redeemBy == nil || !c.redeemBy.After(time.Now()) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		delete(db.services, args[0].(string))
		c.redeemBy = nil
		db.services[args[2].(string)] = c
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case purgeCodesQuery:
		for id, c := range db.services {
			if c.redeemBy != nil && c.redeemBy.Before(time.Now()) {
				delete(db.services, id)
			}
		}
	default:
		return pgconn.CommandTag{}, fmt.Errorf("fakeDB: unexpected exec %q", sql)
	}
	return pgconn.CommandTag{}, nil
}

func (db *fakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.queryErr != nil {
		return &fakeRows{err: db.queryErr}, db.queryErr
	}

	switch sql {
	case loadQuery, loadServiceQuery:
		id := args[0].(string)
		if sql == loadServiceQuery {
			c, ok := db.services[id]
			if !ok || c.host != args[1].(string) || c.redeemBy != nil {
				id = ""
			} else {
				id = c.sessionID
			}
		}
		rows := newRows("session_id", "user_id", "impersonated_user_id", "created_on", "expires_on")
		if s, ok := db.sessions[id]; ok && s.expiresOn.After(time.Now()) {
			rows.add(id, s.userID, s.impersonatedUserID, s.createdOn, s.expiresOn)
		}
		return rows, nil
	case getUserQuery:
		rows := newRows("id", "email")
		if u, ok := db.users[args[0].(int64)]; ok {
			rows.add(u.ID, u.Email)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("fakeDB: unexpected query %q", sql)
	}
}

func (db *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return &fakeRows{err: err}
	}
	return rows.(pgx.Row)
}

// fakeRows implements pgx.Rows (and pgx.Row) over in-memory values.
type fakeRows struct {
	columns []string
	data    [][]any
	pos     int
	err     error
	closed  bool
}

func newRows(columns ...string) *fakeRows {
	return &fakeRows{columns: columns, pos: -1}
}

func (r *fakeRows) add(values ...any) *fakeRows {
	r.data = append(r.data, values)
	return r
}

func (r *fakeRows) Close()                        { r.closed = true }
func (r *fakeRows) Err() error                    { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeRows) Conn() *pgx.Conn               { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map          { return pgtype.NewMap() }
func (r *fakeRows) RawValues() [][]byte           { return nil }
func (r *fakeRows) Values() ([]any, error)        { return r.data[r.pos], nil }

func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	fds := make([]pgconn.FieldDescription, len(r.columns))
	for i, c := range r.columns {
		fds[i] = pgconn.FieldDescription{Name: c}
	}
	return fds
}

func (r *fakeRows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	r.pos++
	if r.pos >= len(r.data) {
		r.Close()
		return false
	}
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	// pgx.Row semantics: Scan on a Row without calling Next first.
	if r.pos == -1 {
		defer r.Close()
		if !r.Next() {
			return pgx.ErrNoRows
		}
	}

	row := r.data[r.pos]
	if len(dest) != len(row) {
		return fmt.Errorf("scan: got %d destinations for %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer || dv.IsNil() {
			return errors.New("scan: destination must be a non-nil pointer")
		}
		v := reflect.ValueOf(row[i])
		target := dv.Elem()
		if !v.IsValid() {
			target.SetZero()
			continue
		}
		if !v.Type().ConvertibleTo(target.Type()) {
			return fmt.Errorf("scan: cannot assign %T to %s", row[i], target.Type())
		}
		target.Set(v.Convert(target.Type()))
	}
	return nil
}
