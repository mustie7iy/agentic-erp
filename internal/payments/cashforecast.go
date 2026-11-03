package payments

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// What a CashPayable is (ADR-0051 §4): a supplier open item, an invoice
// not posted yet as the item it would become (T53e2), or a down payment
// request (T53e2), which a run pays whole and never nets.
const (
	CashItem        = "item"
	CashUnposted    = "unposted"
	CashDownPayment = "down_payment"
)

// CashPayable is a supplier open item as the liquidity forecast reads it
// (ADR-0051 §5, T53e): the item, when it is due, whether its supplier is
// blocked, and the house bank and method a run would pay it through.
type CashPayable struct {
	Kind string // CashItem, CashUnposted or CashDownPayment
	ledger.PayableItem
	// Due is its net due date (ledger.PayableItem.DueDate): an invoice's by
	// the terms it was posted with, else its supplier's net days; a debit
	// (a credit memo) its document date, so it nets with the next run; a
	// down payment request's due date.
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

// Payables are what the legal entity's runs are expected to pay its
// suppliers (ADR-0051 §4, §5): the supplier open items no live run holds
// (a run's payment counts instead, Pending), the invoices not posted yet
// as the items they would become (CashUnposted, T53e2), and the open down
// payment requests no live run pays (CashDownPayment, T53e2); each with
// its due date, its supplier's block, and the bank and method a run on
// the date would route it through. What a run holds is the run's flow,
// never the document's as well (ADR-0051 §5).
func (c CashPosition) Payables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]CashPayable, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	items, err := c.documents(ctx, tx, legalEntity)
	if err != nil {
		return nil, err
	}
	var suppliers []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, it := range items {
		if !seen[it.PartnerID] {
			seen[it.PartnerID] = true
			suppliers = append(suppliers, it.PartnerID)
		}
	}
	payees, err := c.Payees.Payees(ctx, legalEntity, suppliers, on)
	if err != nil {
		return nil, err
	}
	routers := map[string]router{}
	out := make([]CashPayable, len(items))
	banks := make([]*uuid.UUID, len(items))
	for i, it := range items {
		rt, ok := routers[it.Currency]
		if !ok {
			if rt, err = rankedRouter(ctx, tx, legalEntity, it.Currency); err != nil {
				return nil, err
			}
			routers[it.Currency] = rt
		}
		payee, found := payees[it.PartnerID]
		v, _ := rt.route(payee) // not routed: no bank, no method
		it.Blocked, it.Method, banks[i] = blockedReason(payee, found), methodOf(v.method), v.bank
		if it.Kind != CashDownPayment {
			it.Due = it.DueDate(payee.NetDays)
		}
		out[i] = it
	}
	return withBankCodes(ctx, out, banks)
}

// documents are the legal entity's supplier open items no live run holds,
// its unposted invoices and its open down payment requests no live run
// pays, without their routing.
func (c CashPosition) documents(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) ([]CashPayable, error) {
	open, err := c.Owed.Owed(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	reserved, err := reservedItems(ctx, tx, legalEntity)
	if err != nil {
		return nil, err
	}
	var out []CashPayable
	for _, it := range open {
		if !reserved[it.ItemRef] {
			out = append(out, CashPayable{Kind: CashItem, PayableItem: it})
		}
	}
	unposted, err := c.Unposted.Unposted(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	for _, it := range unposted {
		out = append(out, CashPayable{Kind: CashUnposted, PayableItem: it})
	}
	requests, err := c.Requests.Open(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	held, err := heldRequests(ctx, tx, legalEntity)
	if err != nil {
		return nil, err
	}
	for _, r := range requests {
		if !slices.Contains(held, r.ID) {
			out = append(out, CashPayable{Kind: CashDownPayment, Due: r.DueDate,
				PayableItem: ledger.PayableItem{PartnerID: r.SupplierID, DocumentDate: r.DueDate, Currency: r.Currency, Amount: -r.Amount}})
		}
	}
	return out, nil
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
