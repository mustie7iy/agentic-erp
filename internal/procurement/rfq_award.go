package procurement

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The award of an RFQ (ADR-0069 §7, T82d): a published RFQ is awarded
// once, to one of its quotations as a whole. The award creates a draft
// contract with that quotation's supplier through the contract's own
// code path (draftContract: its checks, number and audit record), or a
// draft purchase order through the order's (createOrder, T82f), marks the
// quotation awarded and the RFQ's others not awarded, and moves the RFQ
// to awarded, naming the document created and the reason given, all in
// the request's transaction. A quotation not ranked first needs a reason.

// RFQAwarded is the status of an RFQ that was awarded to a quotation.
const RFQAwarded = "awarded"

// The outcomes of a supplier quotation once its RFQ is awarded.
const (
	QuotationAwarded    = "awarded"
	QuotationNotAwarded = "not_awarded"
)

// What an award creates (Award.To).
const (
	AwardToContract = "contract"
	AwardToOrder    = "purchase_order"
)

// Award is the input of awardRFQ: the quotation that wins, what the award
// creates, the validity of a contract created from it (nil for an order)
// and the reason it wins, if one is given.
type Award struct {
	QuotationID        uuid.UUID
	To                 string
	ValidFrom, ValidTo *time.Time
	Reason             *string
}

// check refuses an award whose fields do not fit what it creates (a
// contract's validity, sent for a contract only) or the RFQ (one from a
// requisition is awarded to an order, so that the order names it).
func (in Award) check(r RFQ) error {
	if in.To != AwardToContract && in.To != AwardToOrder {
		return problem.Unprocessable("request.validation_failed", "An award is to a contract or a purchase order.",
			problem.FieldError{Field: "award_to", Code: "invalid", Message: "contract or purchase_order"})
	}
	var fields []problem.FieldError
	for field, given := range map[string]bool{"valid_from": in.ValidFrom != nil, "valid_to": in.ValidTo != nil} {
		switch {
		case in.To == AwardToContract && !given:
			fields = append(fields, problem.FieldError{Field: field, Code: "required", Message: "the contract's validity, for an award to a contract"})
		case in.To == AwardToOrder && given:
			fields = append(fields, problem.FieldError{Field: field, Code: "invalid", Message: "a contract's validity; an award to a purchase order has none"})
		}
	}
	if len(fields) > 0 {
		slices.SortFunc(fields, func(a, b problem.FieldError) int { return strings.Compare(a.Field, b.Field) })
		return problem.Unprocessable("request.validation_failed", "The award's validity fits what it creates.", fields...)
	}
	if in.To == AwardToContract && r.RequisitionID != nil {
		return problem.Invalid("procurement.rfq_from_requisition", "award_to",
			"RFQ "+r.DocumentNumber+" was created from requisition "+*r.RequisitionNumber+": it is awarded to a purchase order, which names the requisition")
	}
	if in.Reason != nil {
		return problem.RequireText("reason", *in.Reason)
	}
	return nil
}

// reasoned requires a reason for an award to a quotation the comparison
// does not rank first: the lowest total wins unless the buyer says why
// another does. Equal lowest totals share rank 1, so any of them wins
// without one.
func (in Award) reasoned(win Compared) error {
	if win.Rank == 1 || in.Reason != nil {
		return nil
	}
	why := fmt.Sprintf("the quotation is ranked %d, not first: an award to any quotation but the lowest records why it wins", win.Rank)
	return problem.Unprocessable("procurement.award_reason_required", "The award needs a reason: "+why+".",
		problem.FieldError{Field: "reason", Code: "required", Message: why})
}

// winner is the row of the quotation an award names, which must be one
// the comparison ranks (Compared.CanWin): a quotation of this RFQ that
// prices every item and is valid on the comparison's day, told in zone.
// It need not be the lowest. Each refusal names quotation_id.
func (c Comparison) winner(quotation uuid.UUID, zone *time.Location) (Compared, error) {
	i := slices.IndexFunc(c.Rows, func(row Compared) bool { return row.Quotation != nil && row.Quotation.ID == quotation })
	if i < 0 {
		return Compared{}, problem.Invalid("procurement.quotation_not_of_rfq", "quotation_id", "RFQ "+c.RFQ.DocumentNumber+" has no quotation with this id")
	}
	row := c.Rows[i]
	if len(row.Unquoted) > 0 {
		return Compared{}, problem.Invalid("procurement.quotation_incomplete", "quotation_id",
			fmt.Sprintf("the quotation gives no price for the RFQ's items %v; an award goes to a quotation that prices every item", row.Unquoted))
	}
	if row.Expired {
		why := fmt.Sprintf("the quotation was valid until %s, before the legal entity's date today, %s in %s", row.Quotation.ValidUntil.Format(time.DateOnly),
			c.AsOf.Format(time.DateOnly), zone)
		return Compared{}, problem.Unprocessable("procurement.quotation_expired", "The quotation has expired: "+why+".",
			problem.FieldError{Field: "quotation_id", Code: "expired", Message: why})
	}
	return row, nil
}

// contractOf is the contract an award of RFQ r to the row's quotation
// creates: with the quotation's supplier, in the RFQ's currency, the
// RFQ's items at the quoted prices and the quotation's total as its
// target value. Amounts are written as the API's decimals, which
// draftContract parses and checks as it does a buyer's.
func (row Compared) contractOf(r RFQ, in Award) NewContract {
	items := make([]ItemInput, len(row.Priced))
	for i, it := range row.Priced {
		items[i] = ItemInput{MaterialKey: it.MaterialKey, Description: it.Description, Unit: it.Unit, Price: it.UnitPrice.Plain(r.Exponent),
			Currency: r.Currency, PriceUnit: it.PriceUnit, AccountID: it.AccountID}
	}
	return NewContract{LegalEntityID: r.LegalEntityID, SupplierID: row.Supplier.ID, Currency: r.Currency, ValidFrom: *in.ValidFrom, ValidTo: *in.ValidTo,
		Target: row.Total.Plain(r.Exponent), TargetCurrency: r.Currency, Items: items}
}

// awardable compares RFQ r's quotations on its legal entity's date today
// and returns the row of the quotation the award names, which must be
// able to win and whose supplier must still be one an order could be
// placed with, refused by quotation_id; with it the supplier's number
// and the RFQ's other quotations, in the comparison's order (the ranked
// by total, then the rest, each as their suppliers were invited).
func (e *Endpoints) awardable(ctx context.Context, tx pgx.Tx, r RFQ, quotation uuid.UUID) (win Compared, others []uuid.UUID, today time.Time, err error) {
	today, zone, err := e.todayOf(ctx, r.LegalEntityID)
	if err != nil {
		return Compared{}, nil, today, err
	}
	quotations, err := quotationsOf(ctx, tx, r.ID)
	if err != nil {
		return Compared{}, nil, today, err
	}
	c, err := compare(r, quotations, today)
	if err != nil {
		return Compared{}, nil, today, err
	}
	if win, err = c.winner(quotation, zone); err != nil {
		return Compared{}, nil, today, err
	}
	supplier, err := e.supplierAt(ctx, win.Supplier.ID, r.LegalEntityID, "quotation_id")
	if err != nil {
		return Compared{}, nil, today, err
	}
	win.Supplier.Number = supplier.Number
	others = []uuid.UUID{}
	for _, row := range c.Rows {
		if row.Quotation != nil && row.Quotation.ID != quotation {
			others = append(others, row.Quotation.ID)
		}
	}
	return win, others, today, nil
}

// awardRFQ awards a published RFQ to one of its quotations, once: the RFQ
// is read under its row lock at the version the caller read, so two
// awards run one after the other and the second finds it awarded (409);
// the updates are guarded by the states they leave.
func (e *Endpoints) awardRFQ(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in Award) (RFQ, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return RFQ{}, err
	}
	r, err := getRFQ(ctx, tx, id, true)
	if err != nil {
		return RFQ{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return RFQ{}, err
	}
	if r.Status != RFQPublished {
		return RFQ{}, statusConflict("RFQ", r.Status, "award", RFQPublished)
	}
	if err := in.check(r); err != nil {
		return RFQ{}, err
	}
	win, others, today, err := e.awardable(ctx, tx, r, in.QuotationID)
	if err != nil {
		return RFQ{}, err
	}
	if err := in.reasoned(win); err != nil {
		return RFQ{}, err
	}
	doc, err := e.awardedDocument(ctx, tx, r, win, in, today)
	if err != nil {
		return RFQ{}, err
	}
	// The outcomes first: the database lets a quotation's outcome be set
	// only while its RFQ is published, and an RFQ be awarded only with an
	// awarded quotation (000297).
	if _, err := tx.Exec(ctx, `UPDATE procurement.supplier_quotation SET status = CASE WHEN id = $2 THEN $3::text ELSE $4::text END,
			version = version + 1, updated_by = $5 WHERE rfq_id = $1 AND status = $6`,
		id, in.QuotationID, QuotationAwarded, QuotationNotAwarded, p.ID, Recorded); err != nil {
		return RFQ{}, fmt.Errorf("procurement: set the outcomes of the quotations of RFQ %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.rfq SET status = $2, contract_id = $3, purchase_order_id = $4, award_reason = $5, version = version + 1,
			updated_by = $6 WHERE id = $1 AND status = $7`, id, RFQAwarded, doc.contract, doc.order, in.Reason, p.ID, RFQPublished); err != nil {
		return RFQ{}, fmt.Errorf("procurement: award RFQ %s: %w", id, err)
	}
	q := win.Quotation
	detail := map[string]any{"from_status": r.Status, "supplier_quotation_id": q.ID, "supplier_id": q.SupplierID, "contract_id": doc.contract,
		"purchase_order_id": doc.order, "reason": in.Reason, "not_awarded_quotation_ids": others}
	return e.recorded(ctx, tx, id, "award", "awarded", detail, func(stored RFQ) outbox.Event { return doc.event(stored, win, in.Reason) })
}

// awarded is the document an award created: a contract or a purchase
// order, with what its event says of it.
type awarded struct {
	contract, order *uuid.UUID
	event           func(stored RFQ, win Compared, reason *string) outbox.Event
}

// awardedDocument creates what the award creates from the winning
// quotation: a draft contract, or a draft purchase order dated today.
func (e *Endpoints) awardedDocument(ctx context.Context, tx pgx.Tx, r RFQ, win Compared, in Award, today time.Time) (awarded, error) {
	if in.To == AwardToOrder {
		return e.awardedOrder(ctx, tx, r, win, today)
	}
	c, err := e.draftContract(ctx, win.contractOf(r, in), nil)
	if err != nil {
		return awarded{}, err
	}
	return awarded{contract: &c.ID, event: func(stored RFQ, win Compared, reason *string) outbox.Event {
		payload := awardPayload(stored, win, reason)
		payload["contract_id"], payload["contract_document_number"] = c.ID, c.DocumentNumber
		payload["valid_from"], payload["valid_to"] = c.ValidFrom.Format(time.DateOnly), c.ValidTo.Format(time.DateOnly)
		return event("rfq_awarded", "rfq", stored.ID, payload)
	}}, nil
}

// awardPayload is what both award events say: the RFQ, the quotation that
// won, its supplier by id and number, its total and the reason, if any.
func awardPayload(stored RFQ, win Compared, reason *string) map[string]any {
	q := win.Quotation
	payload := map[string]any{"rfq_id": stored.ID, "document_number": stored.DocumentNumber, "legal_entity_id": stored.LegalEntityID,
		"legal_entity_code": stored.CompanyCode, "supplier_quotation_id": q.ID, "supplier_id": q.SupplierID, "supplier_number": win.Supplier.Number,
		"supplier_reference": q.Reference, "total": money.EventAmount(win.Total, stored.Currency, stored.Exponent)}
	if reason != nil {
		payload["reason"] = *reason
	}
	return payload
}
