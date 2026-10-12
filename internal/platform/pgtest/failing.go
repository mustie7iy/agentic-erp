package pgtest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrInjected is the error a FailingTx returns.
var ErrInjected = errors.New("pgtest: injected failure")

// FailingTx wraps a transaction so that its Nth statement (Exec, Query or
// QueryRow, counted from 1) fails with ErrInjected. Running a handler with
// N = 1, 2, ... until it succeeds exercises every database error path. With
// Count set, the statements of every FailingTx sharing it are counted
// together, so a handler spanning several transactions is covered too.
type FailingTx struct {
	pgx.Tx
	FailAt int
	Count  *int
	calls  int
}

// Calls is the number of statements issued so far.
func (f *FailingTx) Calls() int {
	if f.Count != nil {
		return *f.Count
	}
	return f.calls
}

func (f *FailingTx) fail() bool {
	if f.Count != nil {
		*f.Count++
		return *f.Count == f.FailAt
	}
	f.calls++
	return f.calls == f.FailAt
}

func (f *FailingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.fail() {
		return pgconn.CommandTag{}, ErrInjected
	}
	return f.Tx.Exec(ctx, sql, args...)
}

func (f *FailingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.fail() {
		return errRows{}, ErrInjected
	}
	return f.Tx.Query(ctx, sql, args...)
}

func (f *FailingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.fail() {
		return errRow{}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

type errRow struct{}

func (errRow) Scan(...any) error { return ErrInjected }

type errRows struct{}

func (errRows) Close()                                       {}
func (errRows) Err() error                                   { return ErrInjected }
func (errRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (errRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (errRows) Next() bool                                   { return false }
func (errRows) Scan(...any) error                            { return ErrInjected }
func (errRows) Values() ([]any, error)                       { return nil, ErrInjected }
func (errRows) RawValues() [][]byte                          { return nil }
func (errRows) Conn() *pgx.Conn                              { return nil }
func (errRows) TypeMap() *pgtype.Map                         { return nil }
