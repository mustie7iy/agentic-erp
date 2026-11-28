package inventory

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Reverse reverses a material document as a whole, once (ADR-0082 §7): a
// document of the reversal movement type whose lines are the original's
// with their quantities negated, posted on the company's date today and
// naming the original. Documents are insert-only, so reversals of one
// document serialize on a lock named after it, and a second finds the
// first (409 inventory.already_reversed; material_document_reversed_once
// as well). The reversal moves the stock back under the same locks and
// checks as a posting (reversal); a material blocked since is still
// reversed. A delivery's goods issue is reversed by shipping
// (ReverseForDelivery), with the order's delivered quantity; a
// customer's return into stock (653) is not reversed yet (S2b4).
func (e *Endpoints) Reverse(ctx context.Context, id uuid.UUID, ifMatch func(Document) error) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	orig, err := original(ctx, tx, id)
	if err != nil {
		return Document{}, err
	}
	if err := ifMatch(orig); err != nil {
		return Document{}, err
	}
	// A reversal names its original, and a reversed document its
	// reversal, by id and number alike (documentSelect's joins).
	mt := movementTypes[orig.MovementType]
	switch {
	case mt.reverses != "":
		return Document{}, problem.Conflict("inventory.cannot_reverse_reversal", "Material document "+orig.Number+" is itself a reversal, of "+
			*orig.ReversalOfNumber+".")
	case orig.ReversedByID != nil:
		return Document{}, problem.Conflict("inventory.already_reversed", "Material document "+orig.Number+" is reversed already, by "+
			*orig.ReversedByNumber+".")
	case mt.count:
		return Document{}, problem.Conflict("inventory.reverse_count", "Material document "+orig.Number+" posts a physical inventory count's "+
			"differences; it is not reversed: a new count of the material corrects the stock.")
	case orig.transport():
		// The order's status and its stock in transit move with it: Later
		// (As built S2h).
		return Document{}, problem.Conflict("inventory.reverse_stock_transport", "Material document "+orig.Number+" is the goods issue or receipt of "+
			"stock transport order "+*orig.Reference+"; it is not reversed yet.")
	case mt.order:
		// Its GR/IR and the order item's received quantity move with it: a
		// goods receipt (101) or a return to the supplier (122, S1e2).
		return Document{}, problem.Conflict("inventory.reverse_goods_receipt", "Material document "+orig.Number+" is goods receipt or return to the "+
			"supplier "+*orig.Reference+" of a purchase order; reverse the goods movement (reverseGoodsReceipt), which reverses its stock too.")
	case mt.intercompany:
		// The order's issued quantity, its receipts and its billing move
		// with it: Later (ADR-0082 As built S2i).
		return Document{}, problem.Conflict("inventory.reverse_stock_transport", "Material document "+orig.Number+" is the goods issue of "+
			"intercompany stock transport order "+*orig.Reference+"; it is not reversed yet.")
	case mt.delivery:
		// The order's delivered quantity moves with it, and its billing
		// is cancelled first: shipping reverses it (S2b3).
		return Document{}, problem.Conflict("inventory.reverse_goods_issue", "Material document "+orig.Number+" is the goods issue of outbound delivery "+
			*orig.Reference+"; reverse the delivery's goods issue (reverseOutboundDeliveryGoodsIssue), which reverses its stock too.")
	case mt.customerReturn:
		// The return receipt's cost and its returns order move with it:
		// Later (As built S2b4).
		return Document{}, problem.Conflict("inventory.reverse_customer_return", "Material document "+orig.Number+" receives a customer's return of "+
			"returns order "+*orig.Reference+" into stock; it is not reversed yet.")
	}
	return e.reversal(ctx, tx, p, orig, nil)
}

// ReverseForDelivery posts the reversal (602) of an outbound delivery's
// goods issue's material document (601), once, in the request's
// transaction (S2b3; ADR-0083 As built S2b3): on the company's date
// today, its reference the delivery's number, its lines negated, last
// first, each given back at what its original line took (Reverse), so
// the stock and the cost of goods sold return what the goods issue
// moved. Shipping holds the delivery's row lock, which reverses its goods
// issue once; the document's own lock serializes it with Reverse.
func (e *Endpoints) ReverseForDelivery(ctx context.Context, id uuid.UUID, reference string) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	orig, err := original(ctx, tx, id)
	if err != nil {
		return Document{}, err
	}
	if orig.MovementType != "601" || orig.ReversedByID != nil {
		// shipping reverses a delivery's goods issue once, and only its 601.
		return Document{}, fmt.Errorf("inventory: material document %s is a %s, reversed by %v: no goods issue of a delivery to reverse", orig.Number,
			orig.MovementType, orig.ReversedByID)
	}
	return e.reversal(ctx, tx, p, orig, &reference)
}

// original reads a document to reverse under a lock named after it:
// documents are insert-only, so reversals of one serialize on it, and
// the second finds the first.
func original(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Document, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('inventory.material_document:' || $1::text, 0))`, id); err != nil {
		return Document{}, fmt.Errorf("inventory: reverse material document %s: lock: %w", id, err)
	}
	return load(ctx, tx, id)
}

// reversal posts the reversal of orig, of its movement type's reversal
// type, on the company's date today, with the reference, if any: its
// lines negated (negated), moving the stock back under the same locks
// and checks as a posting; its audit record names the original.
func (e *Endpoints) reversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orig Document, reference *string) (Document, error) {
	r, err := e.documentRefs(ctx, []Document{orig})
	if err != nil {
		return Document{}, err
	}
	c, err := e.companyOf(ctx, orig.LegalEntityID)
	if err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: c.le, movementType: movementTypes[orig.MovementType].reversal, day: c.day.Date, reversalOf: &orig.ID,
		reference: reference, lines: negated(orig.Lines)}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.reverse", d, r, map[string]any{"reversal_of_id": orig.ID, "reversal_of_number": orig.Number})
}

// negated are the lines with their quantities negated, last first and
// numbered again: what moves the stock back through the states the
// original went through, so a reversal is refused only when the stock
// it gives back has gone (a chain of transfers, reversed in line order,
// would issue from a location before giving it back).
func negated(lines []Line) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		l.Quantity = money.PlainQuantity(new(big.Rat).Neg(money.ParseRat(l.Quantity)))
		l.LineNo = int16(len(lines) - i)    // at most 200 lines (movement_line_no_check)
		worth := -(l.Amount + l.Difference) // what the original line was valued at, given back
		l.target, l.Amount, l.Difference = &worth, 0, 0
		out[len(lines)-1-i] = l
	}
	return out
}
