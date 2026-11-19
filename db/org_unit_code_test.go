package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestOrgUnitCodes (migrations 000412-000414; tracker leaf "Storage
// location codes unique per plant", ADR-0082 As built): an org unit's code
// is unique in its legal entity and kind (org_unit_code_once), but a
// storage location's is unique under its plant (storage_location_code_once,
// as SAP keys LGORT within its WERKS), one without a plant in its legal
// entity; a move under a plant that has the code is refused as well.
func TestOrgUnitCodes(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(f fixture, p1, p2 uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a plant code twice", func(f fixture, p1, p2 uuid.UUID) []stmt { return []stmt{orgUnit(f, id(), "plant", "P1", nil)} }, "org_unit_code_once"},
		{"a purchasing organization code twice", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "purchasing_org", "O1", nil), orgUnit(f, id(), "purchasing_org", "O1", nil)}
		}, "org_unit_code_once"},
		{"a plant's code for a storage location", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "storage_location", "P1", &p1)}
		}, ""},
		{"a storage location code at two plants", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "storage_location", "S1", &p1), orgUnit(f, id(), "storage_location", "S1", &p2)}
		}, ""},
		{"a storage location code twice at a plant", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "storage_location", "S1", &p1), orgUnit(f, id(), "storage_location", "S1", &p1)}
		}, "storage_location_code_once"},
		{"a storage location code without a plant and under one", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "storage_location", "S1", nil), orgUnit(f, id(), "storage_location", "S1", &p1)}
		}, ""},
		{"a storage location code twice without a plant", func(f fixture, p1, p2 uuid.UUID) []stmt {
			return []stmt{orgUnit(f, id(), "storage_location", "S1", nil), orgUnit(f, id(), "storage_location", "S1", nil)}
		}, "storage_location_code_once"},
		{"a storage location moved under a plant with its code", func(f fixture, p1, p2 uuid.UUID) []stmt {
			s := id()
			return []stmt{orgUnit(f, id(), "storage_location", "S1", &p1), orgUnit(f, s, "storage_location", "S1", &p2),
				step(`UPDATE tenancy.org_unit SET parent_id = $1, version = version + 1 WHERE id = $2`, p1, s)}
		}, "storage_location_code_once"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p1, p2 := fixtureIDs(), id(), id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				if err := f.seed(tx); err != nil {
					return err
				}
				return all(tx, append([]stmt{orgUnit(f, p1, "plant", "P1", nil), orgUnit(f, p2, "plant", "P2", nil)}, c.run(f, p1, p2)...)...)
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

// orgUnit is an org unit of the fixture's legal entity.
func orgUnit(f fixture, unit uuid.UUID, kind, code string, parent *uuid.UUID) stmt {
	return step(`INSERT INTO tenancy.org_unit (tenant_id, id, legal_entity_id, kind, code, name, parent_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5, $6, 'active', $7, $7)`, f.tenant, unit, f.entity, kind, code, parent, f.actor)
}
