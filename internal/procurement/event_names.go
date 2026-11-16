package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// record audits a change with its events, as audit.Record does, after
// naming what each event identifies (nameEvent), so every procurement event a
// consumer reads names the company and the supplier beside their ids
// (REVIEW.md, an identifier without the name its consumer needs;
// review:batch28).
func (e *Endpoints) record(ctx context.Context, action, objectType string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	for _, ev := range events {
		payload, _ := ev.Payload.(map[string]any) // event gives every procurement event a map
		if err := e.nameEvent(ctx, payload); err != nil {
			return err
		}
	}
	return audit.Record(ctx, action, objectType, id, detail, events...)
}

// nameEvent adds the company's code beside an event's legal_entity_id and the
// supplier's business partner number beside its supplier_id, when the
// payload has them and does not already name them. Never the supplier's
// name: an event may be offered to webhooks, and a supplier may be a
// person (product owner 2026-11-05, ADR-0041). A company or supplier the
// directory does not hold fails the write rather than publishing an event
// that leaves it unnamed: the document names it, so a missing one is an
// inconsistency.
func (e *Endpoints) nameEvent(ctx context.Context, payload map[string]any) error {
	if le, ok := payload["legal_entity_id"].(uuid.UUID); ok {
		if _, done := payload["legal_entity_code"]; !done {
			ent, found, err := e.LegalEntities.Entity(ctx, le)
			if err != nil {
				return fmt.Errorf("procurement: name the company %s of an event: %w", le, err)
			}
			if !found {
				return fmt.Errorf("procurement: name the company %s of an event: no such legal entity", le)
			}
			payload["legal_entity_code"] = ent.Code
		}
	}
	supplier, ok := payload["supplier_id"].(uuid.UUID)
	if p, isPtr := payload["supplier_id"].(*uuid.UUID); isPtr && p != nil {
		supplier, ok = *p, true
	}
	if _, done := payload["supplier_number"]; !ok || done {
		return nil
	}
	s, err := idref.Resolve(ctx, e.Partners.Names, []uuid.UUID{supplier}, supplierRef)
	if err != nil {
		return fmt.Errorf("procurement: name the supplier %s of an event: %w", supplier, err)
	}
	payload["supplier_number"] = s[supplier].Number
	return nil
}
