package database

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Both the pool and a transaction must satisfy Querier so the helpers work in either.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = pgx.Tx(nil)
)

// fakeQuerier is a minimal Querier that records the last call and returns canned results.
type fakeQuerier struct {
	t *testing.T

	wantSQL  string
	wantArgs []any

	rows     *fakeRows
	queryErr error
	// nilRowsOnErr returns (nil, queryErr) instead of pgx's error-carrying Rows,
	// as a hand-written Querier may.
	nilRowsOnErr bool

	called bool
}

func (f *fakeQuerier) check(sql string, args []any) {
	f.t.Helper()
	f.called = true
	if sql != f.wantSQL {
		f.t.Errorf("sql = %q, want %q", sql, f.wantSQL)
	}
	if !reflect.DeepEqual(args, f.wantArgs) && (len(args) != 0 || len(f.wantArgs) != 0) {
		f.t.Errorf("args = %#v, want %#v", args, f.wantArgs)
	}
}

func (f *fakeQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.check(sql, args)
	return pgconn.CommandTag{}, f.queryErr
}

func (f *fakeQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.check(sql, args)
	if f.queryErr != nil && f.nilRowsOnErr {
		return nil, f.queryErr
	}
	if f.queryErr != nil {
		// Match pgx: a failed Query still returns Rows that report the error.
		return &fakeRows{err: f.queryErr}, f.queryErr
	}
	return f.rows, nil
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, _ := f.Query(ctx, sql, args...)
	return rows.(pgx.Row)
}

// fakeRows implements pgx.Rows over in-memory values.
type fakeRows struct {
	columns []string
	data    [][]any

	// errAt makes Next fail when it reaches this row index (0-based); -1 disables it.
	errAt  int
	iterEr error

	pos    int
	err    error
	closed bool
}

func newRows(columns ...string) *fakeRows {
	return &fakeRows{columns: columns, errAt: -1, pos: -1}
}

func (r *fakeRows) add(values ...any) *fakeRows {
	r.data = append(r.data, values)
	return r
}

func (r *fakeRows) failAt(row int, err error) *fakeRows {
	r.errAt, r.iterEr = row, err
	return r
}

func (r *fakeRows) Close()                        { r.closed = true }
func (r *fakeRows) Err() error                    { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeRows) Conn() *pgx.Conn               { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map          { return pgtype.NewMap() }
func (r *fakeRows) RawValues() [][]byte           { return nil }

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
	if r.pos == r.errAt {
		r.err = r.iterEr
		r.Close()
		return false
	}
	if r.pos >= len(r.data) {
		r.Close()
		return false
	}
	return true
}

func (r *fakeRows) Values() ([]any, error) {
	return r.data[r.pos], nil
}

func (r *fakeRows) Scan(dest ...any) error {
	// pgx.Row semantics: Scan on a Row without calling Next first.
	if r.pos == -1 {
		defer r.Close()
		if r.err != nil {
			return r.err
		}
		if !r.Next() {
			if r.err != nil {
				return r.err
			}
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
