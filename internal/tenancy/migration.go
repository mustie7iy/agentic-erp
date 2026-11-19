package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// OrgUnitMigration creates the plants and storage locations of an SAP
// migration run for the integration context (ADR-0082 As built S2o): SAP's
// plants (T001W) and storage locations (T001L) become org units, a
// storage location under its plant, active at once, since the materials
// the run loads are extended to them. The run is the governed act (a
// reconciled load a person started), as it is for the centers and
// suppliers it creates.
// It reads org units as Directory does.
type OrgUnitMigration struct{ Directory }

// CreateActiveOrgUnit creates an org unit as CreateOrgUnit does and
// activates it, each step audited and published as the API's are.
func (OrgUnitMigration) CreateActiveOrgUnit(ctx context.Context, in NewOrgUnit) (Unit, error) {
	u, err := CreateOrgUnit(ctx, in)
	if err != nil {
		return Unit{}, err
	}
	u, err = changeOrgUnitStatus(ctx, u.ID, func(int32) error { return nil }, activateOU, "")
	if err != nil {
		return Unit{}, err
	}
	return Unit{ID: u.ID, LegalEntityID: u.LegalEntityID, Kind: u.Kind, Code: u.Code, Name: u.Name, Status: u.Status, ParentID: u.ParentID}, nil
}

// OrgUnitByCode returns the org unit of the kind with the code in the
// legal entity, which holds one at most (its unique key), or ok = false.
// Company access is not applied, as OrgUnits does not apply it.
func (Directory) OrgUnitByCode(ctx context.Context, legalEntity uuid.UUID, kind, code string) (Unit, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Unit{}, false, err
	}
	u := Unit{LegalEntityID: legalEntity, Kind: kind, Code: code}
	err = tx.QueryRow(ctx, `SELECT id, name, status, parent_id FROM tenancy.org_unit WHERE legal_entity_id = $1 AND kind = $2 AND code = $3`,
		legalEntity, kind, code).Scan(&u.ID, &u.Name, &u.Status, &u.ParentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Unit{}, false, nil
	}
	if err != nil {
		return Unit{}, false, fmt.Errorf("tenancy: read %s %s of legal entity %s: %w", kind, code, legalEntity, err)
	}
	return u, true, nil
}
