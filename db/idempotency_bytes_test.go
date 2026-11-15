package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestIdempotencyKeyOneBody (T95d, migration 000331; ADR-0008): a stored
// answer is JSON (response_body) or bytes (response_bytes, an Excel
// workbook), never both.
func TestIdempotencyKeyOneBody(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name, json string // json: "" stores none
		bytes      []byte
		want       string // empty: allowed
	}{
		{"JSON", `{"a": 1}`, nil, ""},
		{"bytes", "", []byte("PK"), ""},
		{"neither", "", nil, ""},
		{"both", `{"a": 1}`, []byte("PK"), "idempotency_key_one_body"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var body *string
			if c.json != "" {
				body = &c.json
			}
			expect(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO platform.idempotency_key (tenant_id, key, principal_id, request_hash, status, response_code, response_body, response_bytes, expires_at)
					VALUES ($1, $2, $3, '\x00', 'completed', 200, $4::jsonb, $5, now())`, f.tenant, "key-"+id().String(), f.actor, body, c.bytes)
			}), c.want)
		})
	}
}
