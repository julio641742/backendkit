package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type user struct {
	ID    int64  `db:"id"`
	Name  string `db:"name"`
	Email string `db:"email"`
}

func TestSelectValue(t *testing.T) {
	ctx := context.Background()
	const query = "SELECT id FROM users WHERE email = $1"

	t.Run("returns the value", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{"a@b.c"}, rows: newRows("id").add(int64(42))}

		id, err := SelectValue[int64](ctx, q, query, "a@b.c")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != 42 {
			t.Errorf("got %d, want 42", id)
		}
		if !q.rows.closed {
			t.Error("rows were not closed")
		}
	})

	t.Run("no rows is IsNotFound", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{"a@b.c"}, rows: newRows("id")}

		_, err := SelectValue[int64](ctx, q, query, "a@b.c")
		if !IsNotFound(err) {
			t.Errorf("got %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("more than one row is an error", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{"a@b.c"}, rows: newRows("id").add(int64(1)).add(int64(2))}

		_, err := SelectValue[int64](ctx, q, query, "a@b.c")
		if !errors.Is(err, pgx.ErrTooManyRows) {
			t.Errorf("got %v, want pgx.ErrTooManyRows", err)
		}
	})

	t.Run("query error is returned", func(t *testing.T) {
		wantErr := errors.New("boom")
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{"a@b.c"}, queryErr: wantErr}

		_, err := SelectValue[int64](ctx, q, query, "a@b.c")
		if !errors.Is(err, wantErr) {
			t.Errorf("got %v, want %v", err, wantErr)
		}
	})
}

func TestSelectValues(t *testing.T) {
	ctx := context.Background()
	const query = "SELECT name FROM users"

	t.Run("returns all values", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, rows: newRows("name").add("ana").add("bob")}

		names, err := SelectValues[string](ctx, q, query)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Join(names, ",") != "ana,bob" {
			t.Errorf("got %v, want [ana bob]", names)
		}
	})

	t.Run("no rows returns empty non-nil slice", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, rows: newRows("name")}

		names, err := SelectValues[string](ctx, q, query)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if names == nil || len(names) != 0 {
			t.Errorf("got %#v, want empty non-nil slice", names)
		}
	})

	t.Run("query error is returned", func(t *testing.T) {
		wantErr := errors.New("boom")
		q := &fakeQuerier{t: t, wantSQL: query, queryErr: wantErr}

		_, err := SelectValues[string](ctx, q, query)
		if !errors.Is(err, wantErr) {
			t.Errorf("got %v, want %v", err, wantErr)
		}
	})
}

func TestSelectRow(t *testing.T) {
	ctx := context.Background()
	const query = "SELECT id, name, email FROM users WHERE id = $1"

	t.Run("maps columns to struct by db tag", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{int64(1)},
			rows: newRows("id", "name", "email").add(int64(1), "ana", "ana@x.io")}

		u, err := SelectRow[user](ctx, q, query, int64(1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := user{ID: 1, Name: "ana", Email: "ana@x.io"}
		if *u != want {
			t.Errorf("got %+v, want %+v", *u, want)
		}
	})

	t.Run("column order does not matter", func(t *testing.T) {
		const reordered = "SELECT email, id, name FROM users WHERE id = $1"
		q := &fakeQuerier{t: t, wantSQL: reordered, wantArgs: []any{int64(1)},
			rows: newRows("email", "id", "name").add("ana@x.io", int64(1), "ana")}

		u, err := SelectRow[user](ctx, q, reordered, int64(1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := user{ID: 1, Name: "ana", Email: "ana@x.io"}
		if *u != want {
			t.Errorf("got %+v, want %+v", *u, want)
		}
	})

	t.Run("missing column for a field is an error", func(t *testing.T) {
		const partial = "SELECT id, name FROM users WHERE id = $1"
		q := &fakeQuerier{t: t, wantSQL: partial, wantArgs: []any{int64(1)},
			rows: newRows("id", "name").add(int64(1), "ana")}

		_, err := SelectRow[user](ctx, q, partial, int64(1))
		if err == nil {
			t.Error("expected error for struct field with no matching column")
		}
	})

	t.Run("extra column without a field is an error", func(t *testing.T) {
		const extra = "SELECT id, name, email, age FROM users WHERE id = $1"
		q := &fakeQuerier{t: t, wantSQL: extra, wantArgs: []any{int64(1)},
			rows: newRows("id", "name", "email", "age").add(int64(1), "ana", "ana@x.io", int64(30))}

		_, err := SelectRow[user](ctx, q, extra, int64(1))
		if err == nil {
			t.Error("expected error for column with no matching struct field")
		}
	})

	t.Run("field tagged db:\"-\" needs no column", func(t *testing.T) {
		type withExtra struct {
			ID    int64    `db:"id"`
			Roles []string `db:"-"`
		}
		const q1 = "SELECT id FROM users WHERE id = $1"
		q := &fakeQuerier{t: t, wantSQL: q1, wantArgs: []any{int64(1)},
			rows: newRows("id").add(int64(1))}

		u, err := SelectRow[withExtra](ctx, q, q1, int64(1))
		if err != nil || u.ID != 1 || u.Roles != nil {
			t.Errorf("got %+v, %v", u, err)
		}
	})

	t.Run("no rows is IsNotFound", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, wantArgs: []any{int64(1)}, rows: newRows("id", "name", "email")}

		u, err := SelectRow[user](ctx, q, query, int64(1))
		if !IsNotFound(err) {
			t.Errorf("got %v, want pgx.ErrNoRows", err)
		}
		if u != nil {
			t.Errorf("got %+v, want nil", u)
		}
	})
}

func TestSelectRows(t *testing.T) {
	ctx := context.Background()
	const query = "SELECT id, name, email FROM users"

	t.Run("returns all rows", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, rows: newRows("id", "name", "email").
			add(int64(1), "ana", "ana@x.io").
			add(int64(2), "bob", "bob@x.io")}

		users, err := SelectRows[user](ctx, q, query)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(users) != 2 || users[0].Name != "ana" || users[1].Name != "bob" {
			t.Errorf("got %+v", users)
		}
		if users[0] == users[1] {
			t.Error("rows share the same pointer")
		}
	})

	t.Run("no rows returns empty non-nil slice", func(t *testing.T) {
		q := &fakeQuerier{t: t, wantSQL: query, rows: newRows("id", "name", "email")}

		users, err := SelectRows[user](ctx, q, query)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if users == nil || len(users) != 0 {
			t.Errorf("got %#v, want empty non-nil slice", users)
		}
	})

	t.Run("error while iterating is returned", func(t *testing.T) {
		wantErr := errors.New("connection lost")
		q := &fakeQuerier{t: t, wantSQL: query, rows: newRows("id", "name", "email").
			add(int64(1), "ana", "ana@x.io").
			add(int64(2), "bob", "bob@x.io").
			failAt(1, wantErr)}

		users, err := SelectRows[user](ctx, q, query)
		if !errors.Is(err, wantErr) {
			t.Errorf("got %v, want %v", err, wantErr)
		}
		if users != nil {
			t.Errorf("got %+v, want nil on error", users)
		}
	})
}

func TestErrorHelpers(t *testing.T) {
	unique := &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}
	fk := &pgconn.PgError{Code: "23503", ConstraintName: "orders_user_id_fkey"}
	other := &pgconn.PgError{Code: "22001"} // string_data_right_truncation

	tests := []struct {
		name       string
		err        error
		notFound   bool
		unique     bool
		constraint string
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("x")},
		{name: "no rows", err: pgx.ErrNoRows, notFound: true},
		{name: "too many rows", err: pgx.ErrTooManyRows},
		{name: "wrapped no rows", err: fmt.Errorf("get user: %w", pgx.ErrNoRows), notFound: true},
		{name: "unique violation", err: unique, unique: true, constraint: "users_email_key"},
		{name: "wrapped unique violation", err: fmt.Errorf("create user: %w", unique), unique: true, constraint: "users_email_key"},
		{name: "foreign key violation", err: fk},
		{name: "other pg error", err: other},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.notFound {
				t.Errorf("IsNotFound = %v, want %v", got, tt.notFound)
			}
			if c, ok := UniqueViolation(tt.err); ok != tt.unique || (ok && c != tt.constraint) {
				t.Errorf("UniqueViolation = %q, %v", c, ok)
			}
		})
	}
}

func TestConnect(t *testing.T) {
	t.Run("invalid DSN", func(t *testing.T) {
		db, err := Connect(context.Background(), "postgres://user:pass@localhost:notaport/db")
		if err == nil {
			db.Close()
			t.Fatal("expected error for invalid DSN")
		}
	})

	t.Run("unreachable server fails on ping", func(t *testing.T) {
		// Port 1 refuses connections, so this fails fast without a real database.
		db, err := Connect(context.Background(), "postgres://user:pass@127.0.0.1:1/db?connect_timeout=1")
		if err == nil {
			db.Close()
			t.Fatal("expected error for unreachable server")
		}
	})
}

func TestQueryErrorWithNilRows(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("boom")
	newQ := func() *fakeQuerier {
		return &fakeQuerier{t: t, wantSQL: "q", queryErr: wantErr, nilRowsOnErr: true}
	}

	if _, err := SelectValue[int64](ctx, newQ(), "q"); !errors.Is(err, wantErr) {
		t.Errorf("SelectValue: got %v, want %v", err, wantErr)
	}
	if _, err := SelectValues[int64](ctx, newQ(), "q"); !errors.Is(err, wantErr) {
		t.Errorf("SelectValues: got %v, want %v", err, wantErr)
	}
	if _, err := SelectRow[user](ctx, newQ(), "q"); !errors.Is(err, wantErr) {
		t.Errorf("SelectRow: got %v, want %v", err, wantErr)
	}
	if _, err := SelectRows[user](ctx, newQ(), "q"); !errors.Is(err, wantErr) {
		t.Errorf("SelectRows: got %v, want %v", err, wantErr)
	}
}
