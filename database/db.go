package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what the Select* functions need: a *pgxpool.Pool, pgx.Tx or
// *pgx.Conn. Run transactions with pgx.BeginFunc.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func SelectValue[T any](ctx context.Context, q Querier, sql string, args ...any) (T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		var zero T
		return zero, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowTo[T])
}

func SelectValues[T any](ctx context.Context, q Querier, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[T])
}

// SelectRow scans exactly one row into a T, matching columns to fields by
// name (case-insensitive, or the field's `db` tag). The mapping is strict:
// every column needs a field and every exported field a column, so a query
// that forgets a column fails rather than leaving the field zero. Tag fields
// that no query fills (computed or loaded separately) `db:"-"`.
//
//	type User struct {
//		ID    int64  `db:"id"`
//		Email string `db:"email"`
//		Roles []string `db:"-"` // loaded separately
//	}
func SelectRow[T any](ctx context.Context, q Querier, sql string, args ...any) (*T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[T])
}

// SelectRows is SelectRow for any number of rows, with the same strict
// mapping.
func SelectRows[T any](ctx context.Context, q Querier, sql string, args ...any) ([]*T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByName[T])
}

// connectTimeout bounds Connect when ctx has no deadline.
const connectTimeout = 5 * time.Second

// Connect opens a pool and pings it. Pool settings go in the DSN
// (pool_max_conns, pool_health_check_period...). The connect and ping are
// bounded by ctx's deadline, or by 5s when ctx has none.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	dbconfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, connectTimeout)
		defer cancel()
	}

	db, err := pgxpool.NewWithConfig(ctx, dbconfig)
	if err != nil {
		return nil, err
	}

	err = db.Ping(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// IsNotFound reports whether err is pgx.ErrNoRows. It does not cover
// pgx.ErrTooManyRows, which SelectValue and SelectRow return when the query
// matches more than one row: that is a bug in the query, not a missing row.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// codeUniqueViolation is the SQLSTATE of a unique violation, from
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const codeUniqueViolation = "23505"

// UniqueViolation reports whether err is a unique violation and returns the
// violated constraint's name, so callers can map it to a field error.
//
//	if c, ok := database.UniqueViolation(err); ok && c == "users_email_key" {
//		return nil, binder.FieldErr(http.StatusConflict, "email", "is already taken")
//	}
func UniqueViolation(err error) (constraint string, ok bool) {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != codeUniqueViolation {
		return "", false
	}
	return pgErr.ConstraintName, true
}
