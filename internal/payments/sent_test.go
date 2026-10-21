package payments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// releasedFile releases a run paying one invoice and returns its bank file.
func releasedFile(t *testing.T, e env, invoice string) uuid.UUID {
	t.Helper()
	e.invoice(t, e.Supplier, invoice, "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	return uuidOf(id(files["items"].([]any)[0].(map[string]any)))
}

// TestSentBankFileSchema: the schema keeps one record per house bank and
// content hash, takes only a SHA-256 and never changes a record.
func TestSentBankFileSchema(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	file := releasedFile(t, e, "INV-1")
	ctx := context.Background()
	insert := `INSERT INTO payments.sent_bank_file (tenant_id, house_bank_id, sha256, bank_file_id, sent_by) VALUES ($1, $2, $3, $4, $5)`
	hash := make([]byte, 32)
	pgtest.Exec(t, insert, e.Tenant.ID, e.bank, hash, file, e.Tenant.Human.ID)
	for name, c := range map[string]struct {
		sql  string
		args []any
		code string
	}{
		"same bank and hash": {insert, []any{e.Tenant.ID, e.bank, hash, file, e.Tenant.Human.ID}, "23505"},
		"not a SHA-256":      {insert, []any{e.Tenant.ID, e.bank, []byte{1}, file, e.Tenant.Human.ID}, "23514"},
		"changed":            {`UPDATE payments.sent_bank_file SET sent_by = sent_by WHERE bank_file_id = $1`, []any{file}, "42501"},
		"deleted":            {`DELETE FROM payments.sent_bank_file WHERE bank_file_id = $1`, []any{file}, "42501"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pgtest.Admin(t).Exec(ctx, c.sql, c.args...)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != c.code {
				t.Errorf("got %v, want SQLSTATE %s", err, c.code)
			}
		})
	}
}
