package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// An RFQ from a requisition (T82e, ADR-0069 As built): a buyer asks the
// suppliers to quote for what an approved requisition asks for. The
// requisition is sourced as a whole, as it is ordered as a whole: every
// live item becomes an RFQ item naming it, and the requisition is
// in_sourcing until an award to a purchase order orders it (T82f).

// sourced reads the requisition an RFQ is created from under its row
// lock and returns the RFQ's currency and items from it: an approved
// requisition of the legal entity, in the currency asked for, if any;
// items are not sent beside it. Its need date is each item's delivery
// date. createRFQ moves it to in_sourcing.
func (e *Endpoints) sourced(ctx context.Context, tx pgx.Tx, le uuid.UUID, in NewRFQ) (string, []Item, *Requisition, error) {
	if len(in.Items) > 0 {
		return "", nil, nil, problem.Invalid("procurement.rfq_items_from_requisition", "items",
			"an RFQ from a requisition takes the requisition's items; send none")
	}
	// Sourcing a requisition reserves it for this RFQ; creating an RFQ is a
	// write already, which an agent needs level 2 for (ADR-0036).
	req, err := getRequisition(ctx, tx, *in.RequisitionID, true)
	if err != nil {
		return "", nil, nil, problem.Reference(err, "requisition_id", "requisition", "no requisition with this id that you can access")
	}
	switch {
	case req.Status != Approved:
		return "", nil, nil, statusConflict("requisition", req.Status, "sourcing", Approved)
	case req.LegalEntityID != le:
		return "", nil, nil, problem.Invalid("procurement.requisition_mismatch", "requisition_id", "a requisition of the RFQ's legal entity")
	case in.Currency != "" && in.Currency != req.Currency:
		return "", nil, nil, problem.Invalid("procurement.requisition_mismatch", "currency", "the requisition's currency, "+req.Currency)
	}
	items, err := e.rfqItems(ctx, le, sourcedInputs(req))
	if err != nil {
		return "", nil, nil, err
	}
	for i := range items {
		items[i].DeliveryDate, items[i].RequisitionItemNo = req.NeededBy, &req.Items[i].No
	}
	return req.Currency, items, &req, nil
}

// sourcedInputs are the requisition's items as an RFQ's: each keyed by
// its catalog key, or by the requisition's number and its item number
// when it has none or shares it with another item, so that every key is
// once in the RFQ (ADR-0069 §2).
func sourcedInputs(r Requisition) []ItemInput {
	keys := map[string]int{}
	for _, it := range r.Items {
		keys[it.MaterialKey]++
	}
	in := make([]ItemInput, len(r.Items))
	for i, it := range r.Items {
		key := it.MaterialKey
		if key == "" || keys[key] > 1 {
			key = fmt.Sprintf("%s/%d", r.DocumentNumber, it.No)
		}
		in[i] = ItemInput{MaterialKey: key, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, AccountID: it.AccountID}
	}
	return in
}
