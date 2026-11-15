package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Direct uploads (ADR-0020, migration 000188): an upload's bytes go to the
// store's presigned URL or to the API's own signed path, and to nothing
// else.
func TestUploadVia(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name string
		via  string // "": the column's default
		want string // empty: allowed
	}{
		{"by default to the store", "", ""},
		{"to the store", "store", ""},
		{"through the API", "api", ""},
		{"anywhere else", "bucket", "upload_via_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cols, vals, args := "", "", []any{f.tenant, id(), f.actor}
			if c.via != "" {
				cols, vals, args = ", via", ", $4", append(args, c.via)
			}
			expect(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payables.upload (tenant_id, id, purpose, content_type, max_bytes, object_ref, status, expires_at, created_by`+cols+`)
					VALUES ($1, $2, 'supplier_invoice', 'application/pdf', 10, 'tenants/x/uploads/u', 'open', now(), $3`+vals+`)`, args...)
			}), c.want)
		})
	}
}

// TestUploadPurposes (T90a, migrations 000333 and 000334; ADR-0020): an
// upload carries a supplier invoice or a customer's purchase order, and
// nothing else; the check is validated.
func TestUploadPurposes(t *testing.T) {
	need(t)
	f := newFixture(t)
	for purpose, want := range map[string]string{"supplier_invoice": "", "sales_order": "", "photo": "upload_purpose_check"} {
		t.Run(purpose, func(t *testing.T) {
			expect(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payables.upload (tenant_id, id, purpose, content_type, max_bytes, object_ref, status, expires_at, created_by)
					VALUES ($1, $2, $3, 'text/plain', 10, 'tenants/x/uploads/u', 'open', now(), $4)`, f.tenant, id(), purpose, f.actor)
			}), want)
		})
	}
	var valid bool
	if err := testDB.QueryRow(t.Context(), `SELECT convalidated FROM pg_constraint WHERE conname = 'upload_purpose_check'`).Scan(&valid); err != nil || !valid {
		t.Errorf("upload_purpose_check validated: got %v %v, want true", valid, err)
	}
}

// TestSalesOrderCaptureRules (T90a, migration 000333): an order captured
// from an upload keeps the upload and what was read, both or neither, what
// was read an object; one upload gives one order. The revert is refused
// while a captured order exists.
func TestSalesOrderCaptureRules(t *testing.T) {
	need(t)
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
		net_amount, status, capture_upload_id, capture, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $1, $1, '2026-03-12', 'EUR', 'NT30', 100, 'draft', $3, $4, $1, $1)`
	upload := id()
	for _, c := range []struct {
		name string
		run  func(tenant uuid.UUID) []stmt
		want string
	}{
		{"captured", func(t uuid.UUID) []stmt { return []stmt{step(order, t, "SO1", upload, `{"items": []}`)} }, ""},
		{"by hand", func(t uuid.UUID) []stmt { return []stmt{step(order, t, "SO1", nil, nil)} }, ""},
		{"an upload without what was read", func(t uuid.UUID) []stmt { return []stmt{step(order, t, "SO1", upload, nil)} }, "sales_order_capture_check"},
		{"what was read without its upload", func(t uuid.UUID) []stmt { return []stmt{step(order, t, "SO1", nil, `{}`)} }, "sales_order_capture_check"},
		{"what was read not an object", func(t uuid.UUID) []stmt { return []stmt{step(order, t, "SO1", upload, `[]`)} }, "sales_order_capture_check"},
		{"one upload, two orders", func(t uuid.UUID) []stmt {
			return []stmt{step(order, t, "SO1", upload, `{}`), step(order, t, "SO2", upload, `{}`)}
		}, "sales_order_one_per_upload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant := id()
			expect(t, asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, c.run(tenant)...) }), c.want)
		})
	}
	for name, kept := range map[string]func(tenant uuid.UUID) stmt{
		"the revert with a captured order": func(tenant uuid.UUID) stmt { return step(order, tenant, "SO1", upload, `{}`) },
		"the revert with an order's upload": func(tenant uuid.UUID) stmt {
			return step(`INSERT INTO payables.upload (tenant_id, id, purpose, content_type, max_bytes, object_ref, status, expires_at, created_by)
				VALUES ($1, gen_random_uuid(), 'sales_order', 'text/plain', 10, 'tenants/x/uploads/u', 'open', now(), $1)`, tenant)
		},
	} {
		t.Run(name, func(t *testing.T) {
			down := migrationNamed(t, "000333_sales_order_capture").Down
			tx, err := testDB.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			tenant := id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), kept(tenant)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "reverting would lose the documents they were read from")
		})
	}
}
