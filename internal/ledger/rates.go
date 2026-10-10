package ledger

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	problem.RegisterConstraint("exchange_rate_pkey", func() *problem.Problem {
		return problem.Conflict("ledger.exchange_rate_exists", "A rate of this type for this currency pair is already recorded from this date; rates are insert-only.")
	})
	problem.RegisterConstraint("exchange_rate_from_currency_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.unknown_currency", "The currency is not known.",
			problem.FieldError{Field: "from_currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"})
	})
	problem.RegisterConstraint("exchange_rate_to_currency_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.unknown_currency", "The currency is not known.",
			problem.FieldError{Field: "to_currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"})
	})
}

// RateTypes are the rate types the database accepts.
var RateTypes = map[string]bool{"average": true, "bank_buying": true, "bank_selling": true, "planning": true}

// Rate is a row of ledger.exchange_rate.
type Rate struct {
	ID           uuid.UUID
	RateType     string
	FromCurrency string
	ToCurrency   string
	ValidFrom    time.Time
	Rate         string
	Source       string
	CreatedAt    time.Time
	CreatedBy    uuid.UUID
}

const rateColumns = `id, rate_type, from_currency, to_currency, valid_from, rate::text, source, created_at, created_by`

func scanRate(row pgx.Row) (Rate, error) {
	var r Rate
	err := row.Scan(&r.ID, &r.RateType, &r.FromCurrency, &r.ToCurrency, &r.ValidFrom, &r.Rate, &r.Source, &r.CreatedAt, &r.CreatedBy)
	return r, err
}

// GetRate returns an exchange rate.
func GetRate(ctx context.Context, id uuid.UUID) (Rate, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Rate{}, err
	}
	r, err := scanRate(tx.QueryRow(ctx, `SELECT `+rateColumns+` FROM ledger.exchange_rate WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, problem.NotFound("No exchange rate with this id.")
	}
	return r, err
}

// NewRate is the input of CreateRate.
type NewRate struct {
	RateType, FromCurrency, ToCurrency string
	ValidFrom                          time.Time
	Rate, Source                       string
}

// CreateRate records an exchange rate. Rates are insert-only: a correction
// is a new rate from a later date.
func CreateRate(ctx context.Context, in NewRate) (Rate, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Rate{}, err
	}
	if !RateTypes[in.RateType] {
		return Rate{}, problem.Unprocessable("ledger.unknown_rate_type", "Unknown rate type.",
			problem.FieldError{Field: "rate_type", Code: "unknown", Message: "average, bank_buying, bank_selling or planning"})
	}
	if in.FromCurrency == in.ToCurrency {
		return Rate{}, problem.Unprocessable("ledger.same_currency", "A rate converts between two different currencies.",
			problem.FieldError{Field: "to_currency", Code: "same_currency", Message: "a currency other than from_currency"})
	}
	if r, _ := new(big.Rat).SetString(in.Rate); r.Sign() <= 0 { // the contract's pattern admits only decimals
		return Rate{}, problem.Unprocessable("ledger.invalid_rate", "A rate is positive.",
			problem.FieldError{Field: "rate", Code: "not_positive", Message: "units of to_currency per unit of from_currency, above zero"})
	}
	r, err := scanRate(tx.QueryRow(ctx, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9) RETURNING `+rateColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), in.RateType, in.FromCurrency, in.ToCurrency, in.ValidFrom, in.Rate, in.Source, p.ID))
	if err != nil {
		return Rate{}, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "exchange_rate_maintained", AggregateType: "ledger.exchange_rate", AggregateID: r.ID,
		Payload: map[string]any{"exchange_rate_id": r.ID, "rate_type": r.RateType, "from_currency": r.FromCurrency, "to_currency": r.ToCurrency,
			"valid_from": r.ValidFrom.Format(time.DateOnly), "rate": r.Rate, "source": r.Source}}); err != nil {
		return Rate{}, err
	}
	return r, audit.Add(ctx, audit.Entry{Action: "ledger.exchange_rate.create", ObjectType: "ledger.exchange_rate", ObjectID: r.ID, Outcome: "succeeded",
		Detail: map[string]any{"rate_type": r.RateType, "pair": r.FromCurrency + "/" + r.ToCurrency, "valid_from": r.ValidFrom.Format(time.DateOnly), "rate": r.Rate}})
}
