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
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// icAccounts are the intercompany accounts withEliminations gives both
// companies, each on a group account of its own.
var icAccounts = []config.Account{
	{Number: "125000", Name: "Intercompany receivables", Type: config.Asset, GroupAccount: "G1250"},
	{Number: "126000", Name: "Intercompany loans receivable", Type: config.Asset, GroupAccount: "G1260"},
	{Number: "225000", Name: "Intercompany payables", Type: config.Liability, GroupAccount: "G2250"},
	{Number: "226000", Name: "Intercompany loans payable", Type: config.Liability, GroupAccount: "G2260"},
	{Number: "425000", Name: "Intercompany revenue", Type: config.Revenue, GroupAccount: "G4250"},
	{Number: "525000", Name: "Intercompany expenses", Type: config.Expense, GroupAccount: "G5250"},
}

// withEliminations is withRuns' group with three eliminations: the
// fixture's IC_BALANCES (receivables and payables, tolerance 10.00),
// IC_LOANS (loans, 0.00) and IC_RESULTS (revenue and expenses, 1.00),
// each difference to G6950, and icAccounts in both companies.
func withEliminations(doc *config.Config) {
	withRuns(doc)
	cs := &doc.Consolidation
	cs.Accounts = append(cs.Accounts, config.GroupAccount{Number: "G1260", Name: "Intercompany loans", Type: config.Asset},
		config.GroupAccount{Number: "G2260", Name: "Intercompany borrowings", Type: config.Liability},
		config.GroupAccount{Number: "G4250", Name: "Intercompany revenue", Type: config.Revenue},
		config.GroupAccount{Number: "G5250", Name: "Intercompany expenses", Type: config.Expense})
	cs.Eliminations = append(cs.Eliminations,
		config.Elimination{Code: "IC_RESULTS", Name: "Intercompany revenue and expenses", Accounts: []string{"G4250", "G5250"}, DifferenceAccount: "G6950",
			Tolerance: "1.00"},
		config.Elimination{Code: "IC_LOANS", Name: "Intercompany loans", Accounts: []string{"G1260", "G2260"}, DifferenceAccount: "G6950", Tolerance: "0"})
	for _, code := range []string{"1000", "2000"} {
		co := doc.Company(code)
		co.ChartOfAccounts = append(co.ChartOfAccounts, icAccounts...)
	}
}

// icLine is a line of postIC: an account, an amount and a trading
// partner, if any.
type icLine struct {
	account, amount string
	partner         *uuid.UUID
}

// postIC posts a manual entry of the company on the day in its currency.
func postIC(t *testing.T, e fixture.Env, company uuid.UUID, code, currency, day string, lines ...icLine) {
	t.Helper()
	var body []string
	for _, l := range lines {
		tp := ""
		if l.partner != nil {
			tp = fmt.Sprintf(`, "trading_partner_id": %q`, *l.partner)
		}
		body = append(body, fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": %q}%s}`, e.Account(t, code, l.account), l.amount, currency, tp))
	}
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q, "lines": [%s]}`,
		company, day, day, currency, strings.Join(body, ", ")), "")
}

// icEnv is runEnv with withEliminations' configuration, both companies on
// one calendar, a company outside the group (9000, Outside Holdings) and
// icFigures posted; it returns the outside company.
func icEnv(t *testing.T) (fixture.Env, uuid.UUID) {
	t.Helper()
	e := fixture.NewWith(t, nil, withEliminations, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	outside := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '9000', 'Outside Holdings', 'US', 'USD', gen_random_uuid(), 'active', $3, $3)`, e.Tenant.ID, outside, e.Tenant.Human.ID)
	icFigures(t, e, outside)
	return e, outside
}

// icFigures posts the intercompany figures TestIntercompanyMatching
// computes by hand, in November 2026 (USD booked at 0.85, closing at
// 0.90). Company 1000 (USD): a receivable of 1,000.00 from 2000 (900.00
// EUR at closing) and of 50.00 from the outside company (45.00), a loan
// of 100.00 to 2000 (90.00) and revenue of 200.00 from 2000 (-170.00
// booked). Company 2000 (EUR): a payable of 895.00 to 1000, a receivable
// of 30.00 naming no trading partner, a loan of 90.00 from 1000 and an
// expense of 150.00 from 1000. Each against the bank.
func icFigures(t *testing.T, e fixture.Env, outside uuid.UUID) {
	t.Helper()
	day := "2026-11-10"
	postIC(t, e, e.US, "1000", "USD", day, icLine{"125000", "1000.00", &e.DE}, icLine{"125000", "50.00", &outside}, icLine{"100000", "-1050.00", nil})
	postIC(t, e, e.US, "1000", "USD", day, icLine{"126000", "100.00", &e.DE}, icLine{"100000", "-100.00", nil})
	postIC(t, e, e.US, "1000", "USD", day, icLine{"100000", "200.00", nil}, icLine{"425000", "-200.00", &e.DE})
	postIC(t, e, e.DE, "2000", "EUR", day, icLine{"100000", "895.00", nil}, icLine{"225000", "-895.00", &e.US})
	postIC(t, e, e.DE, "2000", "EUR", day, icLine{"125000", "30.00", nil}, icLine{"100000", "-30.00", nil})
	postIC(t, e, e.DE, "2000", "EUR", day, icLine{"100000", "90.00", nil}, icLine{"226000", "-90.00", &e.US})
	postIC(t, e, e.DE, "2000", "EUR", day, icLine{"525000", "150.00", &e.US}, icLine{"100000", "-150.00", nil})
}

// moneyOf shows a Money as "amount currency", "-" for none.
func moneyOf(m any) string {
	if m == nil {
		return "-"
	}
	x := m.(map[string]any)
	return x["amount"].(string) + " " + x["currency"].(string)
}

// differenceLines reads a page of differences as one line per difference.
func differenceLines(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		d := it.(map[string]any)
		el := d["elimination"].(map[string]any)
		partner := "-"
		if p, ok := d["partner"].(map[string]any); ok {
			partner = fmt.Sprint(p["code"], " ", p["name"])
		}
		out = append(out, fmt.Sprintf("#%v %s %s: %s %s / %s: %s, %s + %s = %s within %s", d["line_no"], el["code"], el["name"], d["company_code"],
			d["company_name"], partner, d["status"], moneyOf(d["amount"]), moneyOf(d["partner_amount"]), moneyOf(d["difference"]), moneyOf(d["tolerance"])))
	}
	return out
}

// entryLines reads a page of entries as one line per entry and per line.
func entryLines(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		en := it.(map[string]any)
		el, partner := "-", "-"
		if x, ok := en["elimination"].(map[string]any); ok {
			el = fmt.Sprint(x["code"], " ", x["name"])
		}
		if p, ok := en["partner"].(map[string]any); ok {
			partner = fmt.Sprint(p["code"], " ", p["name"])
		}
		out = append(out, fmt.Sprintf("entry %v %s %s: %s %s / %s, debits %s", en["entry_no"], en["kind"], el, en["company_code"], en["company_name"], partner,
			moneyOf(en["debits"])))
		for _, x := range en["lines"].([]any) {
			l := x.(map[string]any)
			g := l["group_account"].(map[string]any)
			company, account, tp := "-", "", ""
			if c, ok := l["company_code"].(string); ok {
				company = c + " " + l["company_name"].(string)
			}
			if a, ok := l["account"].(map[string]any); ok {
				account = fmt.Sprint(" ", a["number"], " ", a["name"])
			}
			if p, ok := l["trading_partner"].(map[string]any); ok {
				tp = fmt.Sprint(" tp ", p["code"])
			}
			out = append(out, fmt.Sprintf("  %v %s %s %s%s%s: %s", l["line_no"], g["number"], g["name"], company, account, tp, moneyOf(l["amount"])))
		}
	}
	return out
}

// TestIntercompanyMatching (ADR-0059 §8): icFigures' run matches per
// elimination and pair in the group currency: the receivables and
// payables differ by 5.00, within IC_BALANCES' 10.00; the loans match;
// the revenue and expense differ by 20.00, above IC_RESULTS' 1.00, and
// are open; the receivable from the outside company is outside_group and
// 2000's receivable naming none no_trading_partner, neither eliminated.
// Each pair's entry reverses its rows and puts its difference on G6950,
// summing to zero; the statements show no balance between the run's
// companies on the eliminations' accounts. The run counts its
// differences; the lists filter, page and total.
func TestIntercompanyMatching(t *testing.T) {
	e, _ := icEnv(t)
	run, _ := start(t, e)
	carryOut(t, e, app.Consolidation(app.Config{}), run)
	path := runsPath + "/" + run.String()
	page, _ := e.Must(200, "GET", path+"/differences", "", "")
	want := []string{
		"#1 IC_BALANCES Intercompany receivables and payables: 1000 Acme Corp US / 2000 Acme GmbH: within_tolerance, 900.00 EUR + -895.00 EUR = 5.00 EUR within 10.00 EUR",
		"#2 IC_BALANCES Intercompany receivables and payables: 1000 Acme Corp US / 9000 Outside Holdings: outside_group, 45.00 EUR + - = - within -",
		"#3 IC_BALANCES Intercompany receivables and payables: 2000 Acme GmbH / -: no_trading_partner, 30.00 EUR + - = - within -",
		"#4 IC_LOANS Intercompany loans: 1000 Acme Corp US / 2000 Acme GmbH: matched, 90.00 EUR + -90.00 EUR = 0.00 EUR within 0.00 EUR",
		"#5 IC_RESULTS Intercompany revenue and expenses: 1000 Acme Corp US / 2000 Acme GmbH: open, -170.00 EUR + 150.00 EUR = -20.00 EUR within 1.00 EUR",
	}
	if got, w := strings.Join(differenceLines(page), "\n"), strings.Join(want, "\n"); got != w {
		t.Errorf("the differences: got\n%s\nwant\n%s", got, w)
	}
	if got, w := totalLines(page), "matched 1 90.00 EUR -90.00 EUR 0.00 EUR | within_tolerance 1 900.00 EUR -895.00 EUR 5.00 EUR | "+
		"open 1 -170.00 EUR 150.00 EUR -20.00 EUR | outside_group 1 45.00 EUR - - | no_trading_partner 1 30.00 EUR - -"; got != w {
		t.Errorf("the differences' totals: got %s, want %s", got, w)
	}
	entries, _ := e.Must(200, "GET", path+"/entries", "", "")
	want = []string{
		"entry 1 intercompany IC_BALANCES Intercompany receivables and payables: 1000 Acme Corp US / 2000 Acme GmbH, debits 900.00 EUR",
		"  1 G1250 Intercompany receivables 1000 Acme Corp US 125000 Intercompany receivables tp 2000: -900.00 EUR",
		"  2 G2250 Intercompany payables 2000 Acme GmbH 225000 Intercompany payables tp 1000: 895.00 EUR",
		"  3 G6950 Intercompany differences -: 5.00 EUR",
		"entry 2 intercompany IC_LOANS Intercompany loans: 1000 Acme Corp US / 2000 Acme GmbH, debits 90.00 EUR",
		"  1 G1260 Intercompany loans 1000 Acme Corp US 126000 Intercompany loans receivable tp 2000: -90.00 EUR",
		"  2 G2260 Intercompany borrowings 2000 Acme GmbH 226000 Intercompany loans payable tp 1000: 90.00 EUR",
		"entry 3 intercompany IC_RESULTS Intercompany revenue and expenses: 1000 Acme Corp US / 2000 Acme GmbH, debits 170.00 EUR",
		"  1 G4250 Intercompany revenue 1000 Acme Corp US 425000 Intercompany revenue tp 2000: 170.00 EUR",
		"  2 G5250 Intercompany expenses 2000 Acme GmbH 525000 Intercompany expenses tp 1000: -150.00 EUR",
		"  3 G6950 Intercompany differences -: -20.00 EUR",
	}
	if got, w := strings.Join(entryLines(entries), "\n"), strings.Join(want, "\n"); got != w {
		t.Errorf("the entries: got\n%s\nwant\n%s", got, w)
	}
	if got, w := fmt.Sprint(entries["totals"]), "[map[count:3 debits:map[amount:1160.00 currency:EUR] kind:intercompany]]"; got != w {
		t.Errorf("the entries' totals: got %s, want %s", got, w)
	}
	got, _ := e.Must(200, "GET", path, "", "")
	if c, w := fmt.Sprint(got["difference_counts"]), "[map[count:1 status:matched] map[count:1 status:within_tolerance] map[count:1 status:open] "+
		"map[count:1 status:outside_group] map[count:1 status:no_trading_partner]]"; c != w {
		t.Errorf("the run's difference counts: got %s, want %s", c, w)
	}
	eliminatedStatements(t, e, run)
	differenceFilters(t, e, run)
}

// totalLines shows a differences page's totals.
func totalLines(page map[string]any) string {
	var out []string
	for _, x := range page["totals"].([]any) {
		t := x.(map[string]any)
		out = append(out, fmt.Sprint(t["status"], " ", t["count"], " ", moneyOf(t["amount"]), " ", moneyOf(t["partner_amount"]), " ", moneyOf(t["difference"])))
	}
	return strings.Join(out, " | ")
}

// eliminatedStatements: the run's statements carry its entries in the
// eliminations column, leaving only the outside company's and the
// unnamed receivable on G1250, nothing on the other eliminations'
// accounts and the differences on G6950, and still sum to zero.
func eliminatedStatements(t *testing.T, e fixture.Env, run uuid.UUID) {
	t.Helper()
	bs, _ := e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	pl, _ := e.Must(200, "GET", statementPath(run, "profit_and_loss"), "", "")
	rows := map[string]string{}
	for _, page := range []map[string]any{bs, pl} {
		for _, it := range page["items"].([]any) {
			r := it.(map[string]any)
			rows[r["number"].(string)] = columnsOf(r)
		}
	}
	for account, w := range map[string]string{
		"G1250": "975.00 EUR / -900.00 EUR / 75.00 EUR",
		"G1260": "90.00 EUR / -90.00 EUR / 0.00 EUR",
		"G2250": "-895.00 EUR / 895.00 EUR / 0.00 EUR",
		"G2260": "-90.00 EUR / 90.00 EUR / 0.00 EUR",
		"G4250": "-170.00 EUR / 170.00 EUR / 0.00 EUR",
		"G5250": "150.00 EUR / -150.00 EUR / 0.00 EUR",
		"G6950": "0.00 EUR / -15.00 EUR / -15.00 EUR",
	} {
		if rows[account] != w {
			t.Errorf("%s: got %s, want %s", account, rows[account], w)
		}
	}
	if got, w := statementTotals(bs, "assets", "liabilities", "equity", "profit"), "assets 1015.00 EUR / -990.00 EUR / 25.00 EUR, "+
		"liabilities -985.00 EUR / 985.00 EUR / 0.00 EUR, equity -10.00 EUR / 0.00 EUR / -10.00 EUR, profit -20.00 EUR / 5.00 EUR / -15.00 EUR"; got != w {
		t.Errorf("the balance sheet's totals: got %s, want %s (each column summing to zero)", got, w)
	}
}

// differenceFilters: the differences and entries by status, elimination,
// company (its own or as the partner) and kind, each with its totals, and
// paged by line and entry number.
func differenceFilters(t *testing.T, e fixture.Env, run uuid.UUID) {
	t.Helper()
	path := runsPath + "/" + run.String()
	lines := func(page map[string]any, key string) string {
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, fmt.Sprint(it.(map[string]any)[key]))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, query, want, totals string }{
		{"open", "status=open", "5", "open 1 -170.00 EUR 150.00 EUR -20.00 EUR"},
		{"IC_BALANCES", "elimination=IC_BALANCES", "1 2 3", "within_tolerance 1 900.00 EUR -895.00 EUR 5.00 EUR | outside_group 1 45.00 EUR - - | " +
			"no_trading_partner 1 30.00 EUR - -"},
		{"company 2000, as company or partner", "legal_entity_id=" + e.DE.String(), "1 3 4 5", "matched 1 90.00 EUR -90.00 EUR 0.00 EUR | " +
			"within_tolerance 1 900.00 EUR -895.00 EUR 5.00 EUR | open 1 -170.00 EUR 150.00 EUR -20.00 EUR | no_trading_partner 1 30.00 EUR - -"},
		{"none", "status=accepted", "", ""},
	} {
		t.Run("differences "+c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", path+"/differences?"+c.query, "", "")
			if got := lines(page, "line_no"); got != c.want {
				t.Errorf("the differences: got %s, want %s", got, c.want)
			}
			if got := totalLines(page); got != c.totals {
				t.Errorf("the totals: got %s, want %s", got, c.totals)
			}
		})
	}
	for _, c := range []struct{ name, query, want, totals string }{
		{"intercompany", "kind=intercompany", "1 2 3", "[map[count:3 debits:map[amount:1160.00 currency:EUR] kind:intercompany]]"},
		{"IC_RESULTS", "elimination=IC_RESULTS", "3", "[map[count:1 debits:map[amount:170.00 currency:EUR] kind:intercompany]]"},
		{"company 2000 as the partner", "legal_entity_id=" + e.DE.String(), "1 2 3", "[map[count:3 debits:map[amount:1160.00 currency:EUR] kind:intercompany]]"},
		{"the investment", "kind=investment", "", "[]"},
	} {
		t.Run("entries "+c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", path+"/entries?"+c.query, "", "")
			if got := lines(page, "entry_no"); got != c.want {
				t.Errorf("the entries: got %s, want %s", got, c.want)
			}
			if got := fmt.Sprint(page["totals"]); got != c.totals {
				t.Errorf("the totals: got %s, want %s", got, c.totals)
			}
		})
	}
	for _, list := range []struct{ name, key, want string }{{"differences", "line_no", "1 2 3 4 5"}, {"entries", "entry_no", "1 2 3"}} {
		var seen []string
		next := path + "/" + list.name + "?page_size=2"
		for next != "" {
			page, _ := e.Must(200, "GET", next, "", "")
			if s := lines(page, list.key); s != "" {
				seen = append(seen, s)
			}
			next = ""
			if tok, ok := page["next_page_token"].(string); ok {
				next = path + "/" + list.name + "?page_size=2&page_token=" + tok
			}
		}
		if got := strings.Join(seen, " "); got != list.want {
			t.Errorf("the %s by pages: got %s, want %s", list.name, got, list.want)
		}
		e.Problem(400, "request.invalid_page_token", "GET", path+"/"+list.name+"?page_token=x", "", "")
	}
}

// TestIntercompanyMatchingTwice (ADR-0012): the elimination step run
// again, after a commit whose completion was lost, writes nothing twice;
// before every company is translated it is refused and writes nothing; a
// caller who may not act in every company of the group finds no run's
// differences or entries; a page size of 0 is refused.
func TestIntercompanyMatchingTwice(t *testing.T) {
	e, outside := icEnv(t)
	h := app.Consolidation(app.Config{})
	run, _ := start(t, e)
	step := func(fn func(ctx context.Context) error) error {
		t.Helper()
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, fn)
	}
	var companies []uuid.UUID
	if err := step(func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := step(func(ctx context.Context) error { return h.TranslateMember(ctx, run, companies[0]) }); err != nil {
		t.Fatal(err)
	}
	if err := step(func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) }); err == nil ||
		!strings.Contains(err.Error(), "1 of the companies of run") {
		t.Errorf("eliminating before every company is translated: got %v, want it refused", err)
	}
	if err := step(func(ctx context.Context) error { return h.TranslateMember(ctx, run, companies[1]) }); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := step(func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) }); err != nil {
			t.Fatalf("eliminate %d: %v", i+1, err)
		}
	}
	var counts string
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM consolidation.run_difference WHERE run_id = $1) || ' ' ||
		(SELECT count(*) FROM consolidation.run_entry WHERE run_id = $1) || ' ' || (SELECT count(*) FROM consolidation.run_entry_line WHERE run_id = $1)`,
		[]any{run}, &counts)
	if counts != "5 3 8" {
		t.Errorf("differences, entries and lines after two eliminations: got %s, want 5 3 8", counts)
	}
	if err := step(func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	if err := step(func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) }); err != nil {
		t.Errorf("eliminating a computed run: got %v, want it left alone", err)
	}
	reader := e.As(e.Reader(t, "Reader of the group and the outside company", e.US, outside))
	for _, list := range []string{"differences", "entries"} {
		reader.Problem(404, "request.not_found", "GET", runsPath+"/"+run.String()+"/"+list, "", "")
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"differences": func(ctx context.Context) error {
			_, err := h.ListIntercompanyDifferences(ctx, gen.ListIntercompanyDifferencesParams{ID: run, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"entries": func(ctx context.Context) error {
			_, err := h.ListConsolidationEntries(ctx, gen.ListConsolidationEntriesParams{ID: run, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn); problem.From(err).Code != "request.invalid_page_size" {
			t.Errorf("the %s' page size 0: got %v, want request.invalid_page_size", name, err)
		}
	}
}

// TestIntercompanyPartnerNames: a difference's partner outside the group
// is named only to a caller who may act in it (product owner 2026-11-05);
// the run's own companies are named to every caller who may read the run.
func TestIntercompanyPartnerNames(t *testing.T) {
	e, outside := icEnv(t)
	run, _ := start(t, e)
	carryOut(t, e, app.Consolidation(app.Config{}), run)
	reader := e.As(e.Reader(t, "Reader of the group", e.US, e.DE))
	page, _ := reader.Must(200, "GET", runsPath+"/"+run.String()+"/differences?status=outside_group", "", "")
	p := page["items"].([]any)[0].(map[string]any)["partner"]
	if got, w := fmt.Sprint(p), "map[id:"+outside.String()+"]"; got != w {
		t.Errorf("the outside partner to a reader of the group: got %s, want its id alone", got)
	}
	entries, _ := reader.Must(200, "GET", runsPath+"/"+run.String()+"/entries?page_size=1", "", "")
	if got, w := entryLines(entries)[0], "entry 1 intercompany IC_BALANCES Intercompany receivables and payables: 1000 Acme Corp US / 2000 Acme GmbH, "+
		"debits 900.00 EUR"; got != w {
		t.Errorf("the first entry to a reader of the group: got %s, want %s", got, w)
	}
}

// TestIntercompanyToleranceFinerThanTheCurrency: a tolerance finer than
// the group currency, in a version activated before Setup refused it, is
// the elimination step's error, which leaves the run running and writes
// nothing.
func TestIntercompanyToleranceFinerThanTheCurrency(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withEliminations(doc)
		doc.Consolidation.Eliminations[2].Tolerance = "0.001"
	}, runPermissions...)
	sameCalendar(t, e)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	h := app.Consolidation(app.Config{})
	run, _ := start(t, e)
	var companies []uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		if companies, err = h.PrepareConsolidationRun(ctx, run); err != nil {
			return err
		}
		for _, c := range companies {
			if err := h.TranslateMember(ctx, run, c); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
	if err == nil || !strings.Contains(err.Error(), `tolerance "0.001" of elimination IC_LOANS in EUR`) {
		t.Errorf("a tolerance finer than the cent: got %v, want the step's error naming it", err)
	}
}
