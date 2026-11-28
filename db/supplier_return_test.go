package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// supplierReturnDocument is a material document of the scene's company
// with a reversal and a reference, either nil for none.
const supplierReturnDocument = `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
	reversal_of_id, reference, created_by) VALUES ($1, $2, $3, 2026, $4, $5, '2026-11-27', $6, $7, $1)`

// TestSupplierReturnDocuments (S1e2, migrations 000502 and 000503;
// ADR-0082 As built S1e2): a return to the supplier (122) and its
// reversal (123) name the goods movement as their reference, a 123
// reverses a 122 and nothing else, and no other movement type is added.
func TestSupplierReturnDocuments(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		run  func(s inventoryScene) []stmt
		want string // empty: allowed
	}{
		{"a return", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "122", nil, "GR1")}
		}, ""},
		{"its reversal", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "122", nil, "GR1"),
				step(supplierReturnDocument, s.tenant, id(), s.company, "MD2", "123", s.doc, "GR2")}
		}, ""},
		{"a return without its reference", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "122", nil)} },
			"material_document_reference"},
		// A 122 naming an original is refused by the reversal trigger, which
		// fires first: no type precedes it.
		{"a 122 reversing a document", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "101", nil, "GR1"),
				step(supplierReturnDocument, s.tenant, id(), s.company, "MD2", "122", s.doc, "GR2")}
		}, "a reversal is of its original"},
		{"a 123 without its reference", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "122", nil, "GR1"), s.document(id(), "MD2", "123", &s.doc)}
		}, "material_document_reference"},
		{"a 123 reversing nothing", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "123", nil, "GR1")}
		}, "material_document_reversal_check"},
		{"a 123 of a 101", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "101", nil, "GR1"),
				step(supplierReturnDocument, s.tenant, id(), s.company, "MD2", "123", s.doc, "GR2")}
		}, "a reversal is of its original"},
		{"a 124", func(s inventoryScene) []stmt {
			return []stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "124", nil, "GR1")}
		}, "material_document_movement_type_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := inventoryScene{tenant: id(), company: id(), doc: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestSupplierReturnsDownRefused: 000502's down migration refuses to drop
// a return to the supplier (06 §6.4: a down migration loses no data),
// reading every tenant's rows; a 123 always reverses a 122, so a return
// stands for both.
func TestSupplierReturnsDownRefused(t *testing.T) {
	need(t)
	s := inventoryScene{tenant: id(), company: id(), doc: id()}
	downRefused(t, migrationNamed(t, "000502_supplier_returns").Down, s.tenant,
		[]stmt{step(supplierReturnDocument, s.tenant, s.doc, s.company, "MD1", "122", nil, "GR1")}, "returns of stock items to the supplier exist")
}
