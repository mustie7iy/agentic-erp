package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A receipt in another currency than the items (ADR-0063 §6 As built,
// T75n; SAP F-28's clearing in another currency, from memory): each item
// in another currency than the receipt is translated into the receipt's
// at the average rate valid on the application's posting date, the
// ledger's rate source (rate: either direction, the reciprocal when only
// the reverse pair is recorded), rounded once half away from zero. The
// clearing entry is in the receipt's currency; it offsets the item at
// that amount and at the local and group amounts it was booked at, so the
// item is cleared at its own rate and what the entry leaves in local or
// group currency is the realized exchange difference applicationLines
// books. Residuals, the tolerance and what is left on account are then
// in the receipt's currency, as for an item in it.

// Translation is how an item in another currency than the receipt was
// translated: its own currency and amount (debit positive) and the rate,
// units of the receipt's currency per unit of the item's, as Conversion
// shows it. The zero value: the item is in the receipt's currency.
type Translation struct {
	Currency string
	Amount   money.Minor
	Rate     string
}

// translateItems translates the application's items (items[1:], items[0]
// being the receipt) that are in another currency than the receipt's in
// place, returning how each was translated, in the order of the items.
// Each item is checked as an item of the application first
// (checkCustomerItem), so an item that is no customer's item is refused as
// such before its rate is looked for. Conversions are read once per
// currency.
func translateItems(ctx context.Context, tx pgx.Tx, in ReceiptApplication, items []item, check func(int, item) error) ([]Translation, error) {
	out := make([]Translation, len(items)-1)
	conversions := map[string]Conversion{}
	for i := 1; i < len(items); i++ {
		it := &items[i]
		if err := check(i-1, *it); err != nil {
			return nil, err
		}
		if it.currency == in.Currency {
			continue
		}
		c, ok := conversions[it.currency]
		if !ok {
			var err error
			if c, err = itemConversion(ctx, tx, i-1, it.currency, in.Currency, in.PostingDate); err != nil {
				return nil, err
			}
			conversions[it.currency] = c
		}
		tc, err := c.Convert(it.tc)
		if err != nil {
			return nil, errTotalTooLarge(fmt.Sprintf("items[%d]", i-1))
		}
		if tc == 0 {
			return nil, problem.Unprocessable("ledger.item_rounds_to_zero", fmt.Sprintf("Item %d, %s in %s, is nothing in %s at the rate of %s.", i,
				it.tc.Plain(c.FromExp), it.currency, in.Currency, c.Rate), problem.FieldError{Field: fmt.Sprintf("items[%d]", i-1), Code: "rounds_to_zero",
				Message: "an item worth at least a minor unit of the receipt's currency"})
		}
		out[i-1] = Translation{Currency: it.currency, Amount: it.tc, Rate: c.Rate}
		it.tc, it.currency = tc, in.Currency
	}
	return out, nil
}

// itemConversion is the conversion of item i's currency into the
// receipt's on the posting date. Without an average rate valid that day
// the item is refused, 422 ledger.no_exchange_rate naming items[i]: the
// rate is the item's to have, not the posting date's.
func itemConversion(ctx context.Context, tx pgx.Tx, i int, from, to string, date time.Time) (Conversion, error) {
	x, err := rate(ctx, tx, from, to, date)
	var p *problem.Problem
	if errors.As(err, &p) && p.Code == "ledger.no_exchange_rate" {
		return Conversion{}, problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("Item %d is in %s, the receipt in %s, and no average exchange rate between the two is valid on %s: record the rate, "+
				"or apply the receipt on a day one is valid.", i+1, from, to, date.Format(time.DateOnly)),
			problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "no_exchange_rate", Message: "an item whose currency has a rate into " + to + " that day"})
	}
	if err != nil {
		return Conversion{}, err
	}
	return withExponents(ctx, tx, Conversion{From: from, To: to, Rate: x.shown, exact: x.exact})
}
