package payments

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CashPayable is a supplier open item as the liquidity forecast reads it
// (ADR-0051 §5, T53e): the item, when it is due, whether its supplier is
// blocked, and the house bank and method a run would pay it through.
type CashPayable struct {
	ledger.PayableItem
	// Due is its net due date (ledger.PayableItem.DueDate): an invoice's by
	// the terms it was posted with, else its supplier's net days; a debit
	// (a credit memo) its document date, so it nets with the next run.
	Due time.Time
	// Blocked is why no run pays its supplier (SupplierBlocked or
	// PaymentBlocked), or "".
	Blocked string
	// HouseBankCode and Method are the house bank the first of its
	// supplier's payment methods ranks for the item's currency and that
	// method, as a run proposed without a house bank routes it; "" when
	// none fits.
	HouseBankCode, Method string
}

// Dates are the days a run would pay the item on, in the order the
// liquidity forecast tries them (ADR-0051 §5): an invoice's cash discount
// tiers' last days, earliest first, then its due date. A credit memo has
// its due date only: it nets with the next run, as the proposer takes it.
func (c CashPayable) Dates() []time.Time {
	var out []time.Time
	if c.Amount < 0 && c.Terms != nil {
		for _, d := range c.Terms.Discounts {
			out = append(out, c.Terms.Until(d))
		}
	}
	return append(out, c.Due)
}

// Pays is what paying the item on the date pays its supplier, positive
// for an invoice and negative for a credit memo it nets: what it owes less
// the cash discount it takes then and the withholding tax deducted, as a
// run on that date computes them (deductionsOn).
func (c CashPayable) Pays(on time.Time) (money.Minor, error) {
	owed := -c.Amount
	discount, withheld, err := deductionsOn(c.Terms, c.Withholding, owed, on)
	if err != nil {
		return 0, err
	}
	return money.Sum(owed, -discount, -withheld)
}

// Payables are the legal entity's supplier open items that no live run
// holds (a run's payment counts instead, Pending), each with its due date,
// its supplier's block, and the bank and method a run on the date would
// route it through. The items of a run are the run's flows, never the
// item's as well (ADR-0051 §5).
func (c CashPosition) Payables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]CashPayable, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	open, err := c.Owed.Owed(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	reserved, err := reservedItems(ctx, tx, legalEntity)
	if err != nil {
		return nil, err
	}
	var suppliers []uuid.UUID
	for i, it := range open {
		if i == 0 || it.PartnerID != open[i-1].PartnerID { // ordered by partner
			suppliers = append(suppliers, it.PartnerID)
		}
	}
	payees, err := c.Payees.Payees(ctx, legalEntity, suppliers, on)
	if err != nil {
		return nil, err
	}
	routers := map[string]router{}
	var out []CashPayable
	var banks []*uuid.UUID
	for _, it := range open {
		if reserved[it.ItemRef] {
			continue
		}
		rt, ok := routers[it.Currency]
		if !ok {
			if rt, err = rankedRouter(ctx, tx, legalEntity, it.Currency); err != nil {
				return nil, err
			}
			routers[it.Currency] = rt
		}
		payee, found := payees[it.PartnerID]
		v, _ := rt.route(payee) // not routed: no bank, no method
		out, banks = append(out, CashPayable{PayableItem: it, Due: it.DueDate(payee.NetDays), Blocked: blockedReason(payee, found),
			Method: methodOf(v.method)}), append(banks, v.bank)
	}
	return withBankCodes(ctx, out, banks)
}

// withBankCodes gives each payable the code of its bank, nil for none.
func withBankCodes(ctx context.Context, out []CashPayable, banks []*uuid.UUID) ([]CashPayable, error) {
	var ids []uuid.UUID
	for _, b := range banks {
		if b != nil {
			ids = append(ids, *b)
		}
	}
	codes, err := houseBankCodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, b := range banks {
		if b != nil {
			out[i].HouseBankCode = codes[*b]
		}
	}
	return out, nil
}
