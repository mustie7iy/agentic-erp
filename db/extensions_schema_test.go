package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestExtensionsSchemaVersion: the custom field schema version written
// with a row (000113, ADR-0046) is a version number, 1 or more, or none.
func TestExtensionsSchemaVersion(t *testing.T) {
	f := newFixture(t)
	var po uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, nil); return err })
	inv, insert := f.invoiceOf("invoice", nil, nil, 100)
	mustApp(t, f.tenant, insert)
	for _, c := range []struct{ table, constraint string }{
		{"payables.supplier_invoice", "supplier_invoice_extensions_schema_check"},
		{"procurement.purchase_order", "purchase_order_extensions_schema_check"},
		{"partner.business_partner", "business_partner_extensions_schema_check"},
	} {
		row := map[string]uuid.UUID{"payables.supplier_invoice": inv, "procurement.purchase_order": po, "partner.business_partner": f.supplier}[c.table]
		set := func(v any) error {
			return asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `UPDATE `+c.table+` SET extensions_schema = $2, version = version + 1 WHERE id = $1`, row, v)
			})
		}
		t.Run(c.table, func(t *testing.T) {
			failsWith(t, set(0), c.constraint)
			for _, v := range []any{1, nil} {
				if err := set(v); err != nil {
					t.Errorf("extensions_schema %v: got %v, want it written", v, err)
				}
			}
		})
	}
}
