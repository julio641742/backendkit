// Package page paginates list routes. Query binds ?page and ?page_size,
// Select runs the count and the page of rows from one Statement, so the two
// can't drift apart, and Page is the response:
//
//	type listInput struct {
//	    page.Query
//	    Difficulty *int `query:"difficulty"`
//	}
//
//	func (h *Handler) List(ctx context.Context, in listInput) (*page.Page[Lab], error) {
//	    var w page.Where
//	    if in.Difficulty != nil {
//	        w.And("l.difficulty = @difficulty", pgx.NamedArgs{"difficulty": *in.Difficulty})
//	    }
//	    return page.Select[Lab](ctx, h.db, in.Query, 20, page.Statement{
//	        Columns: "l.id, l.name",
//	        From:    "labs l",
//	        Where:   w,
//	        OrderBy: "l.name, l.id",
//	    })
//	}
package page

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/julio641742/backendkit/database"
)

const (
	// MaxSize is the largest page size, as Query's binding enforces.
	MaxSize = 100
	// MaxPage is the highest page number, as Query's binding enforces. It
	// keeps the offset far from overflowing.
	MaxPage = 100_000
)

// Select's own named arguments, which Where refuses.
const (
	limitArg  = "page_limit"
	offsetArg = "page_offset"
)

// Query is the page a client asks for. Embed it in a binder input struct;
// both parameters are optional, and Select fills in what is left out.
type Query struct {
	Page     int `query:"page"      binding:"omitempty,min=1,max=100000"`
	PageSize int `query:"page_size" binding:"omitempty,min=1,max=100"`
}

// Page is one page of rows and where it sits among them.
type Page[T any] struct {
	Data       []*T `json:"data"`
	TotalCount int  `json:"total_count"`
	Page       int  `json:"page"`
	PageSize   int  `json:"page_size"`
}

// Statement is a SELECT split into the parts Select needs: it counts with
// From and Where, then selects Columns ordered by OrderBy. From may hold
// joins but not the WHERE, which goes in Where. OrderBy is required, and
// should end with a unique column (an id), or rows that tie can repeat or go
// missing between pages.
type Statement struct {
	Columns string
	From    string
	Where   Where
	OrderBy string
}

// Where collects the conditions of a WHERE clause and their named arguments,
// for filters that only apply when the client asks for them. The zero value
// is an empty clause.
type Where struct {
	conds []string
	args  pgx.NamedArgs
}

// And adds a condition, which is parenthesized, so an OR in it can't escape
// into the conditions around it. Its parameters are named (@name) and given
// in args. It panics when args reuses a name an earlier condition gave, or
// one of Select's own (page_limit, page_offset).
//
// A copy of a Where can be extended without changing the original, so a
// shared base filter is safe to build on.
func (w *Where) And(cond string, args pgx.NamedArgs) {
	// Copies share the map and the slice's spare capacity; write to our own.
	w.args = maps.Clone(w.args)
	if w.args == nil {
		w.args = pgx.NamedArgs{}
	}
	w.conds = slices.Clip(w.conds)
	for name, v := range args {
		if name == limitArg || name == offsetArg {
			panic(fmt.Sprintf("page: Where argument @%s is reserved", name))
		}
		if _, ok := w.args[name]; ok {
			panic(fmt.Sprintf("page: Where argument @%s given twice", name))
		}
		w.args[name] = v
	}
	w.conds = append(w.conds, "("+cond+")")
}

// String returns " WHERE (a) AND (b)", or "" without conditions.
func (w Where) String() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// Select counts the rows s matches and returns the page q asks for, scanned
// as database.SelectRows does. A page left out is the first and a size left
// out is defaultSize; values outside Query's binding are clamped into it, so
// an unvalidated Query is safe too. A page past the last comes back with no
// rows, without querying for them.
func Select[T any](ctx context.Context, db database.Querier, q Query, defaultSize int, s Statement) (*Page[T], error) {
	if s.OrderBy == "" {
		return nil, errors.New("page: Statement.OrderBy is required")
	}
	q = q.normalize(defaultSize)

	args := maps.Clone(s.Where.args)
	if args == nil {
		args = pgx.NamedArgs{}
	}
	where := s.Where.String()

	total, err := database.SelectValue[int](ctx, db, "SELECT COUNT(*) FROM "+s.From+where, args)
	if err != nil {
		return nil, fmt.Errorf("page: counting: %w", err)
	}

	p := &Page[T]{Data: []*T{}, TotalCount: total, Page: q.Page, PageSize: q.PageSize}
	offset := (q.Page - 1) * q.PageSize
	if offset >= total {
		return p, nil
	}

	args[limitArg], args[offsetArg] = q.PageSize, offset
	p.Data, err = database.SelectRows[T](ctx, db,
		"SELECT "+s.Columns+" FROM "+s.From+where+" ORDER BY "+s.OrderBy+
			" LIMIT @"+limitArg+" OFFSET @"+offsetArg,
		args)
	if err != nil {
		return nil, fmt.Errorf("page: selecting: %w", err)
	}
	return p, nil
}

// normalize fills in the defaults and clamps q into Query's binding.
func (q Query) normalize(defaultSize int) Query {
	q.Page = min(max(q.Page, 1), MaxPage)
	if q.PageSize < 1 {
		q.PageSize = defaultSize
	}
	q.PageSize = min(max(q.PageSize, 1), MaxSize)
	return q
}
