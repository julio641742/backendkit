package page

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/julio641742/backendkit/binder"
)

type lab struct {
	ID   int
	Name string
}

var stmt = Statement{Columns: "l.id, l.name", From: "labs l", OrderBy: "l.name, l.id"}

func TestSelect(t *testing.T) {
	var w Where
	w.And("l.public OR l.owner = @me", pgx.NamedArgs{"me": 7})
	w.And("l.difficulty = @d", pgx.NamedArgs{"d": 1})
	s := stmt
	s.Where = w

	db := &fakeDB{total: 3, columns: []string{"id", "name"}, rows: [][]any{{3, "c"}}}
	p, err := Select[lab](context.Background(), db, Query{Page: 2, PageSize: 2}, 8, s)
	if err != nil {
		t.Fatal(err)
	}

	where := " WHERE (l.public OR l.owner = @me) AND (l.difficulty = @d)"
	wantQueries := []string{
		"SELECT COUNT(*) FROM labs l" + where,
		"SELECT l.id, l.name FROM labs l" + where + " ORDER BY l.name, l.id LIMIT @page_limit OFFSET @page_offset",
	}
	if !reflect.DeepEqual(db.queries, wantQueries) {
		t.Fatalf("queries = %q\nwant      %q", db.queries, wantQueries)
	}
	wantArgs := pgx.NamedArgs{"me": 7, "d": 1, "page_limit": 2, "page_offset": 2}
	if !reflect.DeepEqual(db.args[1], wantArgs) {
		t.Fatalf("args = %v, want %v", db.args[1], wantArgs)
	}
	if _, ok := db.args[0]["page_limit"]; ok {
		t.Fatal("the count query got the page arguments")
	}
	if !reflect.DeepEqual(p, &Page[lab]{Data: []*lab{{3, "c"}}, TotalCount: 3, Page: 2, PageSize: 2}) {
		t.Fatalf("page = %+v", p)
	}
	// The Where can be reused: Select must not add its arguments to it.
	if _, ok := w.args["page_limit"]; ok {
		t.Fatal("Select changed the Where's arguments")
	}
}

func TestSelectNormalizes(t *testing.T) {
	tests := []struct {
		name       string
		q          Query
		page, size int
	}{
		{"defaults", Query{}, 1, 8},
		{"negative", Query{Page: -1, PageSize: -5}, 1, 8},
		{"size too large", Query{PageSize: 1_000_000}, 1, MaxSize},
		// (Page-1)*PageSize would overflow to a negative offset.
		{"page too large", Query{Page: 1<<63 - 1, PageSize: 100}, MaxPage, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeDB{total: 1 << 40, columns: []string{"id", "name"}}
			p, err := Select[lab](context.Background(), db, tt.q, 8, stmt)
			if err != nil {
				t.Fatal(err)
			}
			if p.Page != tt.page || p.PageSize != tt.size {
				t.Fatalf("page %d size %d, want %d and %d", p.Page, p.PageSize, tt.page, tt.size)
			}
			if got := db.args[1]["page_offset"]; got != (tt.page-1)*tt.size {
				t.Fatalf("offset = %v", got)
			}
		})
	}
}

func TestSelectPastTheEnd(t *testing.T) {
	db := &fakeDB{total: 3}
	p, err := Select[lab](context.Background(), db, Query{Page: 5}, 2, stmt)
	if err != nil {
		t.Fatal(err)
	}
	if len(db.queries) != 1 {
		t.Fatalf("queries = %q, want only the count", db.queries)
	}
	if p.TotalCount != 3 || p.Page != 5 || p.Data == nil || len(p.Data) != 0 {
		t.Fatalf("page = %+v, want no rows (not nil) and the total", p)
	}
	if data, _ := json.Marshal(p); !strings.Contains(string(data), `"data":[]`) {
		t.Fatalf("json = %s", data)
	}
}

func TestSelectErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Select[lab](context.Background(), &fakeDB{err: boom}, Query{}, 8, stmt); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if _, err := Select[lab](context.Background(), &fakeDB{}, Query{}, 8, Statement{From: "labs"}); err == nil {
		t.Fatal("a Statement without OrderBy was accepted")
	}
}

func TestWhere(t *testing.T) {
	var w Where
	if got := w.String(); got != "" {
		t.Fatalf("empty Where = %q", got)
	}
	w.And("a = @a", pgx.NamedArgs{"a": 1})
	if got := w.String(); got != " WHERE (a = @a)" {
		t.Fatalf("Where = %q", got)
	}

	for name, fn := range map[string]func(){
		"duplicate": func() { w.And("b = @a", pgx.NamedArgs{"a": 2}) },
		"reserved":  func() { w.And("x", pgx.NamedArgs{"page_limit": 2}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if msg, _ := recover().(string); !strings.HasPrefix(msg, "page: ") {
					t.Fatalf("panic = %q", msg)
				}
			}()
			fn()
		})
	}
}

func TestWhereCopies(t *testing.T) {
	var base Where
	for _, c := range []string{"a", "b", "c"} {
		base.And(c+" = @"+c, pgx.NamedArgs{c: 1})
	}
	w1, w2 := base, base
	w1.And("x = @x", pgx.NamedArgs{"x": 1})
	w2.And("y = @x", pgx.NamedArgs{"x": 2})

	const prefix = " WHERE (a = @a) AND (b = @b) AND (c = @c)"
	for _, tt := range []struct {
		w        Where
		want     string
		wantArgs pgx.NamedArgs
	}{
		{base, prefix, pgx.NamedArgs{"a": 1, "b": 1, "c": 1}},
		{w1, prefix + " AND (x = @x)", pgx.NamedArgs{"a": 1, "b": 1, "c": 1, "x": 1}},
		{w2, prefix + " AND (y = @x)", pgx.NamedArgs{"a": 1, "b": 1, "c": 1, "x": 2}},
	} {
		if got := tt.w.String(); got != tt.want {
			t.Errorf("Where = %q, want %q", got, tt.want)
		}
		if !reflect.DeepEqual(tt.w.args, tt.wantArgs) {
			t.Errorf("args = %v, want %v", tt.w.args, tt.wantArgs)
		}
	}
}

// Query's binding rejects what Select would clamp, so clients get a 400.
func TestQueryBinding(t *testing.T) {
	h := binder.NewBinder().Bind(func(Query) error { return nil })
	tests := []struct {
		query string
		want  int
	}{
		{"", http.StatusNoContent},
		{"?page=2&page_size=100", http.StatusNoContent},
		{"?page_size=101", http.StatusBadRequest},
		{"?page_size=0", http.StatusNoContent}, // empty and zero count as left out
		{"?page=100001", http.StatusBadRequest},
		{"?page=-1", http.StatusBadRequest},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/"+tt.query, nil))
		if rec.Code != tt.want {
			t.Errorf("%q: status = %d, want %d; body: %s", tt.query, rec.Code, tt.want, rec.Body)
		}
	}
}

// fakeDB answers the count with total and the page with rows, recording each
// query and its named arguments.
type fakeDB struct {
	total   int
	columns []string
	rows    [][]any
	err     error

	queries []string
	args    []pgx.NamedArgs
}

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeDB: Exec")
}

func (f *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("fakeDB: QueryRow")
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, maps.Clone(args[0].(pgx.NamedArgs))) // later changes must not show
	if f.err != nil {
		return nil, f.err
	}
	if strings.HasPrefix(sql, "SELECT COUNT(*)") {
		return &fakeRows{columns: []string{"count"}, data: [][]any{{f.total}}}, nil
	}
	return &fakeRows{columns: f.columns, data: f.rows}, nil
}

type fakeRows struct {
	columns []string
	data    [][]any
	pos     int
}

func (r *fakeRows) Close()                        {}
func (r *fakeRows) Err() error                    { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeRows) Conn() *pgx.Conn               { return nil }
func (r *fakeRows) RawValues() [][]byte           { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map          { return pgtype.NewMap() }
func (r *fakeRows) Values() ([]any, error)        { return r.data[r.pos-1], nil }
func (r *fakeRows) Next() bool                    { r.pos++; return r.pos <= len(r.data) }

func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	fds := make([]pgconn.FieldDescription, len(r.columns))
	for i, c := range r.columns {
		fds[i] = pgconn.FieldDescription{Name: c}
	}
	return fds
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan: %d destinations for %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(row[i]))
	}
	return nil
}
