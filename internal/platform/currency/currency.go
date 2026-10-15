// Package currency reads currency reference data (ADR-0005): the number of
// minor-unit digits each ISO 4217 code carries, which every amount on the API
// is parsed and rendered with.
package currency

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the part of a transaction Exponent needs.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Exponent returns the minor-unit digits of the currency code. An unknown
// code returns unknown, the caller's problem on its own field; a nil unknown
// reports it as an error like any other failed read, for callers whose
// currency was already validated (it comes from a stored document).
func Exponent(ctx context.Context, q Querier, code string, unknown error) (int, error) {
	var exp int
	err := q.QueryRow(ctx, `SELECT exponent FROM ref.currency WHERE code = $1`, code).Scan(&exp)
	if unknown != nil && errors.Is(err, pgx.ErrNoRows) {
		return 0, unknown
	}
	if err != nil {
		return 0, fmt.Errorf("currency: read exponent of %s: %w", code, err)
	}
	return exp, nil
}
