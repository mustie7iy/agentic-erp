package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Approved is the status of a credit memo request a sales manager
// approved; it is created Open and may be Rejected instead (ADR-0067, As
// built T85c).
const Approved = "approved"

// CreditMemoRequestRange is the document type credit memo requests draw
// their numbers from (configuration number_ranges).
const CreditMemoRequestRange = "credit_memo_request"

// CreditItem is an item of a credit memo request: the amount credited of
// the billing document's item BillingItemNo, which billed BilledQuantity
// of the sales order's item OrderItemNo for Billed. Quantity is the
// quantity credited of what the request's returns order received, nil on
// a request by amount. What is sold, the unit, the account and the tax
// code are the order item's, which the credit memo posts with (T85d).
type CreditItem struct {
	ItemNo         int32       `json:"item_no"`
	BillingItemNo  int32       `json:"billing_item_no"`
	OrderItemNo    int32       `json:"order_item_no"`
	ItemKey        *string     `json:"item_key"`
	Description    string      `json:"description"`
	Unit           string      `json:"unit"`
	Quantity       *string     `json:"quantity"`
	BilledQuantity string      `json:"billed_quantity"`
	Billed         money.Minor `json:"billed_amount"`
	Net            money.Minor `json:"net_amount"`
	AccountID      uuid.UUID   `json:"account_id"`
	TaxCode        *string     `json:"tax_code"`
}

// CreditMemoRequest is a row of sales.credit_memo_request with its items:
// what is asked to be credited of a posted billing document's items
// (ADR-0067, As built T85c). Amounts are net, minor units of Currency,
// the billing document's, whose minor-unit digits are Exponent; Net is
// the sum of the items' amounts. ReturnsOrderID is the returns order
// whose received quantities it credits, nil for a request by amount.
type CreditMemoRequest struct {
	ID                    uuid.UUID
	DocumentNumber        string
	LegalEntityID         uuid.UUID
	CustomerID            uuid.UUID
	BillingDocumentID     uuid.UUID
	BillingDocumentNumber string
	OrderID               uuid.UUID
	OrderNumber           string
	ReturnsOrderID        *uuid.UUID
	ReturnsOrderNumber    *string
	RequestDate           time.Time
	Reason                *string
	Status                string
	RejectionReason       *string
	Currency              string
	Exponent              int
	Net                   money.Minor
	Items                 []CreditItem
	Version               int32
	CreatedAt             time.Time
	CreatedBy             uuid.UUID
	UpdatedAt             time.Time
	UpdatedBy             uuid.UUID
}

// CreditAmount is an amount credited of one item of a billing document as
// a client sends it: the billing item's number and a net amount.
type CreditAmount struct {
	BillingItemNo   int32
	Value, Currency string
}

// CreditInput is a credit memo request as a client sends it: the posted
// billing document and either the returns order whose received
// quantities are credited or the amounts credited of its items, with the
// reason.
type CreditInput struct {
	BillingDocumentID uuid.UUID
	ReturnsOrderID    *uuid.UUID
	Reason            *string
	Items             []CreditAmount
}

// creditSelect reads a request with its currency from its billing
// document, which nothing changes, by subqueries and a lateral one, so
// the keyset's bare id stays the request's (as returnsSelect does), and
// each item with what its billing
// item billed and what the sales order's item names.
const creditSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.customer_id, r.billing_document_id,
		(SELECT b.document_number FROM sales.billing_document b WHERE b.tenant_id = r.tenant_id AND b.id = r.billing_document_id), r.sales_order_id,
		(SELECT o.document_number FROM sales.sales_order o WHERE o.tenant_id = r.tenant_id AND o.id = r.sales_order_id), r.returns_order_id,
		(SELECT x.document_number FROM sales.returns_order x WHERE x.tenant_id = r.tenant_id AND x.id = r.returns_order_id),
		r.request_date, r.reason, r.status, r.rejection_reason, cur.code, cur.exponent,
		(SELECT sum(i.net_amount)::bigint FROM sales.credit_memo_request_item i WHERE i.tenant_id = r.tenant_id AND i.request_id = r.id),
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'billing_item_no', i.billing_item_no, 'order_item_no', d.order_item_no,
			'item_key', t.item_key, 'description', t.description, 'unit', t.uom_code, 'quantity', trim_scale(i.quantity)::text,
			'billed_quantity', trim_scale(d.quantity)::text, 'billed_amount', d.net_amount, 'net_amount', i.net_amount,
			'account_id', t.account_id, 'tax_code', t.tax_code) ORDER BY i.item_no)
		 FROM sales.credit_memo_request_item i
		 JOIN sales.billing_document_item d ON d.tenant_id = i.tenant_id AND d.billing_document_id = i.billing_document_id AND d.item_no = i.billing_item_no
		 JOIN sales.sales_order_item t ON t.tenant_id = d.tenant_id AND t.sales_order_id = d.sales_order_id AND t.item_no = d.order_item_no
		 WHERE i.tenant_id = r.tenant_id AND i.request_id = r.id),
		r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM sales.credit_memo_request r,
		LATERAL (SELECT c.code, c.exponent FROM sales.billing_document b JOIN ref.currency c ON c.code = b.currency
			WHERE b.tenant_id = r.tenant_id AND b.id = r.billing_document_id) cur`

// columns are where a row of creditSelect is scanned to, in its order.
func (r *CreditMemoRequest) columns() []any {
	return []any{&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.CustomerID, &r.BillingDocumentID, &r.BillingDocumentNumber, &r.OrderID,
		&r.OrderNumber, &r.ReturnsOrderID, &r.ReturnsOrderNumber, &r.RequestDate, &r.Reason, &r.Status, &r.RejectionReason, &r.Currency,
		&r.Exponent, &r.Net, &r.Items, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy}
}

func scanCredit(row pgx.Row) (CreditMemoRequest, error) {
	var r CreditMemoRequest
	err := row.Scan(r.columns()...)
	return r, errs.Wrapf(err, "scan credit memo request")
}

// GetCreditMemoRequest returns a credit memo request of a legal entity
// the caller may act in; another's is not found, so ids cannot be probed
// (04 §4.9).
func GetCreditMemoRequest(ctx context.Context, id uuid.UUID) (CreditMemoRequest, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	return creditRequest(ctx, tx, id, false)
}

// creditRequest reads a credit memo request of a legal entity the caller
// may act in, forUpdate under its row lock (stored).
func creditRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (CreditMemoRequest, error) {
	return stored[CreditMemoRequest]{what: "credit memo request", table: "credit_memo_request", query: creditSelect + ` WHERE r.id = $1`, scan: scanCredit,
		company: func(r CreditMemoRequest) uuid.UUID { return r.LegalEntityID }}.read(ctx, tx, id, forUpdate)
}

// RequestCredit creates a credit memo request against a posted billing
// document (ADR-0067, As built T85c): for a returns order's received
// quantities not yet requested, at the billed amount of those quantities
// (creditReturned), or as an amount per billing item with a reason
// (creditAmounts). A billing document the caller cannot read is a wrong
// reference (422 naming billing_document_id); one that is not posted
// takes none (409 sales.billing_document_not_posted). Its sales order's
// row is read FOR UPDATE (ADR-0008), the lock under which returns orders
// are created and billing counts quantities, so two requests against one
// order's billing documents wait for one another and the second is
// judged on what the first asked for: per billing item the requests not
// rejected never exceed its billed amount, and no received quantity is
// requested twice. The request is open, dated the company's date today,
// numbered from the company's credit_memo_request range of that date's
// fiscal year. The audit record and CreditMemoRequestChanged are written
// in the request's transaction. Nothing is posted.
func (e *Endpoints) RequestCredit(ctx context.Context, in CreditInput) (CreditMemoRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	if err := creditBasis(in); err != nil {
		return CreditMemoRequest{}, err
	}
	b, err := billing(ctx, tx, in.BillingDocumentID, false)
	if err != nil {
		return CreditMemoRequest{}, problem.Reference(err, "billing_document_id", "billing document", "no billing document with this id that you can access")
	}
	if b.Status != Posted {
		return CreditMemoRequest{}, problem.Conflict("sales.billing_document_not_posted",
			"Billing document "+b.DocumentNumber+" is "+b.Status+"; a credit memo request is against a posted billing document.")
	}
	if _, err := loadOrder(ctx, tx, b.OrderID, true); err != nil { // a billing document's order exists (billing_document_sales_order_fkey)
		return CreditMemoRequest{}, err
	}
	lines, err := creditLines(ctx, tx, b, in)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, b.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: company of billing document %s: %w", b.ID, err)
	}
	day, err := e.today(ctx, le.Code)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	number, err := e.number(ctx, le.ID, CreditMemoRequestRange, day.Date)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{b.CustomerID})
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: customer of billing document %s: %w", b.ID, err)
	}
	r, err := insertCredit(ctx, tx, p, b, number, day.Date, in, lines)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	return r, recordCredit(ctx, "sales.credit_memo_request.create", r, le, names[b.CustomerID].Number, nil)
}

// creditBasis refuses a request that names both a returns order and
// items, or neither (422 sales.credit_memo_request_basis), a request by
// amount without a reason and a reason of white space alone (422
// request.validation_failed naming reason).
func creditBasis(in CreditInput) error {
	if (in.ReturnsOrderID == nil) == (len(in.Items) == 0) {
		return problem.Invalid("sales.credit_memo_request_basis", "returns_order_id",
			"A credit memo request is either for a returns order's received quantities (returns_order_id) or for an amount per item (items), not both.")
	}
	if in.Reason == nil {
		if in.ReturnsOrderID != nil {
			return nil
		}
		return problem.Unprocessable("request.validation_failed", "The request is not valid: a credit memo request by amount gives its reason.",
			problem.FieldError{Field: "reason", Code: "required", Message: "required with items"})
	}
	if err := problem.RequireText("reason", *in.Reason); err != nil {
		return fmt.Errorf("sales: reason of a credit memo request: %w", err)
	}
	return nil
}

// creditLine is an item a new request takes: the billing document's item,
// its amount and, for a returns order's item, the quantity credited.
type creditLine struct {
	billingItemNo int32
	quantity      *string
	net           money.Minor
}

// creditLines are the items of a new request against the billing
// document, the sales order's row lock held: what its returns order's
// items received and no request took, or the amounts its items name.
func creditLines(ctx context.Context, tx pgx.Tx, b BillingDocument, in CreditInput) ([]creditLine, error) {
	asked, err := creditedSoFar(ctx, tx, b.ID, in.ReturnsOrderID)
	if err != nil {
		return nil, err
	}
	billed := make(map[int32]BillingItem, len(b.Items))
	for _, it := range b.Items {
		billed[it.ItemNo] = it
	}
	if in.ReturnsOrderID == nil {
		return creditAmounts(b, billed, asked, in.Items)
	}
	r, err := returnsOrder(ctx, tx, *in.ReturnsOrderID, false)
	if err != nil {
		return nil, problem.Reference(err, "returns_order_id", "returns order", "no returns order with this id that you can access")
	}
	if r.BillingDocumentID != b.ID {
		return nil, problem.Invalid("sales.returns_order_of_other_document", "returns_order_id", fmt.Sprintf(
			"Returns order %s is for billing document %s, not %s.", r.DocumentNumber, r.BillingDocumentNumber, b.DocumentNumber))
	}
	return creditReturned(b, billed, asked, r)
}

// credited is what the requests of a billing document that are not
// rejected ask for of one of its items: all is the amount they ask for
// together; byQuantity and quantity are the amount and the quantity of
// those that credit quantities, of every returns order; received is the
// quantity those of the returns order in hand credit.
type credited struct {
	all, byQuantity    money.Minor
	quantity, received *big.Rat
}

// creditedSoFar reads what the billing document's requests that are not
// rejected ask for, by the billing item's number (credited); returns is
// the returns order a new request is for, nil for a request by amount.
func creditedSoFar(ctx context.Context, tx pgx.Tx, billingDocument uuid.UUID, returns *uuid.UUID) (map[int32]credited, error) {
	rows, _ := tx.Query(ctx, `SELECT i.billing_item_no, sum(i.net_amount)::bigint, coalesce(sum(i.net_amount) FILTER (WHERE i.quantity IS NOT NULL), 0)::bigint,
			coalesce(sum(i.quantity), 0)::text, coalesce(sum(i.quantity) FILTER (WHERE i.returns_order_id = $2), 0)::text
		FROM sales.credit_memo_request_item i JOIN sales.credit_memo_request r ON r.tenant_id = i.tenant_id AND r.id = i.request_id
		WHERE i.billing_document_id = $1 AND r.status <> $3 GROUP BY i.billing_item_no ORDER BY i.billing_item_no`, billingDocument, returns, Rejected)
	out := map[int32]credited{}
	var no int32
	var c credited
	var quantity, received string
	_, err := pgx.ForEachRow(rows, []any{&no, &c.all, &c.byQuantity, &quantity, &received}, func() error {
		c.quantity, c.received = money.ParseRat(quantity), money.ParseRat(received)
		out[no] = c
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sales: read what is requested of billing document %s: %w", billingDocument, err)
	}
	return out, nil
}

// exceedsBilled refuses an amount that, with what the document's other
// requests ask for of the billing item, is more than the item billed
// (422 sales.exceeds_billed_amount naming field).
func exceedsBilled(b BillingDocument, it BillingItem, asked, amount money.Minor, field string) error {
	if amount <= it.Net-asked { // asked is at most the item's amount (credit_memo_request_item_amounts)
		return nil
	}
	return problem.Invalid("sales.exceeds_billed_amount", field, fmt.Sprintf(
		"Item %d of billing document %s billed %s %s, of which its credit memo requests ask for %s; %s more is above what it billed.",
		it.ItemNo, b.DocumentNumber, it.Net.Plain(b.Exponent), b.Currency, asked.Plain(b.Exponent), amount.Plain(b.Exponent)))
}

// creditAmounts checks the amounts a request by amount names: each a
// billing item of the document, once, with a net amount more than zero
// in the document's currency that, with what the document's other
// requests ask for of the item, is at most what the item billed. Every
// refusal names the request's own field.
func creditAmounts(b BillingDocument, billed map[int32]BillingItem, asked map[int32]credited, in []CreditAmount) ([]creditLine, error) {
	lines, named := make([]creditLine, len(in)), make(map[int32]bool, len(in))
	for i, a := range in {
		field := fmt.Sprintf("items[%d]", i)
		it, ok := billed[a.BillingItemNo]
		switch {
		case !ok:
			return nil, problem.Invalid("sales.unknown_billing_item", field+".billing_item_no",
				fmt.Sprintf("Billing document %s has no item %d.", b.DocumentNumber, a.BillingItemNo))
		case named[a.BillingItemNo]:
			return nil, problem.Invalid("sales.item_repeated", field+".billing_item_no",
				fmt.Sprintf("A credit memo request names an item once; item %d is named more than once.", a.BillingItemNo))
		case a.Currency != b.Currency:
			return nil, problem.Invalid("sales.currency_mismatch", field+".net_amount.currency",
				"A credit memo request's amounts are in its billing document's currency, "+b.Currency+".")
		}
		named[a.BillingItemNo] = true
		net, err := currency.ParseAmount(a.Value, b.Currency, b.Exponent, field+".net_amount.amount")
		if err != nil {
			return nil, fmt.Errorf("sales: amount of item %d of a credit memo request: %w", i+1, err)
		}
		if net <= 0 {
			return nil, problem.Invalid("sales.amount_not_positive", field+".net_amount.amount", "An amount credited is more than zero.")
		}
		if err := exceedsBilled(b, it, asked[a.BillingItemNo].all, net, field+".net_amount.amount"); err != nil {
			return nil, err
		}
		lines[i] = creditLine{billingItemNo: a.BillingItemNo, net: net}
	}
	return lines, nil
}

// creditReturned are the items of a request for the returns order: for
// each of its items, the quantity received less what the requests not
// rejected took of it, when there is one, at the billed amount of that
// quantity in full (product owner 2026-11-12: no charge is taken off).
// As billing shares an order item's value out (billable), the amount is
// the share of the billing item's amount that the quantity requested of
// it so far, this request included, is of its billed quantity (share),
// less what the earlier requests for quantities took: the requests for
// an item's whole billed quantity add up to its billed amount to the
// minor unit, the last taking the remainder. A quantity whose amount
// rounds to zero is left for a later request, as billing leaves one; a
// returns order that leaves nothing to credit is refused (422
// sales.nothing_to_credit naming returns_order_id), as is an item whose
// amount, with the document's requests by amount, is above what it
// billed (exceedsBilled).
func creditReturned(b BillingDocument, billed map[int32]BillingItem, asked map[int32]credited, r ReturnsOrder) ([]creditLine, error) {
	var lines []creditLine
	for _, ri := range r.Items {
		it, taken := billed[ri.BillingItemNo], asked[ri.BillingItemNo] // a returns order's item is of its billing document (returns_order_item_billing_item_fkey)
		open := money.ParseRat(ri.Received)
		soFar := new(big.Rat).Set(open)
		if taken.quantity != nil {
			open.Sub(open, taken.received)
			soFar.Add(open, taken.quantity)
		}
		amount := share(it.Net, soFar, money.ParseRat(it.Quantity)) - taken.byQuantity
		if open.Sign() <= 0 || amount <= 0 {
			continue
		}
		if err := exceedsBilled(b, it, taken.all, amount, "returns_order_id"); err != nil {
			return nil, err
		}
		quantity := money.PlainQuantity(open)
		lines = append(lines, creditLine{billingItemNo: ri.BillingItemNo, quantity: &quantity, net: amount})
	}
	if len(lines) == 0 {
		return nil, problem.Invalid("sales.nothing_to_credit", "returns_order_id", "Returns order "+r.DocumentNumber+
			" has nothing to credit: a credit memo request credits what was received of its items and is not yet requested.")
	}
	return lines, nil
}

// share is the part of a total in minor units that part is of whole,
// shared out with money.Allocate against the rest (product owner
// 2026-11-12), so the shares of a whole add up to the total to the minor
// unit; part is at most whole.
func share(total money.Minor, part, whole *big.Rat) money.Minor {
	all := new(big.Rat).SetInt64(int64(total))
	x := new(big.Rat).Mul(all, new(big.Rat).Quo(part, whole))
	out, _ := money.Allocate([]*big.Rat{x, all.Sub(all, x)}) // the two add up to total, which is in range
	return out[0]
}

// insertCredit writes the request with its items, numbered from 1 in the
// lines' order, and returns it as it reads.
func insertCredit(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b BillingDocument, number string, date time.Time, in CreditInput, lines []creditLine) (CreditMemoRequest, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.credit_memo_request (tenant_id, id, document_number, legal_entity_id, customer_id, billing_document_id,
			sales_order_id, returns_order_id, request_date, reason, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		p.TenantID, id, number, b.LegalEntityID, b.CustomerID, b.ID, b.OrderID, in.ReturnsOrderID, date, in.Reason, Open, p.ID); err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: insert credit memo request %s: %w", id, err)
	}
	nos, quantities, amounts := make([]int32, len(lines)), make([]*string, len(lines)), make([]money.Minor, len(lines))
	for i, l := range lines {
		nos[i], quantities[i], amounts[i] = l.billingItemNo, l.quantity, l.net
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.credit_memo_request_item (tenant_id, request_id, item_no, billing_document_id, billing_item_no,
			returns_order_id, quantity, net_amount)
		SELECT $1, $2, ord::int, $3, n, $4, q::numeric, a FROM unnest($5::int[], $6::text[], $7::bigint[]) WITH ORDINALITY AS x(n, q, a, ord)`,
		p.TenantID, id, b.ID, in.ReturnsOrderID, nos, quantities, amounts); err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: write items of credit memo request %s: %w", id, err)
	}
	r, err := scanCredit(tx.QueryRow(ctx, creditSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: read credit memo request %s: %w", id, err)
	}
	return r, nil
}

// ApproveCredit approves an open credit memo request, once (decide).
func (e *Endpoints) ApproveCredit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (CreditMemoRequest, error) {
	return e.decide(ctx, id, ifMatch, Approved, nil)
}

// RejectCredit rejects an open credit memo request with the reason, once
// (decide); a reason of white space alone is refused (422
// request.validation_failed naming reason).
func (e *Endpoints) RejectCredit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (CreditMemoRequest, error) {
	if err := problem.RequireText("reason", reason); err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: reason of a rejection: %w", err)
	}
	return e.decide(ctx, id, ifMatch, Rejected, &reason)
}

// decide approves or rejects an open credit memo request, once
// (ADR-0067, As built T85c): to is Approved, or Rejected with the reason.
// Its row is read FOR UPDATE and only an open one is decided (ADR-0008):
// a second decision waits for the first and is then refused, 412 at the
// version it read or 409 sales.invalid_status at the new one. Deciding is
// by permission, whatever the amount, and nothing sets the person apart
// from the one who created the request (product owner 2026-11-12). A
// rejected request no longer counts against its billing items' amounts
// or its returns order's received quantities. The new status, its audit
// record and CreditMemoRequestChanged are written in the request's
// transaction. Nothing is posted.
func (e *Endpoints) decide(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, to string, reason *string) (CreditMemoRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	r, err := creditRequest(ctx, tx, id, true)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return CreditMemoRequest{}, err
	}
	if r.Status != Open {
		return CreditMemoRequest{}, problem.Conflict("sales.invalid_status",
			"Credit memo request "+r.DocumentNumber+" is "+r.Status+"; only an open one is approved or rejected.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: company of credit memo request %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{r.CustomerID})
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: customer of credit memo request %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.credit_memo_request SET status = $2, rejection_reason = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND status = $5`, id, to, reason, p.ID, Open); err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: mark credit memo request %s %s: %w", id, to, err)
	}
	decided, err := scanCredit(tx.QueryRow(ctx, creditSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: read credit memo request %s: %w", id, err)
	}
	before := Open
	action := map[string]string{Approved: "sales.credit_memo_request.approve", Rejected: "sales.credit_memo_request.reject"}[to]
	return decided, recordCredit(ctx, action, decided, le, names[r.CustomerID].Number, &before)
}

// recordCredit adds the audit record of a credit memo request's change
// and publishes CreditMemoRequestChanged in the same transaction
// (ADR-0007, ADR-0011); before is its status before, nil when it had
// none. The event names the customer by id and number, never by name
// (product owner, 2026-11-05), and carries the net value, not the items;
// the audit record holds the amounts and quantities asked for and the
// reasons.
func recordCredit(ctx context.Context, action string, r CreditMemoRequest, le tenancy.Entity, customerNumber string, before *string) error {
	net, date := money.EventAmount(r.Net, r.Currency, r.Exponent), r.RequestDate.Format(time.DateOnly)
	payload := map[string]any{"credit_memo_request_id": r.ID, "document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": r.CustomerID, "customer_number": customerNumber, "billing_document_id": r.BillingDocumentID,
		"billing_document_number": r.BillingDocumentNumber, "sales_order_id": r.OrderID, "sales_order_number": r.OrderNumber,
		"returns_order_id": r.ReturnsOrderID, "returns_order_number": r.ReturnsOrderNumber, "request_date": date, "net_amount": net,
		"status": r.Status, "status_before": before, "version": r.Version}
	items := make([]map[string]any, len(r.Items))
	for i, it := range r.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "billing_item_no": it.BillingItemNo, "net_amount": money.EventAmount(it.Net, r.Currency, r.Exponent)}
		if it.Quantity != nil {
			items[i]["quantity"] = map[string]string{"value": *it.Quantity, "unit": it.Unit}
		}
	}
	detail := map[string]any{"document_number": r.DocumentNumber, "customer_id": r.CustomerID, "billing_document_id": r.BillingDocumentID,
		"billing_document_number": r.BillingDocumentNumber, "sales_order_id": r.OrderID, "returns_order_id": r.ReturnsOrderID,
		"returns_order_number": r.ReturnsOrderNumber, "request_date": date, "reason": r.Reason, "rejection_reason": r.RejectionReason,
		"net_amount": net, "status": r.Status, "status_before": before, "items": items}
	return record(ctx, action, "credit_memo_request", "credit_memo_request_changed", r.ID, detail, payload)
}
