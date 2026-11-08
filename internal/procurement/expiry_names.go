package procurement

import (
	"context"
	"fmt"
	"maps"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// supplierRef is how a supplier the partner context does not hold is
// reported (idref.Resolve).
var supplierRef = idref.Kind{Name: "supplier", Context: "partner"}

// eventNames names a round's contracts in their events: the company by its
// code beside legal_entity_id, the supplier by its number and name beside
// supplier_id (REVIEW.md, an identifier without the name its consumer
// needs), so a consumer alerting a buyer need not look them up. The code is
// the one the round read when it dated the legal entity (companyDays), so
// the legal entity is read once a round; the supplier is read per event, as
// a round expires or alerts few contracts of a tenant.
type eventNames struct {
	partners SupplierNames
}

// names is a round's eventNames.
func (x *ContractExpirer) names() *eventNames {
	return &eventNames{partners: x.Partners}
}

// name adds the company's code (day's) and the supplier's number and name
// of contract c to its event's payload. A supplier the partner context does
// not hold fails the round rather than publishing an event that leaves it
// unnamed: it is the contract's own reference, so a missing one is an
// inconsistency (a legal entity the directory does not know already failed
// it in companyDays).
func (n *eventNames) name(ctx context.Context, c Contract, day companyDay, payload map[string]any) error {
	named, err := idref.Resolve(ctx, n.partners.Names, []uuid.UUID{c.SupplierID}, supplierRef)
	if err != nil {
		return fmt.Errorf("procurement: name the supplier of contract %s: %w", c.ID, err)
	}
	s := named[c.SupplierID]
	payload["legal_entity_code"], payload["supplier_number"], payload["supplier_name"] = day.code, s.Number, s.Name
	return nil
}

// publish names contract c in payload and writes its event twice: .v1 with
// the supplier's name, and .v2 without it, which is the one offered to
// webhooks. A supplier's name never goes to a webhook (product owner
// 2026-11-05, security posture: a partner may be a person; ADR-0041), and
// removing a field from .v1 would break its consumers, so .v1 is no longer
// offered and is published beside .v2 until 2027-11-05 (ADR-0041: a .vN+1
// alongside .vN for 12 months).
func (n *eventNames) publish(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, name string, c Contract, day companyDay, payload map[string]any) error {
	if err := n.name(ctx, c, day, payload); err != nil {
		return err
	}
	if _, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", event(name, "contract", c.ID, payload)); err != nil {
		return err
	}
	v2 := maps.Clone(payload)
	delete(v2, "supplier_name")
	e := event(name, "contract", c.ID, v2)
	e.Version = 2
	_, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", e)
	return err
}
