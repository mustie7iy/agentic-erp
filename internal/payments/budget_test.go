package payments_test

import (
	"context"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestProposalBudget checks 01-requirements' budget for a payment proposal
// over 100,000 open items (asynchronous, under 60 s): 2,000 suppliers with
// 50 due invoices each, every supplier paid.
func TestProposalBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setup(t)
	ctx := context.Background()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	ten, by, ap := e.Tenant.ID, e.Tenant.Human.ID, e.Account(t, "2000", "211000")
	exec(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'BP' || n, 'organization', 'Supplier ' || n, 'DE', 'active', $2, $2 FROM generate_series(10000001, 10002000) n`, ten, by)
	exec(`INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by)
		SELECT tenant_id, id, 'supplier', $2 FROM partner.business_partner WHERE tenant_id = $1 AND number BETWEEN 'BP10000001' AND 'BP10002000'`, ten, by)
	exec(`INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		SELECT tenant_id, id, $2, $3, 'NT30', $4, $4 FROM partner.business_partner WHERE tenant_id = $1 AND number BETWEEN 'BP10000001' AND 'BP10002000'`, ten, e.DE, ap, by)
	exec(`WITH c AS (
			INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at, created_by, updated_by)
			SELECT tenant_id, gen_random_uuid(), 'PC' || number, 'create', id, '{}', 'load', 'applied', $2, now(), $2, $2
			FROM partner.business_partner WHERE tenant_id = $1 AND number BETWEEN 'BP10000001' AND 'BP10002000' RETURNING tenant_id, id, partner_id
		) INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
			bank_country_code, holder_name, valid_from, partner_change_id, created_by)
		SELECT tenant_id, gen_random_uuid(), partner_id, '\x01', md5(partner_id::text) || md5(id::text), '0000', 'iban', 'COBADEFFXXX', 'DE',
			'Holder', '2026-01-01', id, $2 FROM c`, ten, by)
	// The invoices are written straight into the journal, balanced, without the row checks a posting runs.
	exec(`SET session_replication_role = replica`)
	exec(`WITH s AS (
			SELECT id AS partner_id, row_number() OVER (ORDER BY number) AS n, gen_random_uuid() AS entry
			FROM partner.business_partner WHERE tenant_id = $1 AND number BETWEEN 'BP10000001' AND 'BP10002000'
		), e AS (
			INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
			SELECT $1, 2026, entry, $2, l.id, 3, 'KR', 'B' || n, '2026-03-01', '2026-03-01', 'EUR', 'EUR', 'EUR', 1, 1, 'INV-' || n, 'payables', $3
			FROM s, ledger.ledger l WHERE l.tenant_id = $1 RETURNING id, ledger_id
		) INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc, partner_id)
		SELECT $1, 2026, s.entry, i, $2, e.ledger_id, 3, '2026-03-01', CASE WHEN i = 1 THEN $4::uuid ELSE $5::uuid END,
			CASE WHEN i = 1 THEN 5000 ELSE -100 END, CASE WHEN i = 1 THEN 5000 ELSE -100 END, CASE WHEN i = 1 THEN 5000 ELSE -100 END,
			CASE WHEN i = 1 THEN NULL ELSE s.partner_id END
		FROM s JOIN e ON e.id = s.entry, generate_series(1, 51) i`, ten, e.DE, by, e.expense, ap)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, partner.business_partner, partner.supplier_company, partner.bank_account`)

	op := e.propose(t, "2026-04-15", "2026-04-15")
	start := time.Now()
	if n := work(t); n != 1 {
		t.Fatalf("worker did %d jobs", n)
	}
	elapsed := time.Since(start)
	var payments, items int
	var total int64
	if err := admin.QueryRow(ctx, `SELECT count(DISTINCT p.id), count(*), (SELECT total_amount FROM payments.payment_run WHERE id = $1)
		FROM payments.payment p JOIN payments.payment_item i ON i.tenant_id = p.tenant_id AND i.payment_id = p.id WHERE p.payment_run_id = $1`,
		uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):])).Scan(&payments, &items, &total); err != nil {
		t.Fatal(err)
	}
	if payments != 2000 || items != 100000 || total != 2000*5000 {
		t.Fatalf("proposal: %d payments, %d items, total %d", payments, items, total)
	}
	t.Logf("proposal of %d items: %v", items, elapsed)
	if elapsed > 60*time.Second {
		t.Errorf("proposal took %v; the budget is 60 s", elapsed)
	}
}
