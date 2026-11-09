package consolidation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// statementPath is a run's statement's path.
func statementPath(run uuid.UUID, statement string) string {
	return runsPath + "/" + run.String() + "/statements/" + statement
}

// statementRows reads a page of a statement as one line per row.
func statementRows(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		r := it.(map[string]any)
		role := ""
		if x, ok := r["role"].(string); ok {
			role = " (" + x + ")"
		}
		out = append(out, fmt.Sprintf("%s %s %s%s: %s", r["number"], r["name"], r["type"], role, columnsOf(r)))
	}
	return out
}

// statementRow is the statement's row of the group account, as
// statementRows shows it, or none.
func statementRow(page map[string]any, number string) string {
	for _, r := range statementRows(page) {
		if strings.HasPrefix(r, number+" ") {
			return r
		}
	}
	return "none"
}

// columnsOf shows a row's or a total's three columns.
func columnsOf(m any) string {
	x := m.(map[string]any)
	amount := func(k string) string {
		a := x[k].(map[string]any)
		return a["amount"].(string) + " " + a["currency"].(string)
	}
	return amount("reported") + " / " + amount("eliminations") + " / " + amount("consolidated")
}

// statementTotals shows a statement's totals, those named in order.
func statementTotals(page map[string]any, names ...string) string {
	tot := page["totals"].(map[string]any)
	var out []string
	for _, n := range names {
		out = append(out, n+" "+columnsOf(tot[n]))
	}
	if len(tot) != len(names) {
		out = append(out, fmt.Sprintf("and %d more", len(tot)-len(names)))
	}
	return strings.Join(out, ", ")
}

// TestConsolidatedStatements (ADR-0059 §11): TestConsolidationRun's run
// as its balance sheet and profit and loss, every group account of their
// types in number order with the companies' translated figures summed,
// the eliminations of 2000's investment in 1000 against 1000's share
// capital (T68h2: an incorporated member's results before November stay
// on the group's retained earnings, and its reserve stays whole), and the
// totals: the balance sheet's assets,
// liabilities, equity and the year's profit summing to zero, the profit
// and loss's revenue, expenses, profit and its attribution, all of it to
// the owners of the parent. Each row's reported figure is
// the balances list's total of its group account; the balance sheet pages
// by number; a final run reads alike; a caller who may not act in every
// company of the group finds no run (ADR-0059 §12).
func TestConsolidatedStatements(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	figures(t, e)
	run, _ := start(t, e)
	carryOut(t, e, app.Consolidation(app.Config{}), run)
	bs, _ := e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	if got := fmt.Sprint(bs["run_id"], " ", bs["group_code"], " ", bs["group_name"], " ", bs["fiscal_year"], "-", bs["period"], " ", bs["period_end"], " ",
		bs["run_status"], " ", bs["statement"], " ", bs["group_currency"]); got != run.String()+" ACME Acme group 2026-11 2026-11-30 computed balance_sheet EUR" {
		t.Errorf("the balance sheet's run: got %s, want ACME's computed November in EUR", got)
	}
	want := []string{
		"G1000 Other assets asset: 1140.00 EUR / 0.00 EUR / 1140.00 EUR",
		"G1250 Intercompany receivables asset: 0.00 EUR / 0.00 EUR / 0.00 EUR",
		"G1500 Investments in subsidiaries asset: 400.00 EUR / -400.00 EUR / 0.00 EUR",
		"G1900 Goodwill asset (goodwill): 0.00 EUR / 0.00 EUR / 0.00 EUR",
		"G2000 Liabilities liability: 0.00 EUR / 0.00 EUR / 0.00 EUR",
		"G2250 Intercompany payables liability: 0.00 EUR / 0.00 EUR / 0.00 EUR",
		"G3000 Share capital equity: -400.00 EUR / 400.00 EUR / 0.00 EUR",
		"G3800 Retained earnings equity (retained_earnings): -630.00 EUR / 0.00 EUR / -630.00 EUR",
		"G3900 Currency translation reserve equity (cta): -155.00 EUR / 0.00 EUR / -155.00 EUR",
		"G3950 Non-controlling interests equity (nci): 0.00 EUR / 0.00 EUR / 0.00 EUR",
	}
	if got, w := strings.Join(statementRows(bs), "\n"), strings.Join(want, "\n"); got != w {
		t.Errorf("the balance sheet: got\n%s\nwant\n%s", got, w)
	}
	if got, w := statementTotals(bs, "assets", "liabilities", "equity", "profit"), "assets 1540.00 EUR / -400.00 EUR / 1140.00 EUR, "+
		"liabilities 0.00 EUR / 0.00 EUR / 0.00 EUR, equity -1185.00 EUR / 400.00 EUR / -785.00 EUR, profit -355.00 EUR / 0.00 EUR / -355.00 EUR"; got != w {
		t.Errorf("the balance sheet's totals: got %s, want %s (summing to zero)", got, w)
	}
	pl, _ := e.Must(200, "GET", statementPath(run, "profit_and_loss"), "", "")
	want = []string{
		"G4000 Revenue revenue: -355.00 EUR / 0.00 EUR / -355.00 EUR",
		"G5000 Expenses expense: 0.00 EUR / 0.00 EUR / 0.00 EUR",
		"G6950 Intercompany differences expense: 0.00 EUR / 0.00 EUR / 0.00 EUR",
	}
	if got, w := strings.Join(statementRows(pl), "\n"), strings.Join(want, "\n"); got != w {
		t.Errorf("the profit and loss: got\n%s\nwant\n%s", got, w)
	}
	if got, w := statementTotals(pl, "revenue", "expenses", "profit", "non_controlling_interests", "owners_of_the_parent"), "revenue -355.00 EUR / "+
		"0.00 EUR / -355.00 EUR, expenses 0.00 EUR / 0.00 EUR / 0.00 EUR, profit -355.00 EUR / 0.00 EUR / -355.00 EUR, non_controlling_interests "+
		"0.00 EUR / 0.00 EUR / 0.00 EUR, owners_of_the_parent -355.00 EUR / 0.00 EUR / -355.00 EUR"; got != w {
		t.Errorf("the profit and loss's totals: got %s, want %s", got, w)
	}
	for _, page := range []map[string]any{bs, pl} {
		for _, it := range page["items"].([]any) {
			r := it.(map[string]any)
			b, _ := e.Must(200, "GET", runsPath+"/"+run.String()+"/balances?group_account="+r["number"].(string), "", "")
			if got, w := b["totals"].(map[string]any)["group"].(map[string]any)["amount"], r["reported"].(map[string]any)["amount"]; got != w {
				t.Errorf("%s: the balances' total %v, want the reported %v", r["number"], got, w)
			}
		}
	}
	statementPages(t, e, run)
	reader := e.As(e.Reader(t, "Reader of 2000", e.DE))
	reader.Problem(404, "request.not_found", "GET", statementPath(run, "balance_sheet"), "", "")
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'final', finalized_by = $2, finalized_at = now(), version = version + 1 WHERE id = $1`, run,
		e.Tenant.Human.ID)
	final, _ := e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	if got := fmt.Sprint(final["run_status"], " ", strings.Join(statementRows(final), "|") == strings.Join(statementRows(bs), "|")); got != "final true" {
		t.Errorf("the final run's balance sheet: got %s, want final with the computed run's rows", got)
	}
}

// statementPages: the balance sheet by pages of four, by number, each
// page with the totals of the whole statement.
func statementPages(t *testing.T, e fixture.Env, run uuid.UUID) {
	t.Helper()
	path := statementPath(run, "balance_sheet") + "?page_size=4"
	var seen []string
	next := path
	for next != "" {
		page, _ := e.Must(200, "GET", next, "", "")
		for _, it := range page["items"].([]any) {
			seen = append(seen, it.(map[string]any)["number"].(string))
		}
		if got := columnsOf(page["totals"].(map[string]any)["assets"]); got != "1540.00 EUR / -400.00 EUR / 1140.00 EUR" {
			t.Errorf("a page's assets: got %s, want the whole statement's", got)
		}
		next = ""
		if tok, ok := page["next_page_token"].(string); ok {
			next = path + "&page_token=" + tok
		}
	}
	if got, w := strings.Join(seen, " "), "G1000 G1250 G1500 G1900 G2000 G2250 G3000 G3800 G3900 G3950"; got != w {
		t.Errorf("the pages: got %s, want %s", got, w)
	}
}

// TestConsolidatedStatementRefusals: a statement of a run not computed
// (queued, discarded) is refused with consolidation.run_not_computed, one
// that is neither statement is not found, and a page token of another
// statement and a page size out of range are refused.
func TestConsolidatedStatementRefusals(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	run, _ := start(t, e)
	e.Problem(409, "consolidation.run_not_computed", "GET", statementPath(run, "balance_sheet"), "", "")
	e.Problem(404, "request.not_found", "GET", statementPath(run, "cash_flow"), "", "")
	h := app.Consolidation(app.Config{})
	carryOut(t, e, h, run)
	pl, _ := e.Must(200, "GET", statementPath(run, "profit_and_loss")+"?page_size=1", "", "")
	e.Problem(400, "request.invalid_page_token", "GET", statementPath(run, "balance_sheet")+"?page_token="+pl["next_page_token"].(string), "", "")
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetConsolidatedStatement(ctx, gen.GetConsolidatedStatementParams{ID: run, Statement: "balance_sheet", PageSize: gen.NewOptInt32(0)})
		return err
	}); problem.From(err).Code != "request.invalid_page_size" {
		t.Errorf("page size 0: got %v, want request.invalid_page_size", err)
	}
	got, tag := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	e.Must(200, "POST", runsPath+"/"+run.String()+":discard", "", tag)
	if got["status"] != "computed" {
		t.Fatalf("the run before its discard: got %v, want computed", got["status"])
	}
	e.Problem(409, "consolidation.run_not_computed", "GET", statementPath(run, "profit_and_loss"), "", "")
}
