package analytics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The cash actuals (ADR-0051 §6, §11; T53j1): each bank statement line's
// share of a liquidity item, by value date, classified by the cash
// context; and the forecast (T53j2): the snapshots' lines at document
// level, each with its lag. The API pages a currency's lines over at most
// 36 months of value dates, which cannot cover a history of unknown
// length, so the database path reads the shares through a port the cash
// context implements (wired by the composition root, ADR-0044 As built
// T53j1), as the caller: the endpoint has required cash.forecast.read for
// the dataset, and the cash context refuses a legal entity the caller
// cannot act in, as its API does. The snapshots' lines are read through
// the same port: listCashSnapshotFlows pages one snapshot's lines of one
// currency, a call per snapshot and currency, and names no item's name.

const (
	cashActuals   = "cash_actuals"
	cashSnapshots = "cash_snapshots"
)

// CashShare is a statement line's share of a liquidity item as the cash
// context classifies it: the line by its statement and number, its value
// date, its house bank's code, its currency and that currency's exponent,
// what classified it (the actuals' match), the item's code and name (""
// for no item) and the signed amount in minor units.
type CashShare struct {
	StatementID   uuid.UUID
	LineNo        int32
	ValueDate     time.Time
	HouseBankCode string
	Currency      string
	Exponent      int
	Match         string
	Item          string
	ItemName      string
	Amount        money.Minor
}

// ForecastShare is a snapshot's line at document level as the cash
// context reads it: the line by its snapshot and number, the snapshot's
// date, the line's value date and lag in days, its house bank's code (""
// for none), its currency and that currency's exponent, its source and
// certainty, whether it is a blocked supplier's, its item's code and name
// ("" for no item) and the signed amount in minor units.
type ForecastShare struct {
	SnapshotID    uuid.UUID
	LineNo        int32
	SnapshotDate  time.Time
	ValueDate     time.Time
	LagDays       int32
	HouseBankCode string
	Currency      string
	Exponent      int
	Source        string
	Certainty     string
	Blocked       bool
	Item          string
	ItemName      string
	Amount        money.Minor
}

// Cash reads a legal entity's cash actuals and forecast as the caller (the
// cash context's ActualShares and ForecastShares).
type Cash interface {
	ActualShares(ctx context.Context, legalEntity uuid.UUID) ([]CashShare, error)
	ForecastShares(ctx context.Context, legalEntity uuid.UUID) ([]ForecastShare, error)
}

// addCash adds the legal entity's cash facts, those the source builds when
// it has the cash context's port: one per share of its statement lines to
// cash_actuals, one per line of its snapshots to cash_snapshots.
func (s Source) addCash(ctx context.Context, b *factBuilder, le legalEntity) error {
	if s.Cash == nil || (!s.builds(cashActuals) && !s.builds(cashSnapshots)) {
		return nil
	}
	id, err := uuid.Parse(le.ID)
	if err != nil {
		return fmt.Errorf("analytics: legal entity %q: %w", le.ID, err)
	}
	if s.builds(cashActuals) {
		if err := s.addActuals(ctx, b, le, id); err != nil {
			return err
		}
	}
	if s.builds(cashSnapshots) {
		return s.addForecast(ctx, b, le, id)
	}
	return nil
}

// addActuals adds one fact per share of the legal entity's statement lines
// to cash_actuals.
func (s Source) addActuals(ctx context.Context, b *factBuilder, le legalEntity, id uuid.UUID) error {
	shares, err := s.Cash.ActualShares(ctx, id)
	if err != nil {
		return fmt.Errorf("analytics: cash actuals of %s: %w", le.Code, err)
	}
	for _, sh := range shares {
		if err := b.exponent(sh.Currency, sh.Exponent); err != nil {
			return fmt.Errorf("analytics: cash actuals of %s: %w", le.Code, err)
		}
		dims := map[string]string{"company": le.Code, "currency": sh.Currency, "house_bank": sh.HouseBankCode, "match": sh.Match, "liquidity_item": sh.Item,
			"liquidity_item_name": sh.ItemName}
		// A date always formats as one: no error to handle.
		_ = b.calendar(dims, "value_date", sh.ValueDate.Format(time.DateOnly))
		b.f.Tables[cashActuals] = append(b.f.Tables[cashActuals], Fact{ID: sh.StatementID.String(), Dims: dims, Amount: sh.Amount, Currency: sh.Currency})
	}
	return nil
}

// addForecast adds one fact per line of the legal entity's snapshots at
// document level to cash_snapshots, dated by its value date.
func (s Source) addForecast(ctx context.Context, b *factBuilder, le legalEntity, id uuid.UUID) error {
	shares, err := s.Cash.ForecastShares(ctx, id)
	if err != nil {
		return fmt.Errorf("analytics: cash forecast of %s: %w", le.Code, err)
	}
	for _, sh := range shares {
		if err := b.exponent(sh.Currency, sh.Exponent); err != nil {
			return fmt.Errorf("analytics: cash forecast of %s: %w", le.Code, err)
		}
		dims := map[string]string{"company": le.Code, "currency": sh.Currency, "lag_days": strconv.Itoa(int(sh.LagDays)), "house_bank": sh.HouseBankCode,
			"source": sh.Source, "certainty": sh.Certainty, "blocked": strconv.FormatBool(sh.Blocked), "liquidity_item": sh.Item, "liquidity_item_name": sh.ItemName}
		// A date always formats as one: no error to handle.
		_ = b.calendar(dims, "value_date", sh.ValueDate.Format(time.DateOnly))
		b.f.Tables[cashSnapshots] = append(b.f.Tables[cashSnapshots], Fact{ID: sh.SnapshotID.String(), Dims: dims, Amount: sh.Amount, Currency: sh.Currency})
	}
	return nil
}

// exponent records a currency's exponent as the cash context gives it, and
// refuses a second one for the same currency.
func (b *factBuilder) exponent(currency string, exp int) error {
	if was, seen := b.f.Exponents[currency]; seen && was != exp {
		return fmt.Errorf("%s has exponents %d and %d", currency, was, exp)
	}
	b.f.Exponents[currency] = exp
	return nil
}
