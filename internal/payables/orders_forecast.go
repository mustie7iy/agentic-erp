package payables

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// What the liquidity forecast reads of purchase orders (ADR-0051 §5,
// decision P7, T53g): the open value of the released orders, as the open
// items invoicing them would give, beside the unposted invoices (Unposted)
// and through the same port of the payments context, so the forecast pays
// them by the payment run's own rules. Payables reads them because it
// knows what is invoiced on an order item and calculates an invoice's tax;
// procurement gains no dependency.

// OpenOrders are the legal entity's released purchase orders' open values,
// each as the supplier open item invoicing it would give (ledger.PayableItem,
// without an item reference): per order and the day its invoice is
// expected, the items' open values (the ordered quantity less what its
// posted, parked and held documents invoice, net of credit memos, with
// what the source system invoiced before migration, never less than none
// nor more than ordered, at the item's price per price unit) plus the tax
// the supplier would charge on them, by their codes on the read date as an
// invoice's tax is calculated (codesDocument, tax.Calculate); a code that
// cannot be calculated then (not an input code of the legal entity, no
// rate, amounts out of range) adds no tax rather than refuse the
// forecast. The day is the item's delivery date, else the order date plus
// the company's cash.order_lead_days; the order's payment terms count from
// it (a term not configured: none, so its supplier's net days, as an
// unposted invoice's). Its lines are its items' accounts and open values
// with their tax codes, signed as an invoice posts them (T53e3). An order
// whose items are all invoiced adds nothing.
func (e *Endpoints) OpenOrders(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]ledger.PayableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := e.Orders.Released(ctx, legalEntity)
	if err != nil || len(orders) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}
	invoiced, err := quantitiesInvoiced(ctx, tx, ids, []string{Posted, Parked, OnHold}, uuid.Nil)
	if err != nil {
		return nil, fmt.Errorf("payables: open purchase orders of %s: %w", legalEntity, err)
	}
	r, err := e.openReader(ctx, legalEntity, on)
	if err != nil {
		return nil, err
	}
	var out []ledger.PayableItem
	for _, o := range orders {
		items, err := r.order(ctx, o, invoiced[o.ID])
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// openReader reads the open orders of one legal entity on the read date:
// its company's lead days and tax settings, and each tax code and payment
// term once however many orders name it.
type openReader struct {
	e        *Endpoints
	on       time.Time
	lead     int
	settings config.TaxSettings
	terms    map[string]*ledger.ItemTerms
}

func (e *Endpoints) openReader(ctx context.Context, legalEntity uuid.UUID, on time.Time) (openReader, error) {
	le, _, err := e.LegalEntities.Entity(ctx, legalEntity) // the forecast's legal entity exists
	if err != nil {
		return openReader{}, err
	}
	co, err := e.company(ctx, le.Code)
	if err != nil {
		return openReader{}, err
	}
	r := openReader{on: on, lead: config.DefaultOrderLeadDays, settings: settingsOf(co), terms: map[string]*ledger.ItemTerms{}}
	if co != nil {
		r.lead = co.Cash.OrderLead()
	}
	cached := *e
	cached.Accounts = &taxCodes{Accounts: e.Accounts, codes: map[string]cachedCode{}}
	r.e = &cached
	return r, nil
}

// taxCodes answers each tax code's question once per read.
type taxCodes struct {
	Accounts
	codes map[string]cachedCode
}

// cachedCode is a tax code as the ledger answered for it.
type cachedCode struct {
	code ledger.TaxCode
	ok   bool
}

func (c *taxCodes) TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error) {
	if t, ok := c.codes[code]; ok {
		return t.code, t.ok, nil
	}
	t, ok, err := c.Accounts.TaxCode(ctx, legalEntity, code, date)
	if err != nil {
		return ledger.TaxCode{}, false, err
	}
	c.codes[code] = cachedCode{t, ok}
	return t, ok, nil
}

// order is an order's open value as the items invoicing it would give, one
// per day its items' invoices are expected, earliest first.
func (r openReader) order(ctx context.Context, o procurement.Order, invoiced map[int32]string) ([]ledger.PayableItem, error) {
	byDay := map[time.Time]*Invoice{}
	var days []time.Time
	for _, it := range o.Items {
		open, err := openValue(it, invoiced[it.ItemNo])
		if err != nil {
			return nil, fmt.Errorf("payables: open value of item %d of purchase order %s: %w", it.ItemNo, o.ID, err)
		}
		if open == 0 {
			continue
		}
		day := o.OrderDate.AddDate(0, 0, r.lead)
		if it.DeliveryDate != nil {
			day = *it.DeliveryDate
		}
		v := byDay[day]
		if v == nil {
			v = &Invoice{ID: o.ID, LegalEntityID: o.LegalEntityID, Currency: o.Currency}
			byDay[day] = v
			days = append(days, day)
		}
		account := it.AccountID
		v.Items = append(v.Items, Item{Amount: open, AccountID: &account, TaxCode: it.TaxCode})
	}
	if len(days) == 0 {
		return nil, nil
	}
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })
	terms, err := r.term(ctx, o.PaymentTermCode)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.PayableItem, len(days))
	for i, day := range days {
		v := byDay[day]
		gross, err := r.gross(ctx, *v)
		if err != nil {
			return nil, err
		}
		out[i] = ledger.PayableItem{PartnerID: o.SupplierID, DocumentDate: day, Currency: o.Currency, Amount: -gross, Lines: itemLines(*v),
			Document: o.ID, Number: o.DocumentNumber}
		if terms != nil {
			t := *terms
			t.BaselineDate = day
			out[i].Terms = &t
		}
	}
	return out, nil
}

// openValue is what of an order item is not invoiced yet: its price per
// price unit times the quantity ordered less the quantity invoiced, the
// latter taken as none when its documents credit more than they invoice
// and the open quantity as none when they invoice more than ordered.
func openValue(it procurement.OrderItem, invoiced string) (money.Minor, error) {
	ordered, ok := new(big.Rat).SetString(it.Quantity)
	if !ok {
		return 0, fmt.Errorf("quantity %q", it.Quantity)
	}
	done := new(big.Rat)
	if invoiced != "" {
		if _, ok := done.SetString(invoiced); !ok {
			return 0, fmt.Errorf("quantity invoiced %q", invoiced)
		}
	}
	open := new(big.Rat).Sub(ordered, done)
	switch {
	case done.Sign() < 0:
		open = ordered
	case open.Sign() <= 0:
		return 0, nil
	}
	return money.Scale(it.UnitPrice, open.FloatString(6), int64(it.PriceUnit))
}

// term is the order's payment terms without their baseline, nil for a
// term not configured.
func (r openReader) term(ctx context.Context, code string) (*ledger.ItemTerms, error) {
	if t, ok := r.terms[code]; ok {
		return t, nil
	}
	term, ok, err := r.e.Suppliers.PaymentTerm(ctx, code)
	if err != nil {
		return nil, err
	}
	var t *ledger.ItemTerms
	if ok {
		t = &ledger.ItemTerms{PaymentTermCode: code, NetDays: term.NetDays}
		for _, d := range term.Discounts {
			t.Discounts = append(t.Discounts, ledger.Discount{Days: d.Days, Percent: d.Percent})
		}
	}
	r.terms[code] = t
	return t, nil
}

// gross is the open items' value plus the tax the supplier would charge on
// them on the read date, the value alone when it cannot be calculated
// (readCharged) or the sum is out of range.
func (r openReader) gross(ctx context.Context, v Invoice) (money.Minor, error) {
	amounts := make([]money.Minor, len(v.Items))
	for i, it := range v.Items {
		amounts[i] = it.Amount
	}
	// Each item's open value is at most its ordered value, and the order's
	// total of those is in range (procurement's checkTotal).
	net, _ := money.Sum(amounts...)
	coded := codedItems(v)
	if len(coded) == 0 {
		return net, nil
	}
	doc, unknown, err := r.e.codesDocument(ctx, v, r.on, coded, r.settings)
	if err != nil {
		return 0, fmt.Errorf("payables: tax of purchase order %s: %w", v.ID, err)
	}
	if unknown >= 0 {
		return net, nil
	}
	res, err := tax.Calculate(doc)
	charged, ok, err := readCharged(res.Items, err)
	if err != nil {
		return 0, fmt.Errorf("payables: tax of purchase order %s: %w", v.ID, err)
	}
	return withTax(net, charged, ok), nil
}

// withTax is the value plus the tax when there is one, the value alone
// when the sum is out of range.
func withTax(net, charged money.Minor, ok bool) money.Minor {
	if !ok {
		return net
	}
	gross, err := money.Sum(net, charged)
	if err != nil {
		return net
	}
	return gross
}

// InvoiceOrders are the purchase orders the supplier invoices among the
// ids were entered on, keyed by invoice; an invoice on no order is left
// out: by them the forecast variance knows a payment of an invoice as the
// order the snapshot forecast (T53g).
func (e *Endpoints) InvoiceOrders(ctx context.Context, invoices []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, purchase_order_id FROM payables.supplier_invoice WHERE id = ANY($1) AND purchase_order_id IS NOT NULL`, invoices)
	out := map[uuid.UUID]uuid.UUID{}
	var id, order uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&id, &order}, func() error { out[id] = order; return nil }); err != nil {
		return nil, fmt.Errorf("payables: purchase orders of %d invoices: %w", len(invoices), err)
	}
	return out, nil
}
