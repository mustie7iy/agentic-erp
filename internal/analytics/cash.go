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
// So are the accuracy's cells (T53j3), which compare the two: no API
// gives them.

const (
	cashActuals   = "cash_actuals"
	cashSnapshots = "cash_snapshots"
	cashAccuracy  = "cash_accuracy"
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

// AccuracyCell is the cash context's comparison of the actuals with the
// forecast of one value date at one lag (0, 7 or 30), in one currency and
// liquidity item, on a day compared (passed, with the company's snapshot
// of the lag before): the cell by its id, the item's code and name (""
// for no item), the actual and forecast amounts and the variance, the
// actual less the forecast, in minor units.
type AccuracyCell struct {
	ID        uuid.UUID
	ValueDate time.Time
	LagDays   int32
	Currency  string
	Exponent  int
	Item      string
	ItemName  string
	Actual    money.Minor
	Forecast  money.Minor
	Variance  money.Minor
}

// Cash reads a legal entity's cash actuals, forecast and the accuracy's
// cells as the caller (the cash context's ActualShares, ForecastShares
// and AccuracyCells).
type Cash interface {
	ActualShares(ctx context.Context, legalEntity uuid.UUID) ([]CashShare, error)
	ForecastShares(ctx context.Context, legalEntity uuid.UUID) ([]ForecastShare, error)
	AccuracyCells(ctx context.Context, legalEntity uuid.UUID) ([]AccuracyCell, error)
}

// addCash adds the legal entity's cash facts, those the source builds when
// it has the cash context's port: one per share of its statement lines to
// cash_actuals, one per line of its snapshots to cash_snapshots, three per
// cell of the accuracy to cash_accuracy.
func (s Source) addCash(ctx context.Context, b *factBuilder, le legalEntity) error {
	if s.Cash == nil || (!s.builds(cashActuals) && !s.builds(cashSnapshots) && !s.builds(cashAccuracy)) {
		return nil
	}
	id, err := uuid.Parse(le.ID)
	if err != nil {
		return fmt.Errorf("analytics: legal entity %q: %w", le.ID, err)
	}
	for _, add := range []struct {
		dataset string
		add     func(context.Context, *factBuilder, legalEntity, uuid.UUID) error
	}{{cashActuals, s.addActuals}, {cashSnapshots, s.addForecast}, {cashAccuracy, s.addAccuracy}} {
		if !s.builds(add.dataset) {
			continue
		}
		if err := add.add(ctx, b, le, id); err != nil {
			return err
		}
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
			"flow_source": sh.Source, "certainty": sh.Certainty, "blocked": strconv.FormatBool(sh.Blocked), "liquidity_item": sh.Item, "liquidity_item_name": sh.ItemName}
		// A date always formats as one: no error to handle.
		_ = b.calendar(dims, "value_date", sh.ValueDate.Format(time.DateOnly))
		b.f.Tables[cashSnapshots] = append(b.f.Tables[cashSnapshots], Fact{ID: sh.SnapshotID.String(), Dims: dims, Amount: sh.Amount, Currency: sh.Currency})
	}
	return nil
}

// The accuracy's figures, one row each per cell (dataset cash_accuracy):
// the signed variance, and the absolute variance and forecast WAPE adds up
// cell by cell (product owner 2026-11-04).
const (
	figureVariance         = "variance"
	figureAbsoluteVariance = "absolute_variance"
	figureAbsoluteForecast = "absolute_forecast"
)

// addAccuracy adds three facts per cell of the legal entity's accuracy to
// cash_accuracy, one per figure, dated by the cell's value date.
func (s Source) addAccuracy(ctx context.Context, b *factBuilder, le legalEntity, id uuid.UUID) error {
	cells, err := s.Cash.AccuracyCells(ctx, id)
	if err != nil {
		return fmt.Errorf("analytics: forecast accuracy of %s: %w", le.Code, err)
	}
	for _, c := range cells {
		if err := b.exponent(c.Currency, c.Exponent); err != nil {
			return fmt.Errorf("analytics: forecast accuracy of %s: %w", le.Code, err)
		}
		// The range of an amount is symmetric: every one has an absolute value.
		for _, f := range []struct {
			name   string
			amount money.Minor
		}{{figureVariance, c.Variance}, {figureAbsoluteVariance, c.Variance.Abs()}, {figureAbsoluteForecast, c.Forecast.Abs()}} {
			dims := map[string]string{"company": le.Code, "currency": c.Currency, "lag_days": strconv.Itoa(int(c.LagDays)), "figure": f.name,
				"liquidity_item": c.Item, "liquidity_item_name": c.ItemName}
			// A date always formats as one: no error to handle.
			_ = b.calendar(dims, "value_date", c.ValueDate.Format(time.DateOnly))
			b.f.Tables[cashAccuracy] = append(b.f.Tables[cashAccuracy], Fact{ID: c.ID.String(), Dims: dims, Amount: f.amount, Currency: c.Currency})
		}
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
