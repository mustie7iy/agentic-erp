package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a sales order: a draft when created, confirmed once
// (ADR-0067 §5).
const (
	Draft     = "draft"
	Confirmed = "confirmed"
)

// OrderRange is the document type sales orders draw their numbers from
// (configuration number_ranges).
const OrderRange = "sales_order"

func init() {
	// The handler checks under the quotation's row lock; the index is the
	// database's rule and answers alike.
	problem.RegisterConstraint("sales_order_one_per_quotation", quotationOrdered)
}

func quotationOrdered() *problem.Problem {
	return problem.Conflict("sales.quotation_ordered", "The quotation already has a sales order; one accepted quotation gives one order.")
}

// OrderItem is an item of a sales order: a sales item with the quantities
// delivered and billed of it so far, decimal strings in the item's unit.
// Deliveries are the item's deliveries by date (T78c); Orders.Order reads
// them, the API's reads of an order do not and leave them nil.
type OrderItem struct {
	Item
	Delivered  string         `json:"delivered_quantity"`
	Billed     string         `json:"billed_quantity"`
	Deliveries []ItemDelivery `json:"deliveries"`
}

// Order is a row of sales.sales_order with its items. Amounts are minor
// units of Currency, whose minor-unit digits are Exponent; Net is the sum
// of the items' net values. QuotationID and QuotationNumber name the
// quotation it was created from, nil for an order created directly.
type Order struct {
	ID              uuid.UUID
	DocumentNumber  string
	LegalEntityID   uuid.UUID
	CustomerID      uuid.UUID
	QuotationID     *uuid.UUID
	QuotationNumber *string
	OrderDate       time.Time
	Currency        string
	Exponent        int
	PaymentTermCode string
	Net             money.Minor
	Status          string
	Items           []OrderItem
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

// OrderInput is a sales order as a client sends it: QuotationID alone, or
// LegalEntityID, CustomerID, Currency and Items; PaymentTermCode with
// either.
type OrderInput struct {
	QuotationID     *uuid.UUID
	LegalEntityID   *uuid.UUID
	CustomerID      *uuid.UUID
	Currency        *string
	PaymentTermCode *string
	Items           []ItemInput
}

// orderQuery reads orders with their items; extra is further key and
// value pairs of each item, led by a comma. It names the quotation by a
// subquery, not a join: the keyset of the list orders by an unqualified
// id, which a second table with an id would make ambiguous.
func orderQuery(extra string) string {
	return `SELECT o.id, o.document_number, o.legal_entity_id, o.customer_id, o.quotation_id,
		(SELECT q.document_number FROM sales.quotation q WHERE q.tenant_id = o.tenant_id AND q.id = o.quotation_id), o.order_date, o.currency,
		c.exponent, o.payment_term_code, o.net_amount, o.status, ` +
		itemsJSON("sales_order_item", "sales_order_id", "o",
			`, 'delivered_quantity', trim_scale(t.delivered_quantity)::text, 'billed_quantity', trim_scale(t.billed_quantity)::text`+extra) + `,
		o.version, o.created_at, o.created_by, o.updated_at, o.updated_by
	FROM sales.sales_order o JOIN ref.currency c ON c.code = o.currency`
}

// orderSelect is what the API's reads of an order take; publishedSelect
// adds what the other contexts read through Orders: each item's
// deliveries (T78c).
var orderSelect, publishedSelect = orderQuery(""), orderQuery(itemDeliveries)

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.DocumentNumber, &o.LegalEntityID, &o.CustomerID, &o.QuotationID, &o.QuotationNumber, &o.OrderDate, &o.Currency,
		&o.Exponent, &o.PaymentTermCode, &o.Net, &o.Status, &o.Items, &o.Version, &o.CreatedAt, &o.CreatedBy, &o.UpdatedAt, &o.UpdatedBy)
	return o, errs.Wrapf(err, "scan sales order")
}

// loadOrder reads an order whatever its company, forUpdate under its row
// lock (ADR-0008); order applies company access. The lock is taken by a
// statement of its own and the order read by the next: a statement that
// waits for the lock sees the order's row as the other transaction left
// it, but its items as they were when it began (read committed), and
// what is open of an item, to deliver or to bill, must be judged on what
// the other left.
func loadOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Order, error) {
	if forUpdate {
		if _, err := tx.Exec(ctx, `SELECT FROM sales.sales_order WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Order{}, fmt.Errorf("sales: lock sales order %s: %w", id, err)
		}
	}
	o, err := scanOrder(tx.QueryRow(ctx, orderSelect+` WHERE o.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, problem.NotFoundOf("sales order")
	}
	if err != nil {
		return Order{}, fmt.Errorf("sales: read sales order %s: %w", id, err)
	}
	return o, nil
}

// order reads an order of a legal entity the caller may act in, forUpdate
// under its row lock; another's is not found, so ids cannot be probed
// (04 §4.9).
func order(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Order, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Order{}, fmt.Errorf("sales: company access for sales order %s: %w", id, err)
	}
	o, err := loadOrder(ctx, tx, id, forUpdate)
	if err == nil && !access.Allows(o.LegalEntityID) {
		return Order{}, problem.NotFoundOf("sales order")
	}
	return o, err
}

// GetOrder returns a sales order of a legal entity the caller may act in.
func GetOrder(ctx context.Context, id uuid.UUID) (Order, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, err
	}
	return order(ctx, tx, id, false)
}

// Orders is the sales context's published read of sales orders (ADR-0067
// §5), in the request's transaction, as procurement.Orders is for
// payables: what revenue recognition reads (T81a).
type Orders struct{}

// Order returns the sales order with its items, each with its deliveries
// by date (T78c), or ok = false when there is none. Company access is not
// applied: callers apply it where it matters.
func (Orders) Order(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, false, err
	}
	o, err := scanOrder(tx.QueryRow(ctx, publishedSelect+` WHERE o.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("sales: read sales order %s: %w", id, err)
	}
	return o, true, nil
}

// ordered is what a new order is made of, from its quotation or from the
// request: its company, its customer, its currency and date, its items
// with their sum, and the request's field that names the customer.
type ordered struct {
	doc           document
	customer      uuid.UUID
	customerField string
	quotation     *uuid.UUID
	items         []Item
	net           money.Minor
}

// MakeOrder creates a sales order (ADR-0067 §5): from an accepted
// quotation, whose company, customer, currency and items it copies as
// they were offered, or directly from items checked and priced as a
// quotation's. The customer has its data in the company; the order's
// payment term is the one given, else the customer's. It is a draft,
// dated the company's date today, numbered from the company's sales_order
// range of that date's fiscal year. It publishes SalesOrderChanged with
// its audit record. Nothing is posted.
func (e *Endpoints) MakeOrder(ctx context.Context, in OrderInput) (Order, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Order{}, err
	}
	src, err := e.source(ctx, tx, in)
	if err != nil {
		return Order{}, err
	}
	customer, err := e.orderCustomer(ctx, src)
	if err != nil {
		return Order{}, err
	}
	term, err := e.paymentTerm(ctx, in.PaymentTermCode, customer)
	if err != nil {
		return Order{}, err
	}
	number, err := e.number(ctx, src.doc.le.ID, OrderRange, src.doc.date)
	if err != nil {
		return Order{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_id, order_date,
			currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		p.TenantID, id, number, src.doc.le.ID, src.customer, src.quotation, src.doc.date, src.doc.currency, term, src.net, Draft, p.ID); err != nil {
		return Order{}, fmt.Errorf("sales: insert sales order %s: %w", id, err)
	}
	if err := insertItems(ctx, tx, "sales_order_item", "sales_order_id", p.TenantID, id, src.items); err != nil {
		return Order{}, err
	}
	o, err := loadOrder(ctx, tx, id, false)
	if err != nil {
		return Order{}, err
	}
	return o, recordOrder(ctx, "sales.sales_order.create", o, src.doc.le, customer.Number, nil)
}

// source resolves what the order is made of: its quotation's, or the
// request's own company, customer, currency and items, never a mix (422
// sales.quotation_or_items naming the field given or missing).
func (e *Endpoints) source(ctx context.Context, tx pgx.Tx, in OrderInput) (ordered, error) {
	given := []struct {
		field string
		set   bool
	}{{"legal_entity_id", in.LegalEntityID != nil}, {"customer_id", in.CustomerID != nil}, {"currency", in.Currency != nil}, {"items", in.Items != nil}}
	for _, g := range given {
		switch {
		case in.QuotationID != nil && g.set:
			return ordered{}, problem.Invalid("sales.quotation_or_items", g.field,
				"An order from a quotation takes its company, customer, currency and items from the quotation; the request names none of them.")
		case in.QuotationID == nil && !g.set:
			return ordered{}, problem.Invalid("sales.quotation_or_items", g.field,
				"An order is created from a quotation (quotation_id) or directly, with legal_entity_id, customer_id, currency and items.")
		}
	}
	if in.QuotationID != nil {
		return e.fromQuotation(ctx, tx, *in.QuotationID)
	}
	le, err := e.company(ctx, *in.LegalEntityID)
	if err != nil {
		return ordered{}, err
	}
	doc, err := e.document(ctx, tx, le, *in.CustomerID, *in.Currency)
	if err != nil {
		return ordered{}, err
	}
	items, net, err := e.items(ctx, tx, doc, in.Items)
	if err != nil {
		return ordered{}, err
	}
	return ordered{doc: doc, customer: *in.CustomerID, customerField: "customer_id", items: items, net: net}, nil
}

// fromQuotation is the order an accepted quotation gives: its company,
// customer, currency and items as they were offered, dated the company's
// date today. The quotation is read under its row lock, so two requests
// for the same quotation's order wait for one another and the second
// finds the first's (409 sales.quotation_ordered); one that the customer
// did not accept gives none (409 sales.quotation_not_accepted). A
// quotation the caller cannot read is a wrong reference (422 naming
// quotation_id).
func (e *Endpoints) fromQuotation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ordered, error) {
	q, err := quotation(ctx, tx, id, true)
	if err != nil {
		return ordered{}, problem.Reference(err, "quotation_id", "quotation", "no quotation with this id that you can access")
	}
	if q.Status != Accepted {
		return ordered{}, problem.Conflict("sales.quotation_not_accepted",
			"Quotation "+q.DocumentNumber+" is "+q.Status+"; an order is created from a quotation the customer accepted.")
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM sales.sales_order WHERE quotation_id = $1)`, id).Scan(&taken); err != nil {
		return ordered{}, fmt.Errorf("sales: find the order of quotation %s: %w", id, err)
	}
	if taken {
		return ordered{}, quotationOrdered()
	}
	le, _, err := e.LegalEntities.Entity(ctx, q.LegalEntityID) // a stored quotation's legal entity exists
	if err != nil {
		return ordered{}, fmt.Errorf("sales: company of quotation %s: %w", id, err)
	}
	day, err := e.today(ctx, le.Code)
	if err != nil {
		return ordered{}, err
	}
	return ordered{doc: document{le: le, customer: q.CustomerID, currency: q.Currency, exp: q.Exponent, date: day.Date, zone: day.Zone}, customer: q.CustomerID, customerField: "quotation_id",
		quotation: &q.ID, items: q.Items, net: q.Net}, nil
}

// orderCustomer is the order's customer with its data in the company,
// which a sales order requires as a receivables document does (ADR-0067
// §4; ADR-0063 §3): one that is unknown is a wrong reference, and one that
// is not active, no longer a customer, without company data there or
// blocked for posting is refused, 422 sales.customer_not_usable, naming
// the request's field that named the customer.
func (e *Endpoints) orderCustomer(ctx context.Context, src ordered) (partner.Customer, error) {
	c, ok, err := e.CustomerData.Customer(ctx, src.customer, src.doc.le.ID)
	if err != nil {
		return partner.Customer{}, fmt.Errorf("sales: customer %s of a sales order: %w", src.customer, err)
	}
	if !ok {
		return partner.Customer{}, problem.ReferenceNotFound(src.customerField, "business partner", "no business partner with this id")
	}
	if why := c.Usable(); why != "" {
		return partner.Customer{}, problem.Invalid("sales.customer_not_usable", src.customerField, "The customer cannot be sold to: "+why+".")
	}
	return c, nil
}

// paymentTerm is the order's payment term: the one given, which must be
// configured (422 sales.unknown_payment_term naming payment_term_code),
// else the customer's in the company.
func (e *Endpoints) paymentTerm(ctx context.Context, given *string, customer partner.Customer) (string, error) {
	if given == nil {
		return customer.PaymentTermCode, nil
	}
	_, ok, err := e.Terms.PaymentTerm(ctx, *given)
	if err != nil {
		return "", fmt.Errorf("sales: payment term %s: %w", *given, err)
	}
	if !ok {
		return "", problem.Invalid("sales.unknown_payment_term", "payment_term_code", "Payment term "+*given+" is not configured.")
	}
	return *given, nil
}

// ConfirmOrder confirms a draft order, once (ADR-0067 §5). The order's
// row is read FOR UPDATE and only a draft is confirmed (ADR-0008): a
// second confirmation waits for the first and is then refused, 409
// sales.invalid_status. The new status, its audit record and
// SalesOrderChanged are written in the request's transaction.
func (e *Endpoints) ConfirmOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Order, error) {
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
	if o.Status != Draft {
		return Order{}, problem.Conflict("sales.invalid_status", "Sales order "+o.DocumentNumber+" is "+o.Status+"; only a draft is confirmed.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return Order{}, fmt.Errorf("sales: company of sales order %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return Order{}, fmt.Errorf("sales: customer of sales order %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.sales_order SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, Confirmed, p.ID); err != nil {
		return Order{}, fmt.Errorf("sales: mark sales order %s confirmed: %w", id, err)
	}
	confirmed, err := loadOrder(ctx, tx, id, false)
	if err != nil {
		return Order{}, err
	}
	before := Draft
	return confirmed, recordOrder(ctx, "sales.sales_order.confirm", confirmed, le, names[o.CustomerID].Number, &before)
}

// recordOrder adds the audit record of an order's change and publishes
// SalesOrderChanged in the same transaction (ADR-0007, ADR-0011); before
// is its status before, nil when it had none. The event names the
// customer by id and number, never by name (product owner, 2026-11-05),
// and carries the net value, not the items.
func recordOrder(ctx context.Context, action string, o Order, le tenancy.Entity, customerNumber string, before *string) error {
	net := money.EventAmount(o.Net, o.Currency, o.Exponent)
	payload := map[string]any{"sales_order_id": o.ID, "document_number": o.DocumentNumber, "legal_entity_id": o.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": o.CustomerID, "customer_number": customerNumber, "quotation_id": o.QuotationID,
		"order_date": o.OrderDate.Format(time.DateOnly), "payment_term_code": o.PaymentTermCode, "net_amount": net,
		"status": o.Status, "status_before": before, "version": o.Version}
	detail := map[string]any{"document_number": o.DocumentNumber, "customer_id": o.CustomerID, "quotation_id": o.QuotationID, "status": o.Status,
		"status_before": before, "payment_term_code": o.PaymentTermCode, "net_amount": net, "items": len(o.Items)}
	return record(ctx, action, "sales_order", "sales_order_changed", o.ID, detail, payload)
}
