package ledger_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestJournalEntriesByAccount: a trial balance's account drilled down to
// its entries (T73j2): only the entries with a line on the account, in the
// period asked, each with all its lines, paged as any listing.
func TestJournalEntriesByAccount(t *testing.T) {
	e := setup(t)
	bank, revenue, expense := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "520000")
	march, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "10.00", ""}, line{revenue, "-10.00", ""}), "")
	april, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-04-15", "EUR", line{bank, "20.00", ""}, line{revenue, "-20.00", ""}), "")
	spent, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-04-20", "EUR", line{expense, "5.00", ""}, line{bank, "-5.00", ""}), "")
	for name, c := range map[string]struct {
		query string
		want  []any
	}{
		"the bank account":                {"account_id=" + bank, []any{march["id"], april["id"], spent["id"]}},
		"the bank account in period 4":    {"account_id=" + bank + "&fiscal_year=2026&fiscal_period=4", []any{april["id"], spent["id"]}},
		"the revenue account in period 3": {"account_id=" + revenue + "&fiscal_period=3", []any{march["id"]}},
		"the expense account":             {"account_id=" + expense, []any{spent["id"]}},
		"period 4":                        {"fiscal_period=4", []any{april["id"], spent["id"]}},
		"an account without lines":        {"account_id=" + e.account(t, "2000", "610000"), nil},
		"the account in another year":     {"account_id=" + bank + "&fiscal_year=2025", nil},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&%s", e.de, c.query), "", "")
			var got []any
			for _, it := range apitest.List(t, page, "items") {
				got = append(got, it.(map[string]any)["id"])
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("each entry with all its lines", func(t *testing.T) {
		page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&account_id=%s", e.de, expense), "", "")
		items := apitest.List(t, page, "items")
		if got := len(apitest.List(t, items[0].(map[string]any), "lines")); got != 2 {
			t.Errorf("lines: got %d, want 2", got)
		}
	})
	t.Run("paged", func(t *testing.T) {
		first, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&account_id=%s&page_size=2", e.de, bank), "", "")
		token, _ := first["next_page_token"].(string)
		if token == "" {
			t.Fatalf("first page: got no next_page_token, want one")
		}
		next, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&account_id=%s&page_size=2&page_token=%s", e.de, bank, token), "", "")
		if got := apitest.List(t, next, "items")[0].(map[string]any)["id"]; got != spent["id"] {
			t.Errorf("second page: got %v, want %v", got, spent["id"])
		}
	})
	t.Run("a token of another account", func(t *testing.T) {
		first, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&account_id=%s&page_size=1", e.de, bank), "", "")
		e.in(t).Problem(400, "request.invalid_page_token", "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&account_id=%s&page_size=1&page_token=%s",
			e.de, revenue, first["next_page_token"]), "", "")
	})
	t.Run("a period that is none", func(t *testing.T) {
		e.in(t).Problem(400, "request.invalid_parameter", "GET", "/v1/journal-entries?fiscal_period=17", "", "")
	})
}

// TestJournalEntriesByAccountBudget holds a trial balance account's
// entries (a first page of 100 of one company's year in its leading
// ledger, in a period and in the whole year) to 01-requirements' list
// budget (p95 150 ms), for an account on every entry and for one on 50,
// among 200,000 entries (400,000 lines). No requirement names an
// account's share of a company's entries, so the two ends are measured,
// which are the plans that matter: an account on many entries is found on
// the first entries by date (the page indexes, 000055 and 000186), one on
// a few by its lines (journal_line_account, 000005), its entries then
// sorted; the planner chooses between them by the account's statistics.
// The first reads a page, the second the account's lines, so neither
// grows with the year's other entries (01 names a million lines a
// company a year on average: 500 million for the largest tenant's 500
// companies).
func TestJournalEntriesByAccountBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	bank, revenue, rare := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "520000")
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-06-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	seedSpread(t, je["id"].(string), 199_950)
	for range 50 {
		e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-06-20", "EUR", line{rare, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	}
	pgtest.Exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line`)
	for name, c := range map[string]struct {
		account, period string
		rows            int
	}{
		"an account on every entry in a period": {bank, "&fiscal_period=6", 100},
		"an account on 50 entries in a period":  {rare, "&fiscal_period=6", 50},
		"an account on every entry in the year": {bank, "", 100},
		"an account on 50 entries in the year":  {rare, "", 50},
	} {
		t.Run(name, func(t *testing.T) {
			path := fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&fiscal_year=2026&account_id=%s&page_size=100%s", e.de, c.account, c.period)
			var times []time.Duration
			for range 20 {
				start := time.Now()
				page, _ := e.in(t).Must(200, "GET", path, "", "")
				times = append(times, time.Since(start))
				if got := len(apitest.List(t, page, "items")); got != c.rows {
					t.Fatalf("entries: got %d, want %d", got, c.rows)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("a first page: median %v, p95 %v", times[len(times)/2], p95)
			if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}

// seedSpread copies an entry and its lines n times with new ids and
// document numbers, spread over the year's twelve periods (each copy on
// the 15th of its month), in one transaction so that each copy balances
// at commit.
func seedSpread(t *testing.T, id string, n int) {
	t.Helper()
	pgtest.Exec(t, `WITH seed AS MATERIALIZED (SELECT g, gen_random_uuid() AS id, (g % 12 + 1)::smallint AS m FROM generate_series(1, $2::int) g),
		entries AS (INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
		 SELECT e.tenant_id, e.fiscal_year, s.id, e.legal_entity_id, e.ledger_id, s.m, e.document_type, 'SEED' || lpad(s.g::text, 7, '0'),
			make_date(2026, s.m, 15), make_date(2026, s.m, 15), e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc, e.rate_lc_gc, 'seed-' || s.g,
			e.source_context, e.posted_by
		 FROM ledger.journal_entry e, seed s WHERE e.id = $1)
		INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		SELECT l.tenant_id, l.fiscal_year, s.id, l.line_no, l.legal_entity_id, l.ledger_id, s.m, make_date(2026, s.m, 15), l.account_id, l.amount_tc,
			l.amount_lc, l.amount_gc
		FROM ledger.journal_line l, seed s WHERE l.journal_entry_id = $1`, id, n)
}
