package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tax"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The credit block of a sales order (ADR-0066 and ADR-0067, As built
// T77b): none for a draft, which is not checked, and for an order
// confirmed within its customer's credit limit or for a customer without
// one; blocked for an order confirmed over the limit, which takes no
// delivery; released once a credit manager released the block.
const (
	NoBlock  = "none"
	Blocked  = "blocked"
	Released = "released"
)

// CreditCheck is the credit context's answer about an amount in a
// company's currency: whether the customer has a limit there, the limit,
// the customer's exposure without the amount (its open items and its
// confirmed orders not yet billed), and whether the exposure and the
// amount together are within the limit. Without a limit the amount is
// within it and the other two are zero.
type CreditCheck struct {
	HasLimit bool
	Limit    money.Minor
	Exposure money.Minor
	Within   bool
}

// Credit checks an amount in the company's currency against a customer's
// credit limit there (credit context, ADR-0066). Credit is a leaf, which
// nothing imports: the port is declared here and the composition root
// answers it from credit.Endpoints.Check. The check holds the customer's
// limit until the request's transaction ends, so two orders of one
// customer are checked one after the other.
type Credit interface {
	Check(ctx context.Context, legalEntity, customer uuid.UUID, amount money.Minor) (CreditCheck, error)
}

// Rates converts an order in another currency into the company's, at the
// average rate valid on a date (ledger context).
type Rates interface {
	Conversion(ctx context.Context, from, to string, date time.Time) (ledger.Conversion, error)
}

// openLine is what is open of an order item for the customer's credit:
// its net value not yet posted as a customer invoice and the output tax
// code it will be billed with.
type openLine struct {
	ItemNo  int32       `json:"item_no"`
	Net     money.Minor `json:"net_amount"`
	TaxCode *string     `json:"tax_code"`
}

// openOrder is an order as its value is calculated: its number, the date
// its tax and its rate are taken on, its currency and its open lines.
type openOrder struct {
	Number   string
	Date     time.Time
	Currency string
	Lines    []openLine
}

// openOrders reads a customer's confirmed orders of a company that are
// not blocked for credit, each with what is open of its items: an item's
// net value less what posted billing documents billed of it. No billing
// document is posted before T79b, which moves one from `created` to
// `posted`; from then on the part posted is the customer's open item and
// is no longer counted here, so nothing counts twice. Orders with nothing
// open are left out. By order date, then id.
const openOrders = `SELECT o.document_number, o.order_date, o.currency, x.lines
	FROM sales.sales_order o CROSS JOIN LATERAL (
		SELECT json_agg(json_build_object('item_no', t.item_no, 'net_amount', t.net_amount - p.posted, 'tax_code', t.tax_code) ORDER BY t.item_no) AS lines
		FROM sales.sales_order_item t CROSS JOIN LATERAL (
			SELECT coalesce(sum(i.net_amount), 0)::bigint AS posted
			FROM sales.billing_document_item i JOIN sales.billing_document b ON b.tenant_id = i.tenant_id AND b.id = i.billing_document_id
			WHERE i.tenant_id = t.tenant_id AND i.sales_order_id = t.sales_order_id AND i.order_item_no = t.item_no AND b.status = 'posted') p
		WHERE t.tenant_id = o.tenant_id AND t.sales_order_id = o.id AND t.net_amount > p.posted) x
	WHERE o.legal_entity_id = $1 AND o.customer_id = $2 AND o.status = 'confirmed' AND o.credit_block <> 'blocked' AND x.lines IS NOT NULL
	ORDER BY o.order_date, o.id`

// OpenOrders is the sales context's published read for the customer's
// credit exposure (ADR-0066 §3, As built T77b): how many confirmed
// orders of the company the customer has with a part not yet billed,
// those blocked for credit left out, and what those parts come to in the
// company's currency, each at its net value plus its output tax
// (valuation). Company access is the caller's to check. An order whose
// value cannot be calculated refuses the read; it is never left out.
func (e *Endpoints) OpenOrders(ctx context.Context, legalEntity, customer uuid.UUID) (int32, money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, 0, err
	}
	rows, _ := tx.Query(ctx, openOrders, legalEntity, customer)
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (openOrder, error) {
		var o openOrder
		return o, r.Scan(&o.Number, &o.Date, &o.Currency, &o.Lines)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("sales: read the open orders of customer %s in legal entity %s: %w", customer, legalEntity, err)
	}
	if len(orders) == 0 {
		return 0, 0, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, legalEntity) // a stored order's legal entity exists
	if err != nil {
		return 0, 0, fmt.Errorf("sales: company of the open orders of customer %s: %w", customer, err)
	}
	v, err := e.valuation(ctx, le)
	if err != nil {
		return 0, 0, err
	}
	values := make([]money.Minor, len(orders))
	for i, o := range orders {
		if _, values[i], err = v.value(ctx, o); err != nil {
			return 0, 0, err
		}
	}
	total, err := money.Sum(values...)
	if err != nil { // money.Sum's only error is the range
		return 0, 0, problem.Unprocessable("sales.amount_too_large", "The customer's open orders add up to more than an amount can hold.")
	}
	return int32(len(orders)), total, nil
}

// valuation values the orders of one company for the customer's credit:
// an order's net value plus its output tax, in the company's currency.
// The tax is the calculator's (internal/tax) on the order's date over
// the items that carry a code, rounded as the company's tax settings say,
// which is how receivables calculates an invoice's (ADR-0063 §4); the
// order's date stands in for the invoice date billing will have. Each
// tax code is read once per date.
type valuation struct {
	e        *Endpoints
	le       tenancy.Entity
	rounding tax.Rounding
	codes    map[string]ledger.TaxCode
}

func (e *Endpoints) valuation(ctx context.Context, le tenancy.Entity) (*valuation, error) {
	settings, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: tax settings of company %s: %w", le.Code, err)
	}
	v := &valuation{e: e, le: le, codes: map[string]ledger.TaxCode{}}
	// A company the configuration lacks rounds by the default, per document.
	if co := settings.Company(le.Code); co != nil {
		v.rounding = tax.Rounding(co.TaxSettings.Rounding)
	}
	return v, nil
}

// value is the order's gross value, its lines' net values plus their
// output tax, in the order's currency and in the company's: the same
// amount for an order in the company's currency, else converted at the
// average rate valid on the order's date, rounded once. A value too
// large for an amount is refused (422 sales.amount_too_large).
func (v *valuation) value(ctx context.Context, o openOrder) (gross, inCompany money.Minor, err error) {
	if gross, err = v.gross(ctx, o); err == nil {
		inCompany, err = v.convert(ctx, o, gross)
	}
	if errors.Is(err, money.ErrOverflow) {
		return 0, 0, problem.Unprocessable("sales.amount_too_large", "The value of sales order "+o.Number+" with its tax is too large.")
	}
	return gross, inCompany, err
}

// gross adds the lines' output tax to their net values, exactly.
func (v *valuation) gross(ctx context.Context, o openOrder) (money.Minor, error) {
	doc := tax.Document{Company: v.le.Code, TaxDate: o.Date, Currency: o.Currency, Codes: map[string]tax.Code{}, Rounding: v.rounding}
	amounts := make([]money.Minor, 0, 2*len(o.Lines))
	for _, l := range o.Lines {
		amounts = append(amounts, l.Net)
		if l.TaxCode == nil {
			continue // an item without a code bears no tax
		}
		t, err := v.code(ctx, o, l)
		if err != nil {
			return 0, err
		}
		doc.Codes[t.Code] = t.Calculable()
		doc.Lines = append(doc.Lines, tax.Line{Amount: l.Net, Code: t.Code})
	}
	if len(doc.Lines) > 0 {
		res, err := tax.Calculate(doc)
		if err != nil { // the codes were read on the date with their rates: only the range is expected
			return 0, fmt.Errorf("sales: output tax of sales order %s on %s: %w", o.Number, o.Date.Format(time.DateOnly), err)
		}
		for _, it := range res.Items {
			amounts = append(amounts, it.Tax)
		}
	}
	sum, err := money.Sum(amounts...)
	if err != nil {
		return 0, fmt.Errorf("sales: gross value of sales order %s: %w", o.Number, err)
	}
	return sum, nil
}

// code reads the line's tax code on the order's date: an output tax code
// of the company with a rate in force then, as the order's creation
// required. One that no longer is refuses the valuation (422
// sales.unknown_tax_code naming the order and the item): the order's
// value is not guessed.
func (v *valuation) code(ctx context.Context, o openOrder, l openLine) (ledger.TaxCode, error) {
	day := o.Date.Format(time.DateOnly)
	key := *l.TaxCode + " " + day
	if t, ok := v.codes[key]; ok {
		return t, nil
	}
	t, ok, err := v.e.Accounts.TaxCode(ctx, v.le.ID, *l.TaxCode, o.Date)
	if err != nil {
		return ledger.TaxCode{}, fmt.Errorf("sales: tax code %s of sales order %s: %w", *l.TaxCode, o.Number, err)
	}
	if !ok || t.Direction != config.TaxOutput {
		return ledger.TaxCode{}, problem.Unprocessable("sales.unknown_tax_code", fmt.Sprintf(
			"Tax code %s of item %d of sales order %s is not an output tax code of the legal entity on the order's date %s; the order's value with its tax cannot be calculated.",
			*l.TaxCode, l.ItemNo, o.Number, day))
	}
	v.codes[key] = t
	return t, nil
}

// convert is the amount in the company's currency.
func (v *valuation) convert(ctx context.Context, o openOrder, amount money.Minor) (money.Minor, error) {
	if o.Currency == v.le.LocalCurrency {
		return amount, nil
	}
	c, err := v.e.Rates.Conversion(ctx, o.Currency, v.le.LocalCurrency, o.Date)
	if err != nil {
		return 0, fmt.Errorf("sales: value of sales order %s in %s: %w", o.Number, v.le.LocalCurrency, err)
	}
	return c.Convert(amount)
}

// checked is the credit check of an order at its confirmation: the block
// it gives, the order's gross value in its own currency and in the
// company's, whose minor-unit digits are exp, and credit's answer.
type checked struct {
	block  string
	gross  money.Minor
	amount money.Minor
	exp    int
	answer CreditCheck
}

// creditCheck checks an order being confirmed against its customer's
// credit limit (ADR-0066, As built T77b): the order's gross value in the
// company's currency with the customer's exposure, which does not hold
// this order yet, since it is still a draft. An order that is not within
// the limit is blocked; a customer without a limit is never blocked.
func (e *Endpoints) creditCheck(ctx context.Context, tx pgx.Tx, o Order, le tenancy.Entity) (checked, error) {
	v, err := e.valuation(ctx, le)
	if err != nil {
		return checked{}, err
	}
	lines := make([]openLine, len(o.Items))
	for i, it := range o.Items {
		lines[i] = openLine{ItemNo: it.ItemNo, Net: it.Net, TaxCode: it.TaxCode}
	}
	c := checked{block: NoBlock}
	if c.gross, c.amount, err = v.value(ctx, openOrder{Number: o.DocumentNumber, Date: o.OrderDate, Currency: o.Currency, Lines: lines}); err != nil {
		return checked{}, err
	}
	if c.answer, err = e.Credit.Check(ctx, le.ID, o.CustomerID, c.amount); err != nil {
		return checked{}, fmt.Errorf("sales: credit check of sales order %s: %w", o.DocumentNumber, err)
	}
	if !c.answer.Within {
		c.block = Blocked
	}
	if c.exp, err = currency.Exponent(ctx, tx, le.LocalCurrency, nil); err != nil { // a legal entity's currency exists
		return checked{}, fmt.Errorf("sales: currency of company %s: %w", le.Code, err)
	}
	return c, nil
}

// blockedEvent is SalesOrderCreditBlocked for the confirmed order: its
// gross value, the amount checked, and the limit and the exposure it was
// checked against, the last three in the company's currency.
func (c checked) blockedEvent(o Order, le tenancy.Entity, customerNumber string) outbox.Event {
	local := func(m money.Minor) map[string]string { return money.EventAmount(m, le.LocalCurrency, c.exp) }
	return outbox.Event{Context: "sales", Name: "sales_order_credit_blocked", AggregateType: "sales.sales_order", AggregateID: o.ID,
		Payload: map[string]any{"sales_order_id": o.ID, "document_number": o.DocumentNumber, "legal_entity_id": o.LegalEntityID,
			"legal_entity_code": le.Code, "customer_id": o.CustomerID, "customer_number": customerNumber,
			"gross_amount": money.EventAmount(c.gross, o.Currency, o.Exponent), "checked_amount": local(c.amount),
			"limit": local(c.answer.Limit), "exposure": local(c.answer.Exposure), "version": o.Version}}
}

// detail is what the confirmation's audit record says of the check: the
// amount checked, and the limit and the exposure when the customer has a
// limit.
func (c checked) detail(le tenancy.Entity) map[string]any {
	d := map[string]any{"amount": money.EventAmount(c.amount, le.LocalCurrency, c.exp), "has_limit": c.answer.HasLimit}
	if c.answer.HasLimit {
		d["limit"] = money.EventAmount(c.answer.Limit, le.LocalCurrency, c.exp)
		d["exposure"] = money.EventAmount(c.answer.Exposure, le.LocalCurrency, c.exp)
	}
	return d
}

// ReleaseCreditBlock releases the credit block of an order confirmed
// over its customer's limit (ADR-0066, As built T77b), once. The order's
// row is read FOR UPDATE and only a blocked order is released (ADR-0008):
// a second release waits for the first and is then refused, 409
// sales.order_not_credit_blocked. Who released it and when, the audit
// record and SalesOrderCreditReleased are written in the request's
// transaction. Nothing is checked again: the release is the credit
// manager's decision, whatever the amount.
func (e *Endpoints) ReleaseCreditBlock(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Order, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, err
	}
	o, err := order(ctx, tx, id, true)
	if err != nil {
		return Order{}, err
	}
	if err := ifMatch(o.Version); err != nil {
		return Order{}, err
	}
	if o.CreditBlock != Blocked {
		return Order{}, problem.Conflict("sales.order_not_credit_blocked",
			"Sales order "+o.DocumentNumber+" is not blocked for credit (its credit block is "+o.CreditBlock+"); only a blocked order is released.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Order{}, fmt.Errorf("sales: company of sales order %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Order{}, fmt.Errorf("sales: customer of sales order %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order SET credit_block = $2, credit_released_by = $3, credit_released_at = $4, version = version + 1,
			updated_by = $3 WHERE id = $1 AND credit_block = $5`, id, Released, p.ID, clock.Now(e.Now), Blocked); err != nil {
		return Order{}, fmt.Errorf("sales: release the credit block of sales order %s: %w", id, err)
	}
	released, err := loadOrder(ctx, tx, id, false)
	if err != nil {
		return Order{}, err
	}
	at := released.CreditReleasedAt.UTC().Format(time.RFC3339Nano)
	payload := map[string]any{"sales_order_id": released.ID, "document_number": released.DocumentNumber, "legal_entity_id": released.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": released.CustomerID, "customer_number": names[o.CustomerID].Number,
		"released_by": p.ID, "released_at": at, "version": released.Version}
	detail := map[string]any{"document_number": released.DocumentNumber, "customer_id": released.CustomerID, "credit_block": Released,
		"credit_block_before": Blocked, "released_at": at}
	if err := audit.Record(ctx, "sales.sales_order.release_credit_block", "sales.sales_order", id, detail,
		outbox.Event{Context: "sales", Name: "sales_order_credit_released", AggregateType: "sales.sales_order", AggregateID: id, Payload: payload}); err != nil {
		return Order{}, fmt.Errorf("sales: record the release of the credit block of sales order %s: %w", id, err)
	}
	return released, nil
}
