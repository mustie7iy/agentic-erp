// Package currency reads currency reference data (ADR-0005): the number of
// minor-unit digits each ISO 4217 code carries, which every amount on the API
// is parsed and rendered with.
package currency

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// ParseAmount reads an API amount of the currency, a decimal string with
// exactly its exp minor-unit digits, into minor units (ADR-0005). Another
// form is 400 request.invalid_amount naming field, the request's own
// field of the amount.
func ParseAmount(value, code string, exp int, field string) (money.Minor, error) {
	n, err := money.ParseAmount(value, exp)
	if err != nil {
		return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", code, exp),
			problem.FieldError{Field: field, Code: "invalid", Message: err.Error()})
	}
	return n, nil
}

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
