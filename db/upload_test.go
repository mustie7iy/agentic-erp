package db

import (
	"testing"

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
