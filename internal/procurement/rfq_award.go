package procurement

import (
	"context"
	"fmt"
	"slices"
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
// code path (draftContract: its checks, number and audit record), marks
// the quotation awarded and the RFQ's others not awarded, and moves the
// RFQ to awarded, naming the contract, all in the request's transaction.

// RFQAwarded is the status of an RFQ that was awarded to a quotation.
const RFQAwarded = "awarded"

// The outcomes of a supplier quotation once its RFQ is awarded.
const (
	QuotationAwarded    = "awarded"
	QuotationNotAwarded = "not_awarded"
)

// Award is the input of awardRFQ: the quotation that wins and the
// validity of the contract created from it.
type Award struct {
	QuotationID        uuid.UUID
	ValidFrom, ValidTo time.Time
}

// winner is the row of the quotation an award names, which must be one
// the comparison ranks (Compared.CanWin): a quotation of this RFQ that
// prices every item and is valid on the comparison's day, told in zone.
// It need not be the lowest. Each refusal names quotation_id.
func (c Comparison) winner(quotation uuid.UUID, zone *time.Location) (Compared, error) {
	i := slices.IndexFunc(c.Rows, func(row Compared) bool { return row.Quotation != nil && row.Quotation.ID == quotation })
	if i < 0 {
		return Compared{}, invalid("procurement.quotation_not_of_rfq", "quotation_id", "RFQ "+c.RFQ.DocumentNumber+" has no quotation with this id")
	}
	row := c.Rows[i]
	if len(row.Unquoted) > 0 {
		return Compared{}, invalid("procurement.quotation_incomplete", "quotation_id",
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
	return NewContract{LegalEntityID: r.LegalEntityID, SupplierID: row.Supplier.ID, Currency: r.Currency, ValidFrom: in.ValidFrom, ValidTo: in.ValidTo,
		Target: row.Total.Plain(r.Exponent), TargetCurrency: r.Currency, Items: items}
}

// awardable compares RFQ r's quotations on its legal entity's date today
// and returns the row of the quotation the award names, which must be
// able to win and whose supplier must still be one an order could be
// placed with, refused by quotation_id; with it the supplier's number
// and the RFQ's other quotations, in the order their suppliers were
// invited.
func (e *Endpoints) awardable(ctx context.Context, tx pgx.Tx, r RFQ, quotation uuid.UUID) (win Compared, others []uuid.UUID, err error) {
	today, zone, err := e.todayOf(ctx, r.LegalEntityID)
	if err != nil {
		return Compared{}, nil, err
	}
	quotations, err := quotationsOf(ctx, tx, r.ID)
	if err != nil {
		return Compared{}, nil, err
	}
	c, err := compare(r, quotations, today)
	if err != nil {
		return Compared{}, nil, err
	}
	if win, err = c.winner(quotation, zone); err != nil {
		return Compared{}, nil, err
	}
	supplier, err := e.supplierAt(ctx, win.Supplier.ID, r.LegalEntityID, "quotation_id")
	if err != nil {
		return Compared{}, nil, err
	}
	win.Supplier.Number = supplier.Number
	others = []uuid.UUID{}
	for _, row := range c.Rows {
		if row.Quotation != nil && row.Quotation.ID != quotation {
			others = append(others, row.Quotation.ID)
		}
	}
	return win, others, nil
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
	win, others, err := e.awardable(ctx, tx, r, in.QuotationID)
	if err != nil {
		return RFQ{}, err
	}
	c, err := e.draftContract(ctx, win.contractOf(r, in), nil)
	if err != nil {
		return RFQ{}, err
	}
	// The outcomes first: the database lets a quotation's outcome be set
	// only while its RFQ is published, and an RFQ be awarded only with an
	// awarded quotation (000278).
	if _, err := tx.Exec(ctx, `UPDATE procurement.supplier_quotation SET status = CASE WHEN id = $2 THEN $3::text ELSE $4::text END,
			version = version + 1, updated_by = $5 WHERE rfq_id = $1 AND status = $6`,
		id, in.QuotationID, QuotationAwarded, QuotationNotAwarded, p.ID, Recorded); err != nil {
		return RFQ{}, fmt.Errorf("procurement: set the outcomes of the quotations of RFQ %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.rfq SET status = $2, contract_id = $3, version = version + 1, updated_by = $4 WHERE id = $1 AND status = $5`,
		id, RFQAwarded, c.ID, p.ID, RFQPublished); err != nil {
		return RFQ{}, fmt.Errorf("procurement: award RFQ %s: %w", id, err)
	}
	q := win.Quotation
	detail := map[string]any{"from_status": r.Status, "supplier_quotation_id": q.ID, "supplier_id": q.SupplierID, "contract_id": c.ID,
		"not_awarded_quotation_ids": others}
	return e.recorded(ctx, tx, id, "award", "awarded", detail, func(stored RFQ) outbox.Event {
		return event("rfq_awarded", "rfq", stored.ID, map[string]any{"rfq_id": stored.ID, "document_number": stored.DocumentNumber,
			"legal_entity_id": stored.LegalEntityID, "legal_entity_code": stored.CompanyCode, "supplier_quotation_id": q.ID,
			"supplier_id": q.SupplierID, "supplier_number": win.Supplier.Number, "supplier_reference": q.Reference, "contract_id": c.ID,
			"contract_document_number": c.DocumentNumber, "valid_from": c.ValidFrom.Format(time.DateOnly), "valid_to": c.ValidTo.Format(time.DateOnly),
			"total": money.EventAmount(win.Total, stored.Currency, stored.Exponent)})
	})
}
