package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestAssetReconciliationAccounts (T57a, ADR-0057 §3, migration 000189): a
// line on an asset reconciliation account posts only in an entry of the
// assets context or of a migration run (integration); the ledger's own
// entries and every other context's are refused, naming the constraint the
// API maps to ledger.asset_reconciliation_account.
func TestAssetReconciliationAccounts(t *testing.T) {
	need(t)
	f := newFixture(t)
	cost := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
			VALUES ($1,$2,$3,'160000','Machinery','asset','asset','active',$4,$4)`, f.tenant, cost, f.chart, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
			f.tenant, f.entity, cost, f.actor)
	})
	for _, tc := range []struct {
		source string
		ok     bool
	}{{"assets", true}, {"integration", true}, {"ledger", false}, {"payables", false}, {"payments", false}, {"procurement", false}, {"receivables", false}} {
		t.Run(tc.source, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return f.sourced(tx, tc.source, line{account: cost, amount: 1000}, line{account: f.bank, amount: -1000})
			})
			if tc.ok {
				if err != nil {
					t.Fatalf("a line on the asset reconciliation account from %s: got %v, want it posted", tc.source, err)
				}
				return
			}
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.ConstraintName != "journal_line_asset_reconciliation" {
				t.Fatalf("a line on the asset reconciliation account from %s: got %v, want journal_line_asset_reconciliation", tc.source, err)
			}
		})
	}
	err := asApp(t, f.tenant, func(tx pgx.Tx) error {
		return f.sourced(tx, "ledger", line{account: f.expense, amount: 1000}, line{account: f.bank, amount: -1000})
	})
	if err != nil {
		t.Errorf("a manual entry on accounts that reconcile nothing: got %v, want it posted", err)
	}
}

// sourced posts a balanced entry in May 2026 from the source context.
func (f fixture) sourced(tx pgx.Tx, source string, lines ...line) error {
	je := id()
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1,2026,$2,$3,$4,5,'AA',$5,'2026-05-10','2026-05-10','USD','USD','EUR',1,0.9,$6,$7)`,
		f.tenant, je, f.entity, f.ledger, je.String()[24:], source, f.actor); err != nil {
		return err
	}
	for i, l := range lines {
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc) VALUES ($1,2026,$2,$3,$4,$5,5,'2026-05-10',$6,$7,$7,$8)`,
			f.tenant, je, i+1, f.entity, f.ledger, l.account, l.amount, l.amount*9/10); err != nil {
			return err
		}
	}
	return nil
}
