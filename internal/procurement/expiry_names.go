package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
)

// supplierRef is how a supplier the partner context does not hold is
// reported (idref.Resolve).
var supplierRef = idref.Kind{Name: "supplier", Context: "partner"}

// eventNames names a round's contracts in their events: the company by its
// code beside legal_entity_id, the supplier by its number and name beside
// supplier_id (REVIEW.md, an identifier without the name its consumer
// needs), so a consumer alerting a buyer need not look them up. A round
// reads each legal entity's code once; the supplier is read per event, as
// a round expires or alerts few contracts of a tenant.
type eventNames struct {
	entities LegalEntities
	partners SupplierNames
	codes    map[uuid.UUID]string
}

// names is a round's eventNames.
func (x *ContractExpirer) names() *eventNames {
	return &eventNames{entities: x.LegalEntities, partners: x.Partners, codes: map[uuid.UUID]string{}}
}

// name adds the company's code and the supplier's number and name of
// contract c to its event's payload. A legal entity or supplier its
// context does not hold fails the round rather than publishing an event
// that leaves it unnamed: the contract's own references, so a missing one
// is an inconsistency.
func (n *eventNames) name(ctx context.Context, c Contract, payload map[string]any) error {
	code, ok := n.codes[c.LegalEntityID]
	if !ok {
		le, found, err := n.entities.Entity(ctx, c.LegalEntityID)
		if err != nil {
			return fmt.Errorf("procurement: name legal entity %s of contract %s: %w", c.LegalEntityID, c.ID, err)
		}
		if !found {
			return fmt.Errorf("procurement: name legal entity %s of contract %s: not held by the tenancy context", c.LegalEntityID, c.ID)
		}
		code = le.Code
		n.codes[c.LegalEntityID] = code
	}
	named, err := idref.Resolve(ctx, n.partners.Names, []uuid.UUID{c.SupplierID}, supplierRef)
	if err != nil {
		return fmt.Errorf("procurement: name the supplier of contract %s: %w", c.ID, err)
	}
	s := named[c.SupplierID]
	payload["legal_entity_code"], payload["supplier_number"], payload["supplier_name"] = code, s.Number, s.Name
	return nil
}
