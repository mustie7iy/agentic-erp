package ledger_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// reconEnv is setupParallel with, in company 2000, a posting to both
// ledgers and two to 2L alone on the bank and revenue accounts.
type reconEnv struct {
	parallelEnv
	bank, revenue string
}

func setupRecon(t *testing.T) reconEnv {
	t.Helper()
	r := reconEnv{parallelEnv: setupParallel(t)}
	r.bank, r.revenue = r.account(t, "2000", "100000"), r.account(t, "2000", "400000")
	r.in(t).Must(201, "POST", "/v1/journal-entries", entry(r.de, "2026-03-15", "EUR", line{r.bank, "10.00", ""}, line{r.revenue, "-10.00", ""}), "")
	for _, date := range []string{"2026-03-16", "2026-03-17"} {
		r.in(t).Must(201, "POST", "/v1/journal-entries", with(entry(r.de, date, "EUR", line{r.bank, "25.00", ""}, line{r.revenue, "-25.00", ""}),
			`"ledger_id": "`+r.local+`"`), "")
	}
	return r
}

// query is the reconciliation's query string of company 2000 and 2026
// against 2L, with more parameters.
func (r reconEnv) query(more url.Values) string {
	q := url.Values{"legal_entity_id": {r.de.String()}, "compare_ledger_id": {r.local}, "fiscal_year": {"2026"}}
	for k, v := range more {
		q[k] = v
	}
	return q.Encode()
}

// TestLedgerReconciliation: the differences between two ledgers per
// account and currency, each ledger's trial balance, and the
// ledger-specific lines behind a difference (T52e, ADR-0045).
func TestLedgerReconciliation(t *testing.T) {
	r := setupRecon(t)
	t.Run("differences", func(t *testing.T) { differences(t, r) })
	t.Run("no differences", func(t *testing.T) { noDifferences(t, r) })
	t.Run("entries", func(t *testing.T) { reconEntries(t, r) })
	t.Run("unbalanced ledger", func(t *testing.T) {
		pgtest.Unguarded(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
				debit_tc, debit_lc, debit_gc) VALUES ($1, $2, $3, $4, 2026, 5, 'EUR', 100, 100, 100)`, r.ten.ID, uuid.MustParse(r.local), r.de, uuid.MustParse(r.bank))
		page, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(url.Values{"fiscal_period": {"5"}}), "", "")
		tbs := page["trial_balances"].([]any)
		if got := tbs[0].(map[string]any)["balanced"]; got != true {
			t.Errorf("0L balanced: got %v, want true", got)
		}
		if got := tbs[1].(map[string]any)["balanced"]; got != false {
			t.Errorf("2L balanced: got %v, want false", got)
		}
		if got := len(page["items"].([]any)); got != 1 {
			t.Errorf("differences: got %d, want the bank account alone", got)
		}
	})
}

func differences(t *testing.T, r reconEnv) {
	t.Helper()
	page, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(nil), "", "")
	if page["ledger_id"] != r.leading || page["compare_ledger_id"] != r.local {
		t.Errorf("ledgers: got %v with %v, want 0L %s with 2L %s", page["ledger_id"], page["compare_ledger_id"], r.leading, r.local)
	}
	want := map[string][3]string{r.bank: {"10.00", "60.00", "-50.00"}, r.revenue: {"-10.00", "-60.00", "50.00"}}
	names := map[string]string{r.bank: "100000 Bank - Operating", r.revenue: "400000 Sales Revenue"}
	items := page["items"].([]any)
	if len(items) != len(want) {
		t.Fatalf("differences: got %d, want %d", len(items), len(want))
	}
	for _, it := range items {
		d := it.(map[string]any)
		w := want[d["account_id"].(string)]
		got := [3]string{amount(d["balance"]), amount(d["compare_balance"]), amount(d["difference"])}
		if got != w {
			t.Errorf("account %v: got balance, compared, difference %v, want %v", d["account_id"], got, w)
		}
		if lc := amount(d["difference_lc"]); lc != w[2] {
			t.Errorf("account %v: difference lc got %s, want %s", d["account_id"], lc, w[2])
		}
		if got, want := fmt.Sprint(d["account_number"], " ", d["account_name"]), names[d["account_id"].(string)]; got != want {
			t.Errorf("account %v: named %s, want %s", d["account_id"], got, want)
		}
	}
	for i, tb := range page["trial_balances"].([]any) {
		b := tb.(map[string]any)
		if wantID := []string{r.leading, r.local}[i]; b["ledger_id"] != wantID || b["balanced"] != true {
			t.Errorf("trial balance %d: got %v balanced %v, want %s balanced", i, b["ledger_id"], b["balanced"], wantID)
		}
	}
	first, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(url.Values{"page_size": {"1"}, "fiscal_period": {"3"}}), "", "")
	next, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(url.Values{"page_size": {"1"}, "fiscal_period": {"3"}, "page_token": {first["next_page_token"].(string)}}), "", "")
	if a, b := first["items"].([]any), next["items"].([]any); len(a) != 1 || len(b) != 1 || next["next_page_token"] != nil {
		t.Errorf("pages: got %d then %d items (next %v), want one each and no third", len(a), len(b), next["next_page_token"])
	}
	if got := len(next["trial_balances"].([]any)); got != 0 {
		t.Errorf("trial balances on the second page: got %d, want none", got)
	}
	one, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(url.Values{"account_id": {r.bank}, "ledger_id": {r.local}, "compare_ledger_id": {r.leading}}), "", "")
	if items := one["items"].([]any); len(items) != 1 || amount(items[0].(map[string]any)["difference"]) != "50.00" {
		t.Errorf("2L against 0L on the bank: got %v, want one difference of 50.00", items)
	}
}

func amount(m any) string { return m.(map[string]any)["amount"].(string) }

func noDifferences(t *testing.T, r reconEnv) {
	t.Helper()
	for _, c := range []struct {
		name   string
		q      url.Values
		trials int
	}{
		{"another period", url.Values{"fiscal_period": {"4"}}, 2},
		{"a ledger with itself", url.Values{"compare_ledger_id": {r.leading}}, 2},
		{"a ledger the company does not keep", url.Values{"legal_entity_id": {r.us.String()}}, 0},
		{"an unknown ledger", url.Values{"ledger_id": {uuid.NewString()}}, 0},
		{"an unknown legal entity", url.Values{"legal_entity_id": {uuid.NewString()}}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(c.q), "", "")
			if got := len(page["items"].([]any)); got != 0 {
				t.Errorf("differences: got %d, want none", got)
			}
			if got := len(page["trial_balances"].([]any)); got != c.trials {
				t.Errorf("trial balances: got %d, want %d", got, c.trials)
			}
			entries, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(c.q)+"&account_id="+r.bank, "", "")
			if got := len(entries["items"].([]any)); got != 0 {
				t.Errorf("entries: got %d, want none", got)
			}
		})
	}
}

func reconEntries(t *testing.T, r reconEnv) {
	t.Helper()
	page, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(url.Values{"account_id": {r.bank}, "currency": {"EUR"}}), "", "")
	items := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("entries: got %d, want the two 2L postings", len(items))
	}
	for i, it := range items {
		l := it.(map[string]any)
		if l["ledger_id"] != r.local || amount(l["amount"]) != "25.00" || amount(l["amount_lc"]) != "25.00" {
			t.Errorf("entry %d: got ledger %v amount %v, want 2L 25.00", i, l["ledger_id"], l["amount"])
		}
		if want := []string{"2026-03-16", "2026-03-17"}[i]; l["posting_date"] != want {
			t.Errorf("entry %d posting date: got %v, want %s", i, l["posting_date"], want)
		}
	}
	first, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(url.Values{"account_id": {r.bank}, "page_size": {"1"}, "fiscal_period": {"3"}, "currency": {"EUR"}}), "", "")
	next, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(url.Values{"account_id": {r.bank}, "page_size": {"1"}, "fiscal_period": {"3"}, "currency": {"EUR"},
		"page_token": {first["next_page_token"].(string)}}), "", "")
	if got := next["items"].([]any)[0].(map[string]any)["posting_date"]; got != "2026-03-17" || next["next_page_token"] != nil {
		t.Errorf("second page: got %v (next %v), want 2026-03-17 and the last page", got, next["next_page_token"])
	}
	usd, _ := r.in(t).Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(url.Values{"account_id": {r.bank}, "currency": {"USD"}}), "", "")
	if got := len(usd["items"].([]any)); got != 0 {
		t.Errorf("USD entries: got %d, want none", got)
	}
}

// TestLedgerReconciliationDatabaseFailures calls the reconciliation reads
// outside a unit of work and with each database statement failing in turn.
func TestLedgerReconciliationDatabaseFailures(t *testing.T) {
	r := setupRecon(t)
	h := endpoints()
	compare, bank := uuid.MustParse(r.local), uuid.MustParse(r.bank)
	for name, call := range map[string]func(ctx context.Context) error{
		"reconcile": func(ctx context.Context) error {
			_, err := h.ListLedgerReconciliation(ctx, gen.ListLedgerReconciliationParams{LegalEntityID: r.de, CompareLedgerID: compare, FiscalYear: 2026})
			return err
		},
		"reconciliation entries": func(ctx context.Context) error {
			_, err := h.ListLedgerReconciliationEntries(ctx, gen.ListLedgerReconciliationEntriesParams{LegalEntityID: r.de, CompareLedgerID: compare,
				FiscalYear: 2026, AccountID: bank})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, r.ten, r.ten.Human, name, call)
		})
	}
	t.Run("page size", func(t *testing.T) {
		// Handlers check page sizes for callers other than the generated router.
		zero := gen.NewOptInt32(0)
		for name, call := range map[string]func(ctx context.Context) error{
			"reconcile": func(ctx context.Context) error {
				_, err := h.ListLedgerReconciliation(ctx, gen.ListLedgerReconciliationParams{LegalEntityID: r.de, CompareLedgerID: compare, FiscalYear: 2026,
					PageSize: zero})
				return err
			},
			"reconciliation entries": func(ctx context.Context) error {
				_, err := h.ListLedgerReconciliationEntries(ctx, gen.ListLedgerReconciliationEntriesParams{LegalEntityID: r.de, CompareLedgerID: compare,
					FiscalYear: 2026, AccountID: bank, PageSize: zero})
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				if err := apitest.InRequest(t, r.ten, r.ten.Human, call); err == nil {
					t.Error("page size 0: got no error, want one")
				}
			})
		}
	})
}
