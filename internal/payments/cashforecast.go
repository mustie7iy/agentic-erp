package payments

import (
	"context"
	"fmt"
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
	// PlanningGroup is its supplier's planning group in the legal entity,
	// "" for none (ADR-0051 §4, T53e3).
	PlanningGroup string
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
		it.Blocked, it.Method, banks[i], it.PlanningGroup = blockedReason(payee, found), methodOf(v.method), v.bank, payee.PlanningGroup
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
	if err := c.withLines(ctx, out); err != nil {
		return nil, err
	}
	unposted, err := c.Unposted.Unposted(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	if err := c.withNumbers(ctx, legalEntity, unposted); err != nil {
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

// withLines gives each posted item its entry's offsetting lines (T53e3).
func (c CashPosition) withLines(ctx context.Context, items []CashPayable) error {
	entries := make([]ledger.EntryRef, len(items))
	for i, it := range items {
		entries[i] = it.Entry()
	}
	lines, err := c.Owed.Offsetting(ctx, entries)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Lines = lines[items[i].Entry()]
	}
	return nil
}

// withNumbers gives the unposted invoices' lines their accounts' numbers,
// from the legal entity's chart, read only when there is a line; an
// account no longer active there keeps no number, which no account rule
// covers.
func (c CashPosition) withNumbers(ctx context.Context, legalEntity uuid.UUID, items []ledger.PayableItem) error {
	if !slices.ContainsFunc(items, func(it ledger.PayableItem) bool { return len(it.Lines) > 0 }) {
		return nil
	}
	chart, err := c.Accounts.Chart(ctx, legalEntity)
	if err != nil {
		return err
	}
	numbers := make(map[uuid.UUID]string, len(chart))
	for number, a := range chart {
		numbers[a.ID] = number
	}
	for _, it := range items {
		for j := range it.Lines {
			it.Lines[j].Account = numbers[it.Lines[j].AccountID]
		}
	}
	return nil
}

// CashPaying is what a payment of a run not yet released pays, as the
// liquidity forecast classifies it (ADR-0051 §4, T53e3): its supplier's
// planning group in the legal entity, and each open item it settles with
// what it pays of it and the item's offsetting lines. A down payment
// request's payment settles no item.
type CashPaying struct {
	PlanningGroup string
	Items         []CashPaid
}

// CashPaid is an open item a payment settles: what it pays of it, after
// its cash discount and withholding tax (negative for a credit memo it
// nets), and the item's offsetting lines.
type CashPaid struct {
	Amount money.Minor
	Lines  []ledger.OffsettingLine
}

// Paying is what each of the payments pays, keyed by payment; a payment
// not among the legal entity's is left out.
func (c CashPosition) Paying(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, on time.Time) (map[uuid.UUID]CashPaying, error) {
	all, err := paidItems(ctx, legalEntity, ids)
	if err != nil {
		return nil, err
	}
	var suppliers []uuid.UUID
	var entries []ledger.EntryRef
	for _, x := range all {
		suppliers = append(suppliers, x.supplier)
		if x.entry != nil {
			entries = append(entries, *x.entry)
		}
	}
	payees, err := c.Payees.Payees(ctx, legalEntity, suppliers, on)
	if err != nil {
		return nil, err
	}
	lines, err := c.Owed.Offsetting(ctx, entries)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]CashPaying{}
	for _, x := range all {
		pay := out[x.payment]
		pay.PlanningGroup = payees[x.supplier].PlanningGroup
		if x.entry != nil {
			pay.Items = append(pay.Items, CashPaid{Amount: x.amount, Lines: lines[*x.entry]})
		}
		out[x.payment] = pay
	}
	return out, nil
}

// cashSettled is an open item a payment settles, or a payment that settles none
// (entry nil).
type cashSettled struct {
	payment, supplier uuid.UUID
	entry             *ledger.EntryRef
	amount            money.Minor
}

// paidItems are the active items the legal entity's payments among the
// ids settle, with what each pays of its item, in order; a payment of no
// item once, without one.
func paidItems(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID) ([]cashSettled, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.supplier_id, i.item_fiscal_year, i.item_entry_id, i.amount - i.discount_amount - i.withholding_amount
		FROM payments.payment p JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		LEFT JOIN payments.payment_item i ON i.tenant_id = p.tenant_id AND i.payment_id = p.id AND i.is_active
		WHERE p.id = ANY($1) AND r.legal_entity_id = $2
		ORDER BY p.id, i.item_fiscal_year, i.item_entry_id, i.item_line_no`, ids, legalEntity)
	var all []cashSettled
	var p cashSettled
	var year *int16
	var entry *uuid.UUID
	var amount *money.Minor
	_, err = pgx.ForEachRow(rows, []any{&p.payment, &p.supplier, &year, &entry, &amount}, func() error {
		p.entry, p.amount = nil, 0
		if entry != nil {
			p.entry, p.amount = &ledger.EntryRef{FiscalYear: *year, EntryID: *entry}, *amount
		}
		all = append(all, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("payments: what the payments of runs not released of %s pay: %w", legalEntity, err)
	}
	return all, nil
}
