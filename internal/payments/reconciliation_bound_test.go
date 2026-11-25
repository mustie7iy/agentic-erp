package payments_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// seedReconciled copies the reconciled statement n times, each with its
// lines posted, as statements the bank reconciled before it: history that
// adds nothing to the figures (no line excluded, none posted late).
func seedReconciled(t *testing.T, statement string, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after the commit; on failure the test has already failed
	for _, sql := range []string{
		`CREATE TEMP TABLE seed ON COMMIT DROP AS SELECT g, gen_random_uuid() AS id FROM generate_series(1, $2::int) g WHERE $1::uuid IS NOT NULL`,
		`INSERT INTO payments.bank_statement SELECT (jsonb_populate_record(s, jsonb_build_object('id', x.id, 'statement_key', 'seed-' || x.g,
			'file_position', 1000 + x.g, 'opening_date', date '2026-01-01' + x.g, 'closing_date', date '2026-01-01' + x.g,
			'previous_statement_id', NULL, 'reconciled_excluded', 0, 'reconciled_posted_until', NULL))).*
		 FROM payments.bank_statement s, seed x WHERE s.id = $1 AND $2::int > 0`,
		`INSERT INTO payments.bank_statement_line SELECT (jsonb_populate_record(l, jsonb_build_object('statement_id', x.id, 'status', 'posted'))).*
		 FROM payments.bank_statement_line l, seed x WHERE l.statement_id = $1 AND $2::int > 0`,
	} {
		if _, err := tx.Exec(ctx, sql, statement, n); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	pgtest.Exec(t, `ANALYZE payments.bank_statement`)
	pgtest.Exec(t, `ANALYZE payments.bank_statement_line`)
}

// TestReconcileReadsUnreconciledLines (T49): a reconciliation check reads
// the lines of the statements not reconciled, not the bank's history. R1
// reconciles keeping what it adds (its excluded line's -5.00, the date its
// lines were posted); 300 statements reconciled before it hold 900 lines;
// excluding R2's one line reconciles R2 on R1's kept figures (excluded
// -65.00 in all) while reading a handful of lines.
func TestReconcileReadsUnreconciledLines(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	sc.openingEntry(t, 10000)
	_, tag := e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711."}`, sc.tag(t))
	_, tag = e.Must(201, "POST", sc.line(3, "exclude"), `{"reason": "The bank's error."}`, tag)
	e.Must(200, "POST", sc.line(2, "post"), "", tag)
	var kept string
	pgtest.Scan(t, `SELECT status || ' ' || reconciled_excluded || ' ' || reconciled_posted_until FROM payments.bank_statement WHERE id = $1`,
		[]any{sc.statement}, &kept)
	if want := "reconciled -500 2026-04-17"; kept != want {
		t.Fatalf("R1 kept: got %s, want %s", kept, want)
	}
	seedReconciled(t, sc.statement, 300)
	file := mtStatement("R2", "2", "124,50", "260418", "64,50", "260418", ":61:2604180418D60,00NTRFNONREF//R6\n")
	r2 := id(e.imported(t, "mt940", file)[0].(map[string]any))
	h := endpoints(nil)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx) // Committed always puts its transaction in the context
		before := pgtest.RowsRead(t, ctx, tx, "payments", "bank\\_statement\\_line")
		if _, err := h.ExcludeLine(ctx, uuidOf(r2), 1, func(int32) error { return nil }, "Charged twice."); err != nil {
			return err
		}
		if got := pgtest.RowsRead(t, ctx, tx, "payments", "bank\\_statement\\_line") - before; got > 10 {
			t.Errorf("statement lines read by the exclusion and its check: got %d, want at most 10 with 900 reconciled before", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.Must(200, "GET", "/v1/bank-statements/"+r2, "", ""); s["status"] != "reconciled" {
		t.Errorf("R2 with its line excluded: got %v, want reconciled", s["status"])
	}
	evs := events(t, e.Tenant, "bank_statement_reconciled")
	if got := amountOf(evs[len(evs)-1]["excluded"]); got != "-65.00" {
		t.Errorf("R2's excluded: got %v, want -65.00 (R1's -5.00 and its own -60.00)", got)
	}
}
