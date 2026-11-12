package sales

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Created is the status of a billing document until it is posted (T79b;
// ADR-0067 §9).
const Created = "created"

// BillingRange is the document type billing documents draw their numbers
// from (configuration number_ranges).
const BillingRange = "billing_document"

// BillingItem is an item of a billing document: the quantity billed of
// the order item OrderItemNo and its amount, with what the order item
// names and its price, account and tax code.
type BillingItem struct {
	Item
	OrderItemNo int32 `json:"order_item_no"`
}

// BillingDocument is a row of sales.billing_document with its items: what
// is billed of one sales order (ADR-0067 §9). Amounts are minor units of
// Currency, whose minor-unit digits are Exponent; Net is the sum of the
// items' amounts. OrderNumber and PaymentTermCode are the order's.
type BillingDocument struct {
	ID              uuid.UUID
	DocumentNumber  string
	LegalEntityID   uuid.UUID
	CustomerID      uuid.UUID
	OrderID         uuid.UUID
	OrderNumber     string
	PaymentTermCode string
	BillingDate     time.Time
	Currency        string
	Exponent        int
	Net             money.Minor
	Status          string
	Items           []BillingItem
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

// BillingInput is a billing document as a client sends it: the order to
// bill and, optionally, the billing date.
type BillingInput struct {
	OrderID uuid.UUID
	Date    *time.Time
}

// billingSelect names the order by subqueries, not a join, for the
// reason orderSelect names the quotation by one. An item reads what it
// names and its price from the order's item, which nothing changes.
const billingSelect = `SELECT b.id, b.document_number, b.legal_entity_id, b.customer_id, b.sales_order_id,
		(SELECT o.document_number FROM sales.sales_order o WHERE o.tenant_id = b.tenant_id AND o.id = b.sales_order_id),
		(SELECT o.payment_term_code FROM sales.sales_order o WHERE o.tenant_id = b.tenant_id AND o.id = b.sales_order_id),
		b.billing_date, b.currency, c.exponent, b.net_amount, b.status,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'item_key', t.item_key, 'description', t.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', t.uom_code, 'unit_price', t.unit_price, 'price_unit', t.price_unit,
			'net_amount', i.net_amount, 'account_id', t.account_id, 'tax_code', t.tax_code, 'order_item_no', i.order_item_no) ORDER BY i.item_no)
		 FROM sales.billing_document_item i JOIN sales.sales_order_item t ON t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id
			AND t.item_no = i.order_item_no
		 WHERE i.tenant_id = b.tenant_id AND i.billing_document_id = b.id),
		b.version, b.created_at, b.created_by, b.updated_at, b.updated_by
	FROM sales.billing_document b JOIN ref.currency c ON c.code = b.currency`

func scanBilling(row pgx.Row) (BillingDocument, error) {
	var b BillingDocument
	err := row.Scan(&b.ID, &b.DocumentNumber, &b.LegalEntityID, &b.CustomerID, &b.OrderID, &b.OrderNumber, &b.PaymentTermCode, &b.BillingDate,
		&b.Currency, &b.Exponent, &b.Net, &b.Status, &b.Items, &b.Version, &b.CreatedAt, &b.CreatedBy, &b.UpdatedAt, &b.UpdatedBy)
	return b, errs.Wrapf(err, "scan billing document")
}

// GetBilling returns a billing document of a legal entity the caller may
// act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetBilling(ctx context.Context, id uuid.UUID) (BillingDocument, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return BillingDocument{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: company access for billing document %s: %w", id, err)
	}
	b, err := scanBilling(tx.QueryRow(ctx, billingSelect+` WHERE b.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !access.Allows(b.LegalEntityID) {
		return BillingDocument{}, problem.NotFoundOf("billing document")
	}
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: read billing document %s: %w", id, err)
	}
	return b, nil
}

// CreateBilling creates a billing document for a sales order (ADR-0067
// §9): each of its items bills what is delivered of an order item and
// not yet billed, at the order's price (billable). The order's row is
// read FOR UPDATE (ADR-0008), so two requests for one order, or one and
// a delivery, wait for one another and no quantity is billed twice. An
// order the caller cannot read is a wrong reference (422 naming
// sales_order_id); one with nothing to bill is refused (422
// sales.nothing_to_bill), as is one whose item can no longer be billed
// (billableOn). The document is created `created`, dated the billing
// date given, else the company's date today, numbered from the company's
// billing_document range of that date's fiscal year. The database adds
// each quantity to the order item's billed quantity and moves the
// order's version (migration 000293). The audit record and
// BillingDocumentChanged are written in the request's transaction.
// Nothing is posted: posting is T79b.
func (e *Endpoints) CreateBilling(ctx context.Context, in BillingInput) (BillingDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return BillingDocument{}, err
	}
	o, err := order(ctx, tx, in.OrderID, true)
	if err != nil {
		return BillingDocument{}, problem.Reference(err, "sales_order_id", "sales order", "no sales order with this id that you can access")
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: company of sales order %s: %w", o.ID, err)
	}
	date, err := e.billingDate(ctx, le.Code, in.Date)
	if err != nil {
		return BillingDocument{}, err
	}
	items, err := billable(ctx, tx, o)
	if err != nil {
		return BillingDocument{}, err
	}
	if err := e.billableOn(ctx, o, items, date); err != nil {
		return BillingDocument{}, err
	}
	number, err := e.number(ctx, le.ID, BillingRange, date)
	if err != nil {
		return BillingDocument{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: customer of sales order %s: %w", o.ID, err)
	}
	b, err := insertBilling(ctx, tx, p, o, number, date, items)
	if err != nil {
		return BillingDocument{}, err
	}
	return b, recordBilling(ctx, b, le, names[o.CustomerID].Number)
}

// billingDate is the billing date given, else the company's date today
// in its time zone (product owner 2026-10-31).
func (e *Endpoints) billingDate(ctx context.Context, company string, given *time.Time) (time.Time, error) {
	if given != nil {
		return *given, nil
	}
	day, err := e.today(ctx, company)
	if err != nil {
		return time.Time{}, err
	}
	return day.Date, nil
}

// billable are the items a billing document of the order takes now, by
// the order's item number, numbered from 1: for each order item, the
// quantity delivered less the quantity earlier documents billed, when
// there is one. An item's amount is the order item's net value × the
// quantity billed so far, this document included, ÷ the quantity
// ordered, rounded half away from zero, less what earlier documents
// billed of it: the documents of a fully billed item add up to its net
// value to the minor unit, the last taking the remainder. A quantity
// whose amount rounds to zero is left for a later document, so every
// item of a document has an amount. An order with nothing to bill is
// refused (422 sales.nothing_to_bill naming sales_order_id). The order
// is read under its row lock.
func billable(ctx context.Context, tx pgx.Tx, o Order) ([]BillingItem, error) {
	earlier, err := billedAmounts(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	var items []BillingItem
	for _, it := range o.Items {
		delivered := money.ParseRat(it.Delivered)
		open := new(big.Rat).Sub(delivered, money.ParseRat(it.Billed))
		share := new(big.Rat).Quo(delivered, money.ParseRat(it.Quantity))
		upTo, _ := money.Round(share.Mul(share, new(big.Rat).SetInt64(int64(it.Net))), 0) // delivered is at most ordered (sales_order_item_quantities), so this is at most the item's net value, which is in range
		amount := upTo - earlier[it.ItemNo]
		if open.Sign() <= 0 || amount <= 0 {
			continue
		}
		billed := it.Item
		billed.ItemNo, billed.Quantity, billed.Net = int32(len(items)+1), money.PlainQuantity(open), amount
		items = append(items, BillingItem{Item: billed, OrderItemNo: it.ItemNo})
	}
	if len(items) == 0 {
		return nil, problem.Invalid("sales.nothing_to_bill", "sales_order_id", "Sales order "+o.DocumentNumber+
			" has nothing to bill: a billing document bills what is delivered of its items and not yet billed.")
	}
	return items, nil
}

// billedAmounts are the amounts earlier billing documents billed of each
// of the order's items, by the order's item number.
func billedAmounts(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) (map[int32]money.Minor, error) {
	rows, _ := tx.Query(ctx, `SELECT order_item_no, sum(net_amount)::bigint FROM sales.billing_document_item
		WHERE sales_order_id = $1 GROUP BY order_item_no ORDER BY order_item_no`, orderID)
	out := map[int32]money.Minor{}
	var no int32
	var amount money.Minor
	_, err := pgx.ForEachRow(rows, []any{&no, &amount}, func() error { out[no] = amount; return nil })
	if err != nil {
		return nil, fmt.Errorf("sales: read what is billed of sales order %s: %w", orderID, err)
	}
	return out, nil
}

// billCheck refuses a billing document one of whose items can no longer
// be billed on the billing date (422 sales.order_item_not_billable naming
// sales_order_id): its account can no longer be posted in the company,
// or its tax code is no output tax code of the company with a rate on
// that date. An order's items were checked when they were entered, on
// the order's or the quotation's date, and are not changed in the first
// version, so such an order cannot be billed on that date; the document
// is refused here rather than created and never posted. The ledger is
// asked about each account and tax code once.
type billCheck struct {
	e        *Endpoints
	o        Order
	date     time.Time
	accounts map[uuid.UUID]bool // postable in the company
	codes    map[string]bool    // output tax codes in force on the billing date
}

// billableOn checks the items of a billing document of the order dated
// date (billCheck).
func (e *Endpoints) billableOn(ctx context.Context, o Order, items []BillingItem, date time.Time) error {
	c := billCheck{e: e, o: o, date: date, accounts: map[uuid.UUID]bool{}, codes: map[string]bool{}}
	for _, it := range items {
		if err := c.account(ctx, it); err != nil {
			return err
		}
		if err := c.taxCode(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

func (c billCheck) refuse(it BillingItem, why string) error {
	return problem.Invalid("sales.order_item_not_billable", "sales_order_id", fmt.Sprintf(
		"Item %d of sales order %s cannot be billed on %s: %s.", it.OrderItemNo, c.o.DocumentNumber, c.date.Format(time.DateOnly), why))
}

// account requires the item's account to be postable in the company
// still.
func (c billCheck) account(ctx context.Context, it BillingItem) error {
	if c.accounts[it.AccountID] {
		return nil
	}
	ok, err := c.e.Accounts.IsPostable(ctx, c.o.LegalEntityID, it.AccountID)
	if err != nil {
		return fmt.Errorf("sales: account of item %d of sales order %s: %w", it.OrderItemNo, c.o.ID, err)
	}
	if !ok {
		return c.refuse(it, "its account can no longer be posted in the legal entity")
	}
	c.accounts[it.AccountID] = true
	return nil
}

// taxCode requires the item's tax code, when it has one, to be an output
// tax code of the company with a rate in force on the billing date, which
// is the tax date (ADR-0063 §4).
func (c billCheck) taxCode(ctx context.Context, it BillingItem) error {
	if it.TaxCode == nil || c.codes[*it.TaxCode] {
		return nil
	}
	t, ok, err := c.e.Accounts.TaxCode(ctx, c.o.LegalEntityID, *it.TaxCode, c.date)
	if err != nil {
		return fmt.Errorf("sales: tax code %s of item %d of sales order %s: %w", *it.TaxCode, it.OrderItemNo, c.o.ID, err)
	}
	if !ok || t.Direction != config.TaxOutput {
		return c.refuse(it, "its tax code "+*it.TaxCode+" is not an output tax code of the legal entity with a rate on that date")
	}
	c.codes[*it.TaxCode] = true
	return nil
}

// insertBilling writes the billing document with its items and returns
// it as it reads. Its net value is the sum of its items' amounts, at most
// the order's.
func insertBilling(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o Order, number string, date time.Time, items []BillingItem) (BillingDocument, error) {
	id := uuid.Must(uuid.NewV7())
	n := len(items)
	nos, orderNos, quantities, amounts := make([]int32, n), make([]int32, n), make([]string, n), make([]money.Minor, n)
	for i, it := range items {
		nos[i], orderNos[i], quantities[i], amounts[i] = it.ItemNo, it.OrderItemNo, it.Quantity, it.Net
	}
	net, _ := money.Sum(amounts...) // at most the order's net value, which is in range
	if _, err := tx.Exec(ctx, `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id,
			billing_date, currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		p.TenantID, id, number, o.LegalEntityID, o.CustomerID, o.ID, date, o.Currency, net, Created, p.ID); err != nil {
		return BillingDocument{}, fmt.Errorf("sales: insert billing document %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
		SELECT $1, $2, n, $3, o, q::numeric, a FROM unnest($4::int[], $5::int[], $6::text[], $7::bigint[]) AS x(n, o, q, a)`,
		p.TenantID, id, o.ID, nos, orderNos, quantities, amounts); err != nil {
		return BillingDocument{}, fmt.Errorf("sales: write items of billing document %s: %w", id, err)
	}
	b, err := scanBilling(tx.QueryRow(ctx, billingSelect+` WHERE b.id = $1`, id))
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: read billing document %s: %w", id, err)
	}
	return b, nil
}

// recordBilling adds the audit record of a billing document's creation
// and publishes BillingDocumentChanged in the same transaction (ADR-0007,
// ADR-0011). The event names the customer by id and number, never by
// name (product owner, 2026-11-05), and carries the net value, not the
// items; a created document had no status before.
func recordBilling(ctx context.Context, b BillingDocument, le tenancy.Entity, customerNumber string) error {
	net := money.EventAmount(b.Net, b.Currency, b.Exponent)
	date := b.BillingDate.Format(time.DateOnly)
	payload := map[string]any{"billing_document_id": b.ID, "document_number": b.DocumentNumber, "legal_entity_id": b.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": b.CustomerID, "customer_number": customerNumber, "sales_order_id": b.OrderID,
		"sales_order_number": b.OrderNumber, "billing_date": date, "net_amount": net, "status": b.Status, "status_before": nil, "version": b.Version}
	detail := map[string]any{"document_number": b.DocumentNumber, "customer_id": b.CustomerID, "sales_order_id": b.OrderID,
		"sales_order_number": b.OrderNumber, "billing_date": date, "status": b.Status, "net_amount": net, "items": len(b.Items)}
	return record(ctx, "sales.billing_document.create", "billing_document", "billing_document_changed", b.ID, detail, payload)
}
