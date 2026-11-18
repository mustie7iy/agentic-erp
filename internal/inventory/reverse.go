package inventory

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"

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
// checks as a posting; a material blocked since is still reversed.
func (e *Endpoints) Reverse(ctx context.Context, id uuid.UUID, ifMatch func(Document) error) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('inventory.material_document:' || $1::text, 0))`, id); err != nil {
		return Document{}, fmt.Errorf("inventory: reverse material document %s: lock: %w", id, err)
	}
	orig, err := load(ctx, tx, id)
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
	}
	r, err := e.documentRefs(ctx, []Document{orig})
	if err != nil {
		return Document{}, err
	}
	le, day, err := e.today(ctx, orig.LegalEntityID)
	if err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: le, movementType: mt.reversal, day: day.Date, reversalOf: &orig.ID, lines: negated(orig.Lines)}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.reverse", d, r, map[string]any{"reversal_of_id": orig.ID, "reversal_of_number": orig.Number})
}

// negated are the lines with their quantities negated: what moves the
// stock back.
func negated(lines []Line) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		l.Quantity = money.PlainQuantity(new(big.Rat).Neg(money.ParseRat(l.Quantity)))
		out[i] = l
	}
	return out
}
