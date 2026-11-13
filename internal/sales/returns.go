package sales

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ReturnsRange is the document type returns orders draw their numbers
// from (configuration number_ranges).
const ReturnsRange = "returns_order"

// ReturnReasons are the reasons a customer returns items for, in the
// order the contract lists them (ADR-0067, As built T85a). The
// enumeration is open: a later version may add to it.
var ReturnReasons = []string{"defective", "wrong_item", "not_wanted", "other"}

// ReturnsItem is an item of a returns order: the quantity returned of the
// billing document's item BillingItemNo, which billed Billed of the sales
// order's item OrderItemNo. Quantities are decimal strings in Unit, the
// order item's, whose key and description say what is returned.
type ReturnsItem struct {
	ItemNo        int32   `json:"item_no"`
	BillingItemNo int32   `json:"billing_item_no"`
	OrderItemNo   int32   `json:"order_item_no"`
	ItemKey       *string `json:"item_key"`
	Description   string  `json:"description"`
	Unit          string  `json:"unit"`
	Billed        string  `json:"billed_quantity"`
	Quantity      string  `json:"quantity"`
}

// ReturnsOrder is a row of sales.returns_order with its items: quantities
// of a posted billing document's items the customer sends back, with the
// reason (ADR-0067, As built T85a). It carries no amounts, posts nothing
// and moves no stock. OrderID is the sales order the billing document
// billed, under whose row lock returns orders are created.
type ReturnsOrder struct {
	ID                    uuid.UUID
	DocumentNumber        string
	LegalEntityID         uuid.UUID
	CustomerID            uuid.UUID
	BillingDocumentID     uuid.UUID
	BillingDocumentNumber string
	OrderID               uuid.UUID
	OrderNumber           string
	ReturnsDate           time.Time
	Reason                string
	ReasonText            *string
	Status                string
	Items                 []ReturnsItem
	Version               int32
	CreatedAt             time.Time
	CreatedBy             uuid.UUID
	UpdatedAt             time.Time
	UpdatedBy             uuid.UUID
}

// ReturnsInput is a returns order as a client sends it: the posted
// billing document, the reason with an optional free text, and the
// quantities returned of the billing document's items, each named by the
// billing item's number.
type ReturnsInput struct {
	BillingDocumentID uuid.UUID
	Reason            string
	ReasonText        *string
	Items             []ItemQuantity
}

// returnsSelect names the billing document and the sales order by
// subqueries, not joins, for the reason orderSelect names the quotation
// by one. An item reads what it names and its unit from the sales order's
// item its billing item billed, which nothing changes.
const returnsSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.customer_id, r.billing_document_id,
		(SELECT b.document_number FROM sales.billing_document b WHERE b.tenant_id = r.tenant_id AND b.id = r.billing_document_id), r.sales_order_id,
		(SELECT o.document_number FROM sales.sales_order o WHERE o.tenant_id = r.tenant_id AND o.id = r.sales_order_id),
		r.returns_date, r.reason, r.reason_text, r.status,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'billing_item_no', i.billing_item_no, 'order_item_no', b.order_item_no,
			'item_key', t.item_key, 'description', t.description, 'unit', t.uom_code, 'billed_quantity', trim_scale(b.quantity)::text,
			'quantity', trim_scale(i.quantity)::text) ORDER BY i.item_no)
		 FROM sales.returns_order_item i
		 JOIN sales.billing_document_item b ON b.tenant_id = i.tenant_id AND b.billing_document_id = i.billing_document_id AND b.item_no = i.billing_item_no
		 JOIN sales.sales_order_item t ON t.tenant_id = b.tenant_id AND t.sales_order_id = b.sales_order_id AND t.item_no = b.order_item_no
		 WHERE i.tenant_id = r.tenant_id AND i.returns_order_id = r.id),
		r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM sales.returns_order r`

func scanReturns(row pgx.Row) (ReturnsOrder, error) {
	var r ReturnsOrder
	err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.CustomerID, &r.BillingDocumentID, &r.BillingDocumentNumber, &r.OrderID,
		&r.OrderNumber, &r.ReturnsDate, &r.Reason, &r.ReasonText, &r.Status, &r.Items, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan returns order")
}

// GetReturns returns a returns order of a legal entity the caller
// may act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetReturns(ctx context.Context, id uuid.UUID) (ReturnsOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ReturnsOrder{}, err
	}
	return returnsOrder(ctx, tx, id, false)
}

// returnsOrder reads a returns order of a legal entity the caller may act
// in, forUpdate under its row lock (stored): the read then sees what the
// transaction it waited for left, the returns order's items included.
func returnsOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (ReturnsOrder, error) {
	return stored[ReturnsOrder]{what: "returns order", table: "returns_order", query: returnsSelect + ` WHERE r.id = $1`, scan: scanReturns,
		company: func(r ReturnsOrder) uuid.UUID { return r.LegalEntityID }}.read(ctx, tx, id, forUpdate)
}

// MakeReturns creates a returns order for quantities of a posted
// billing document's items (ADR-0067, As built T85a). A billing document
// the caller cannot read is a wrong reference (422 naming
// billing_document_id); one that is not posted takes none (409
// sales.billing_document_not_posted). Its sales order's row is read FOR
// UPDATE (ADR-0008), the lock under which billing counts quantities, so
// two requests for one order's billing documents wait for one another
// and the second is judged on what the first took: no quantity is
// returned twice (returnable). The returns order is a draft, dated the
// company's date today, numbered from the company's returns_order range
// of that date's fiscal year. The database adds each quantity to the
// sales order item's returned quantity and moves the sales order's
// version (migration 000303). The audit record and ReturnsOrderChanged
// are written in the request's transaction. Nothing is posted and no
// stock moves.
func (e *Endpoints) MakeReturns(ctx context.Context, in ReturnsInput) (ReturnsOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ReturnsOrder{}, err
	}
	if err := returnReason(in); err != nil {
		return ReturnsOrder{}, err
	}
	b, err := billing(ctx, tx, in.BillingDocumentID, false)
	if err != nil {
		return ReturnsOrder{}, problem.Reference(err, "billing_document_id", "billing document", "no billing document with this id that you can access")
	}
	if b.Status != Posted {
		return ReturnsOrder{}, problem.Conflict("sales.billing_document_not_posted",
			"Billing document "+b.DocumentNumber+" is "+b.Status+"; a returns order is for the items of a posted billing document.")
	}
	if _, err := loadOrder(ctx, tx, b.OrderID, true); err != nil { // a billing document's order exists (billing_document_sales_order_fkey)
		return ReturnsOrder{}, err
	}
	if err := returnable(ctx, tx, b, in.Items); err != nil {
		return ReturnsOrder{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, b.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: company of billing document %s: %w", b.ID, err)
	}
	day, err := e.today(ctx, le.Code)
	if err != nil {
		return ReturnsOrder{}, err
	}
	number, err := e.number(ctx, le.ID, ReturnsRange, day.Date)
	if err != nil {
		return ReturnsOrder{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{b.CustomerID})
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: customer of billing document %s: %w", b.ID, err)
	}
	r, err := insertReturns(ctx, tx, p, b, number, day.Date, in)
	if err != nil {
		return ReturnsOrder{}, err
	}
	return r, recordReturns(ctx, "sales.returns_order.create", r, le, names[b.CustomerID].Number, nil)
}

// returnReason refuses a reason the contract does not list (422
// sales.unknown_return_reason naming reason) and a free text of white
// space alone (422 request.validation_failed naming reason_text).
func returnReason(in ReturnsInput) error {
	if !slices.Contains(ReturnReasons, in.Reason) {
		return problem.Invalid("sales.unknown_return_reason", "reason",
			fmt.Sprintf("%q is not a reason for a return; the reasons are %v.", in.Reason, ReturnReasons))
	}
	if in.ReasonText == nil {
		return nil
	}
	if err := problem.RequireText("reason_text", *in.ReasonText); err != nil {
		return fmt.Errorf("sales: reason of a returns order: %w", err)
	}
	return nil
}

// returnable checks a returns order's items against the billing
// document's, the sales order's row lock held (taking): each names an
// item of the billing document, once, with a quantity more than zero in
// the item's unit that is at most what can still be returned of it, its
// billed quantity less what the document's other returns orders took,
// drafts included, since a returns order is not cancelled. Every refusal
// names the request's own field, and the last the item with what was
// billed and taken of it.
func returnable(ctx context.Context, tx pgx.Tx, b BillingDocument, in []ItemQuantity) error {
	taken, err := returnedQuantities(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	billed := make(map[int32]BillingItem, len(b.Items))
	for _, it := range b.Items {
		billed[it.ItemNo] = it
	}
	short, err := taking{document: "a returns order", noField: "billing_item_no",
		find: func(no int32) (string, *big.Rat, bool) {
			it, ok := billed[no]
			if !ok {
				return "", nil, false
			}
			return it.Unit, new(big.Rat).Sub(money.ParseRat(it.Quantity), taken.of(no)), true
		},
		otherUnit: func(no int32, unit string) string {
			return fmt.Sprintf("Item %d of billing document %s is in %s; its return is in that unit.", no, b.DocumentNumber, unit)
		}}.check(in)
	switch {
	case err != nil || short == nil:
		return err
	case short.open == nil:
		return problem.Invalid("sales.unknown_billing_item", short.noField, fmt.Sprintf("Billing document %s has no item %d.", b.DocumentNumber, short.no))
	}
	it := billed[short.no]
	return problem.Invalid("sales.exceeds_returnable_quantity", short.quantityField, fmt.Sprintf(
		"Item %d of billing document %s has %s %s that can still be returned (%s billed, %s taken by its returns orders); %s is more.",
		short.no, b.DocumentNumber, money.PlainQuantity(short.open), it.Unit, it.Quantity, money.PlainQuantity(taken.of(short.no)), short.quantity))
}

// returnedQuantities are the quantities the billing document's returns
// orders took of each of its items, by the billing item's number.
func returnedQuantities(ctx context.Context, tx pgx.Tx, billingDocument uuid.UUID) (returned, error) {
	rows, _ := tx.Query(ctx, `SELECT billing_item_no, sum(quantity)::text FROM sales.returns_order_item
		WHERE billing_document_id = $1 GROUP BY billing_item_no ORDER BY billing_item_no`, billingDocument)
	out := returned{}
	var no int32
	var quantity string
	_, err := pgx.ForEachRow(rows, []any{&no, &quantity}, func() error { out[no] = money.ParseRat(quantity); return nil })
	if err != nil {
		return nil, fmt.Errorf("sales: read what is returned of billing document %s: %w", billingDocument, err)
	}
	return out, nil
}

// returned are the quantities taken of a document's items, by item
// number.
type returned map[int32]*big.Rat

// of is the quantity taken of the item: zero for one nothing took.
func (r returned) of(no int32) *big.Rat {
	if q, ok := r[no]; ok {
		return q
	}
	return new(big.Rat)
}

// insertReturns writes the returns order with its items, numbered from 1
// in the request's order, and returns it as it reads.
func insertReturns(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b BillingDocument, number string, date time.Time, in ReturnsInput) (ReturnsOrder, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.returns_order (tenant_id, id, document_number, legal_entity_id, customer_id, billing_document_id,
			sales_order_id, returns_date, reason, reason_text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		p.TenantID, id, number, b.LegalEntityID, b.CustomerID, b.ID, b.OrderID, date, in.Reason, in.ReasonText, Draft, p.ID); err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: insert returns order %s: %w", id, err)
	}
	nos, quantities := make([]int32, len(in.Items)), make([]string, len(in.Items))
	for i, it := range in.Items {
		nos[i], quantities[i] = it.ItemNo, it.Quantity
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.returns_order_item (tenant_id, returns_order_id, item_no, billing_document_id, billing_item_no, quantity)
		SELECT $1, $2, ord::int, $3, n, q::numeric FROM unnest($4::int[], $5::text[]) WITH ORDINALITY AS x(n, q, ord)`,
		p.TenantID, id, b.ID, nos, quantities); err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: write items of returns order %s: %w", id, err)
	}
	r, err := scanReturns(tx.QueryRow(ctx, returnsSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: read returns order %s: %w", id, err)
	}
	return r, nil
}

// ConfirmReturns confirms a draft returns order, once (ADR-0067, As
// built T85a). Its row is read FOR UPDATE and only a draft is confirmed
// (ADR-0008): a second confirmation waits for the first and is then
// refused, 412 at the version it read or 409 sales.invalid_status at the
// new one. Confirming is by permission; nothing is checked again. The
// new status, its audit record and ReturnsOrderChanged are written in the
// request's transaction. Nothing is posted and no stock moves.
func (e *Endpoints) ConfirmReturns(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (ReturnsOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ReturnsOrder{}, err
	}
	r, err := returnsOrder(ctx, tx, id, true)
	if err != nil {
		return ReturnsOrder{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return ReturnsOrder{}, err
	}
	if r.Status != Draft {
		return ReturnsOrder{}, problem.Conflict("sales.invalid_status", "Returns order "+r.DocumentNumber+" is "+r.Status+"; only a draft is confirmed.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: company of returns order %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{r.CustomerID})
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: customer of returns order %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.returns_order SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1 AND status = $4`,
		id, Confirmed, p.ID, Draft); err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: mark returns order %s confirmed: %w", id, err)
	}
	confirmed, err := scanReturns(tx.QueryRow(ctx, returnsSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return ReturnsOrder{}, fmt.Errorf("sales: read returns order %s: %w", id, err)
	}
	before := Draft
	return confirmed, recordReturns(ctx, "sales.returns_order.confirm", confirmed, le, names[r.CustomerID].Number, &before)
}

// recordReturns adds the audit record of a returns order's change and
// publishes ReturnsOrderChanged in the same transaction (ADR-0007,
// ADR-0011); before is its status before, nil when it had none. The
// event names the customer by id and number, never by name (product
// owner, 2026-11-05), and carries the reason, not the items; the audit
// record holds the quantities returned.
func recordReturns(ctx context.Context, action string, r ReturnsOrder, le tenancy.Entity, customerNumber string, before *string) error {
	date := r.ReturnsDate.Format(time.DateOnly)
	payload := map[string]any{"returns_order_id": r.ID, "document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": r.CustomerID, "customer_number": customerNumber, "billing_document_id": r.BillingDocumentID,
		"billing_document_number": r.BillingDocumentNumber, "sales_order_id": r.OrderID, "sales_order_number": r.OrderNumber, "returns_date": date,
		"reason": r.Reason, "status": r.Status, "status_before": before, "version": r.Version}
	items := make([]map[string]any, len(r.Items))
	for i, it := range r.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "billing_item_no": it.BillingItemNo, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit}}
	}
	detail := map[string]any{"document_number": r.DocumentNumber, "customer_id": r.CustomerID, "billing_document_id": r.BillingDocumentID,
		"billing_document_number": r.BillingDocumentNumber, "sales_order_id": r.OrderID, "returns_date": date, "reason": r.Reason,
		"reason_text": r.ReasonText, "status": r.Status, "status_before": before, "items": items}
	return record(ctx, action, "returns_order", "returns_order_changed", r.ID, detail, payload)
}
