package partner

import (
	"context"

	"github.com/google/uuid"
)

// Migration loads business partners for the integration context's
// migration runs. The run itself is the governed act (a reconciled load
// started by a person with integration.migration.run), so the changes it
// makes are applied at once instead of waiting for approval requests.
type Migration struct {
	Ports Ports
}

// Create creates the partner and applies its create change.
func (m Migration) Create(ctx context.Context, in NewPartner) (uuid.UUID, error) {
	bp, c, err := CreatePartner(ctx, m.Ports, in)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = ApplyChange(ctx, m.Ports.Schemas, c.ID)
	return bp.ID, err
}

// AddCompanies sets the partner up as a supplier of more legal entities.
func (m Migration) AddCompanies(ctx context.Context, id uuid.UUID, companies []SupplierCompanyInput, reason string) error {
	c, err := RequestUpdate(ctx, m.Ports, id, Changes{SupplierCompanies: companies}, reason)
	if err != nil {
		return err
	}
	_, err = ApplyChange(ctx, m.Ports.Schemas, c.ID)
	return err
}

// Block blocks the partner.
func (Migration) Block(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := Block(ctx, id, func(int32) error { return nil }, reason)
	return err
}
