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
// the supplier would charge on them, per code on the read date as an
// invoice's tax is calculated (codesDocument, tax.Calculate); a code that
// cannot be calculated then (not an input code of the legal entity, no
// rate, amounts out of range) adds no tax rather than refuse the
// forecast. The order's down payments not yet cleared (open, forecast as
// down_payment; paid, out already) are taken off its earliest documents,
// since its invoice is cleared against them. The day is the item's delivery date, else the order date plus
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
	advances, err := orderAdvances(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("payables: open purchase orders of %s: %w", legalEntity, err)
	}
	r, err := e.openReader(ctx, legalEntity, on)
	if err != nil {
		return nil, err
	}
	var out []ledger.PayableItem
	for _, o := range orders {
		items, err := r.order(ctx, o, invoiced[o.ID], advances[orderCurrency{o.ID, o.Currency}])
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// orderCurrency is an order and a currency its down payments are in.
type orderCurrency struct {
	order    uuid.UUID
	currency string
}

// orderAdvances are the down payments requested on the orders and not yet
// cleared against an invoice (open: forecast as down_payment; paid: the
// cash went out, and the order's invoice is cleared against it), tax
// included, by order and currency: what of an order's open value its
// invoice will not pay again.
func orderAdvances(ctx context.Context, tx pgx.Tx, orders []uuid.UUID) (map[orderCurrency]money.Minor, error) {
	rows, _ := tx.Query(ctx, `SELECT purchase_order_id, currency, sum(amount)::bigint FROM payables.down_payment_request
		WHERE purchase_order_id = ANY($1) AND status IN ('open', 'paid') GROUP BY 1, 2`, orders)
	out := map[orderCurrency]money.Minor{}
	var k orderCurrency
	var amount money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&k.order, &k.currency, &amount}, func() error { out[k] = amount; return nil }); err != nil {
		return nil, fmt.Errorf("down payments of %d orders: %w", len(orders), err)
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
// per day its items' invoices are expected, earliest first, less the down
// payments on it not yet cleared (advance), taken from the earliest first
// and none left below nothing.
func (r openReader) order(ctx context.Context, o procurement.Order, invoiced map[int32]string, advance money.Minor) ([]ledger.PayableItem, error) {
	byDay, days, err := r.byDay(o, invoiced)
	if err != nil || len(days) == 0 {
		return nil, err
	}
	terms, err := r.term(ctx, o.PaymentTermCode)
	if err != nil {
		return nil, err
	}
	var out []ledger.PayableItem
	for _, day := range days {
		v := byDay[day]
		gross, err := r.gross(ctx, *v)
		if err != nil {
			return nil, err
		}
		taken := min(gross, advance)
		gross, advance = gross-taken, advance-taken
		if gross <= 0 {
			continue
		}
		it := ledger.PayableItem{PartnerID: o.SupplierID, DocumentDate: day, Currency: o.Currency, Amount: -gross, Lines: itemLines(*v),
			Document: o.ID, Number: o.DocumentNumber}
		if terms != nil {
			t := *terms
			t.BaselineDate = day
			it.Terms = &t
		}
		out = append(out, it)
	}
	return out, nil
}

// byDay are the order's open items grouped by the day their invoice is
// expected, as the documents invoicing them, and those days, earliest
// first.
func (r openReader) byDay(o procurement.Order, invoiced map[int32]string) (map[time.Time]*Invoice, []time.Time, error) {
	byDay := map[time.Time]*Invoice{}
	var days []time.Time
	for _, it := range o.Items {
		open, err := openValue(it, invoiced[it.ItemNo])
		if err != nil {
			return nil, nil, fmt.Errorf("payables: open value of item %d of purchase order %s: %w", it.ItemNo, o.ID, err)
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
			v = &Invoice{ID: o.ID, Kind: KindInvoice, LegalEntityID: o.LegalEntityID, Currency: o.Currency}
			byDay[day] = v
			days = append(days, day)
		}
		account := it.AccountID
		v.Items = append(v.Items, Item{Amount: open, AccountID: &account, TaxCode: it.TaxCode})
	}
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })
	return byDay, days, nil
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
// them on the read date, calculated per tax code as an invoice's is
// (codesDocument, tax.Calculate, which rounds each code's tax on its own):
// a code that cannot be calculated then (not an input code of the legal
// entity, no rate, amounts out of range: readCharged) adds no tax, the
// other codes theirs; the value alone when the sum is out of range.
func (r openReader) gross(ctx context.Context, v Invoice) (money.Minor, error) {
	amounts := make([]money.Minor, len(v.Items))
	byCode := map[string][]Item{}
	var codes []string
	for i, it := range v.Items {
		amounts[i] = it.Amount
		if it.TaxCode == nil {
			continue
		}
		if _, ok := byCode[*it.TaxCode]; !ok {
			codes = append(codes, *it.TaxCode)
		}
		byCode[*it.TaxCode] = append(byCode[*it.TaxCode], it)
	}
	// Each item's open value is at most its ordered value, and the order's
	// total of those is in range (procurement's checkTotal).
	gross, _ := money.Sum(amounts...)
	for _, code := range codes {
		one := v
		one.Items = byCode[code]
		charged, ok, err := r.codeTax(ctx, one)
		if err != nil {
			return 0, fmt.Errorf("payables: tax %s of purchase order %s: %w", code, v.ID, err)
		}
		gross = withTax(gross, charged, ok)
	}
	return gross, nil
}

// codeTax is the tax the supplier charges on the items of one tax code on
// the read date; ok = false when it cannot be calculated then.
func (r openReader) codeTax(ctx context.Context, v Invoice) (money.Minor, bool, error) {
	coded := make([]int, len(v.Items))
	for i := range coded {
		coded[i] = i
	}
	doc, unknown, err := r.e.codesDocument(ctx, v, r.on, coded, r.settings)
	if err != nil || unknown >= 0 {
		return 0, false, err
	}
	res, err := tax.Calculate(doc)
	return readCharged(res.Items, err)
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
