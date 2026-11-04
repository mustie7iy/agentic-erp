package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The cash actuals (ADR-0051 §6, §11; T53j1): each bank statement line's
// share of a liquidity item, by value date, classified by the cash
// context. The API pages a currency's lines over at most 36 months of
// value dates, which cannot cover a history of unknown length, so the
// database path reads the shares through a port the cash context
// implements (wired by the composition root, ADR-0044 As built T53j1), as
// the caller: the endpoint has required cash.forecast.read for the
// dataset, and the cash context refuses a legal entity the caller cannot
// act in, as its API does.

const cashActuals = "cash_actuals"

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

// CashActuals reads the cash actuals of a legal entity as the caller (the
// cash context's ActualShares).
type CashActuals interface {
	ActualShares(ctx context.Context, legalEntity uuid.UUID) ([]CashShare, error)
}

// addCash adds one fact per share of the legal entity's statement lines to
// cash_actuals, when the source builds it and has the cash context's port.
func (s Source) addCash(ctx context.Context, b *factBuilder, le legalEntity) error {
	if !s.builds(cashActuals) || s.Cash == nil {
		return nil
	}
	id, err := uuid.Parse(le.ID)
	if err != nil {
		return fmt.Errorf("analytics: legal entity %q: %w", le.ID, err)
	}
	shares, err := s.Cash.ActualShares(ctx, id)
	if err != nil {
		return fmt.Errorf("analytics: cash actuals of %s: %w", le.Code, err)
	}
	for _, sh := range shares {
		if exp, seen := b.f.Exponents[sh.Currency]; seen && exp != sh.Exponent {
			return fmt.Errorf("analytics: cash actuals of %s: %s has exponents %d and %d", le.Code, sh.Currency, exp, sh.Exponent)
		}
		b.f.Exponents[sh.Currency] = sh.Exponent
		dims := map[string]string{"company": le.Code, "currency": sh.Currency, "house_bank": sh.HouseBankCode, "match": sh.Match, "liquidity_item": sh.Item,
			"liquidity_item_name": sh.ItemName}
		// A date always formats as one: no error to handle.
		_ = b.calendar(dims, "value_date", sh.ValueDate.Format(time.DateOnly))
		b.f.Tables[cashActuals] = append(b.f.Tables[cashActuals], Fact{ID: sh.StatementID.String(), Dims: dims, Amount: sh.Amount, Currency: sh.Currency})
	}
	return nil
}
