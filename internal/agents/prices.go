package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Price is a model's published price per million tokens, as an operator
// records it: decimal amounts of the currency's major units (USD 4.00),
// from EffectiveFrom (YYYY-MM-DD) on.
type Price struct {
	Model         string `json:"model"`
	EffectiveFrom string `json:"effective_from"`
	Currency      string `json:"currency"`
	Input         string `json:"input"`
	Output        string `json:"output"`
	CacheWrite    string `json:"cache_write"`
	CacheRead     string `json:"cache_read"`
}

// modelID is a model's id as ref.model_price holds it.
var modelID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,99}$`)

// RecordPrices records prices in tx, kept in millionths of the minor unit
// with who recorded them (ADR-0036). A price, once recorded, stands: one
// for a model and date recorded already is refused, and a change is a new
// price from a later date. tx runs as a role that may write reference data
// (erp-server prices load).
func RecordPrices(ctx context.Context, tx pgx.Tx, by string, prices []Price) error {
	for _, p := range prices {
		if err := recordPrice(ctx, tx, by, p); err != nil {
			return fmt.Errorf("agents: price of %s from %s: %w", p.Model, p.EffectiveFrom, err)
		}
	}
	return nil
}

func recordPrice(ctx context.Context, tx pgx.Tx, by string, p Price) error {
	if !modelID.MatchString(p.Model) {
		return errors.New("model must be a model id such as claude-opus-5-5")
	}
	if _, err := time.Parse(time.DateOnly, p.EffectiveFrom); err != nil {
		return errors.New("effective_from must be a date such as 2026-11-01")
	}
	var exponent int
	err := tx.QueryRow(ctx, `SELECT exponent FROM ref.currency WHERE code = $1`, p.Currency).Scan(&exponent)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("currency %s is not in the reference data", p.Currency)
	case err != nil:
		return fmt.Errorf("read currency %s: %w", p.Currency, err)
	}
	var amounts [4]money.Micro
	for i, f := range []struct{ name, value string }{{"input", p.Input}, {"output", p.Output}, {"cache_write", p.CacheWrite}, {"cache_read", p.CacheRead}} {
		// Millionths of the minor unit are the major unit's exponent+6th
		// decimal.
		m, err := money.ParseDecimal(f.value, exponent+6)
		if err != nil || m < 0 {
			return fmt.Errorf("%s must be a non-negative amount of %s with at most %d decimals", f.name, p.Currency, exponent+6)
		}
		amounts[i] = money.Micro(m)
	}
	_, err = tx.Exec(ctx, `INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.Model, p.EffectiveFrom, p.Currency, amounts[0], amounts[1], amounts[2], amounts[3], by)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "23505":
		return errors.New("a price is already recorded for this model and date; record a change from a later date")
	case err != nil:
		return fmt.Errorf("record: %w", err)
	}
	return nil
}
