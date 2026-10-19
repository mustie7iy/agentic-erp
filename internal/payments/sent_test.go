package payments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
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

// transmitIn transmits a file in a request that is rolled back afterwards,
// as if its commit were lost.
func transmitIn(t *testing.T, e env, h *payments.Endpoints, file uuid.UUID) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.TransmitFile(ctx, file, yes)
		return err
	})
}

func sentRecords(t *testing.T, file uuid.UUID) int {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM payments.sent_bank_file WHERE bank_file_id = $1`, file).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDuplicateByHash: a file whose bank already took its content hash
// counts as sent (ADR-0038, T44c). The record of the upload is committed in
// a transaction of its own, so a transmission whose commit is lost keeps
// it, and the retry sends nothing and says it was a duplicate.
func TestDuplicateByHash(t *testing.T) {
	e := setup(t)
	h := endpoints(e.connector)
	h.Sent = payments.SentFiles{Pool: pgtest.Pool(t), AppRole: "erp_app"}
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	file := releasedFile(t, e, "INV-1")

	if err := transmitIn(t, e, h, file); err != nil {
		t.Fatalf("transmit with a lost commit: %v", err)
	}
	if got, _ := e.Must(200, "GET", "/v1/bank-files/"+file.String(), "", ""); got["status"] != "created" {
		t.Fatalf("file after the lost commit, status: got %v, want created", got["status"])
	}
	if got := sentRecords(t, file); got != 1 {
		t.Fatalf("sent records after the lost commit: got %d, want 1", got)
	}

	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "transmit a duplicate", func(ctx context.Context) error {
		_, err := h.TransmitFile(ctx, file, yes)
		return err
	})
	e.Must(202, "POST", "/v1/bank-files/"+file.String()+":transmit", "", `"1"`)
	if got := len(e.connector.files); got != 1 {
		t.Errorf("uploads after the retry: got %d, want 1", got)
	}
	if got, _ := e.Must(200, "GET", "/v1/bank-files/"+file.String(), "", ""); got["status"] != "transmitted" {
		t.Errorf("file after the retry, status: got %v, want transmitted", got["status"])
	}

	other := releasedFile(t, e, "INV-2")
	e.Must(202, "POST", "/v1/bank-files/"+other.String()+":transmit", "", `"1"`)
	if got := len(e.connector.files); got != 2 {
		t.Errorf("uploads after a new file: got %d, want 2", got)
	}
	ev := events(t, e.Tenant, "bank_file_transmitted")
	want := map[string]bool{file.String(): true, other.String(): false}
	if len(ev) != len(want) {
		t.Fatalf("transmitted events: got %d, want %d", len(ev), len(want))
	}
	for _, x := range ev {
		if got := x["duplicate"]; got != want[x["bank_file_id"].(string)] {
			t.Errorf("event of %v, duplicate: got %v, want %v", x["bank_file_id"], got, want[x["bank_file_id"].(string)])
		}
	}
}

// TestSentRecordFailures: when the record of an upload cannot be committed,
// the transmission fails and the file stays created, to be transmitted
// again.
func TestSentRecordFailures(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	file := releasedFile(t, e, "INV-1")
	pool := pgtest.Pool(t)
	wrap := func(f pgtest.FailingTx) database.BeginFunc {
		return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			f.Tx = tx
			return &f, err
		}
	}
	for name, sent := range map[string]payments.SentFiles{
		"no database":  {},
		"begin fails":  {Begin: func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }},
		"insert fails": {Begin: wrap(pgtest.FailingTx{FailAt: 1}), AppRole: "erp_app"},
		"commit fails": {Begin: wrap(pgtest.FailingTx{FailCommit: true}), AppRole: "erp_app"},
		// No connection to be had: the record gives up after its bound
		// instead of waiting for the request's.
		"no connection free": {Timeout: time.Millisecond, Begin: func(ctx context.Context, _ database.Scope) (pgx.Tx, error) {
			<-ctx.Done()
			return nil, ctx.Err() // what pgxpool returns when the context ends while it waits
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := endpoints(e.connector)
			h.Sent = sent
			if err := transmitIn(t, e, h, file); err == nil {
				t.Error("got nil error, want the record's failure")
			}
			if got := sentRecords(t, file); got != 0 {
				t.Errorf("sent records: got %d, want 0", got)
			}
			if got, _ := e.Must(200, "GET", "/v1/bank-files/"+file.String(), "", ""); got["status"] != "created" {
				t.Errorf("file status: got %v, want created", got["status"])
			}
		})
	}
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
