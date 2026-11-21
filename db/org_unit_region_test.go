package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestOrgUnitRegion (migration 000431; ADR-0082 As built S2j, the plant
// required): only a plant has a region, in the form of an ISO 3166-2
// subdivision code; that it is one of its company's country is the
// tenancy context's check.
func TestOrgUnitRegion(t *testing.T) {
	need(t)
	const unit = `INSERT INTO tenancy.org_unit (tenant_id, id, legal_entity_id, kind, code, name, region, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $3, 'U1', 'Unit', $4, 'active', $5, $5)`
	cases := []struct {
		name, kind string
		region     *string
		want       string // empty: allowed
	}{
		{"a plant's emirate", "plant", text("AE-DU"), ""},
		{"a plant without one", "plant", nil, ""},
		{"a storage location's", "storage_location", text("AE-DU"), "org_unit_region"},
		{"a country, not a subdivision", "plant", text("AE"), "org_unit_region"},
		{"a name, not a code", "plant", text("Dubai"), "org_unit_region"},
		{"a lower-case code", "plant", text("ae-du"), "org_unit_region"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fixtureIDs()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				if err := f.seed(tx); err != nil {
					return err
				}
				return all(tx, step(unit, f.tenant, f.entity, c.kind, c.region, f.actor))
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}
