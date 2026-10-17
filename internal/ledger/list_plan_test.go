package ledger_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestListReadsOnlyItsPage: a page of journal entries reads the entries on
// it, not every entry of the legal entity. The nightly soak's latency grew
// with the journal because the list read all of it: pgx's cached statement
// switched to a generic plan after five runs, which cannot use the filters,
// and no index matched the sort. So the list runs six times before it is
// measured, on 2,000 entries that share one posting date (the worst case
// for a sort by posting date). pgtest.RowsRead counts what the query
// fetched from the journal's entry partitions.
func TestListReadsOnlyItsPage(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-06-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	seedCopies(t, je["id"].(string), 1999)
	const size = 50
	for _, order := range []string{"posting_date", "posting_date desc", "document_number", "document_number desc"} {
		t.Run(order, func(t *testing.T) {
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				params := gen.ListJournalEntriesParams{LegalEntityID: gen.NewOptUUID(e.de), FiscalYear: gen.NewOptInt32(2026),
					PageSize: gen.NewOptInt32(size), OrderBy: gen.NewOptString(order)}
				h := endpoints()
				for range 6 {
					if _, err := h.ListJournalEntries(ctx, params); err != nil {
						return err
					}
				}
				tx, _ := reqctx.TxFrom(ctx) // InRequest always puts its transaction in the context
				before := pgtest.RowsRead(t, ctx, tx, "ledger", `journal\_entry\_y%`)
				if _, err := h.ListJournalEntries(ctx, params); err != nil {
					return err
				}
				got := pgtest.RowsRead(t, ctx, tx, "ledger", `journal\_entry\_y%`) - before
				if got > size+1 {
					t.Errorf("entries read for a page of %d: got %d, want at most %d", size, got, size+1)
				}
				if got < size {
					t.Errorf("entries read for a page of %d: got %d, want at least %d (the statistics count the page)", size, got, size)
				}
				return nil
			}); err != nil {
				t.Fatalf("list: %v", err)
			}
		})
	}
}

// seedCopies copies an entry and its lines n times with new ids and
// document numbers, in one transaction so that each copy balances at commit.
func seedCopies(t *testing.T, id string, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after the commit; on failure the test has already failed
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`CREATE TEMP TABLE seed ON COMMIT DROP AS SELECT g, gen_random_uuid() AS id FROM generate_series(1, $1::int) g`, []any{n}},
		{`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
		 SELECT e.tenant_id, e.fiscal_year, s.id, e.legal_entity_id, e.ledger_id, e.fiscal_period, e.document_type, 'SEED' || lpad(s.g::text, 6, '0'),
			e.document_date, e.posting_date, e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc, e.rate_lc_gc, 'seed-' || s.g, e.source_context, e.posted_by
		 FROM ledger.journal_entry e, seed s WHERE e.id = $1`, []any{id}},
		{`INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		 SELECT l.tenant_id, l.fiscal_year, s.id, l.line_no, l.legal_entity_id, l.ledger_id, l.fiscal_period, l.posting_date, l.account_id, l.amount_tc, l.amount_lc, l.amount_gc
		 FROM ledger.journal_line l, seed s WHERE l.journal_entry_id = $1`, []any{id}},
	} {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	pgtest.Exec(t, `ANALYZE ledger.journal_entry`)
}
