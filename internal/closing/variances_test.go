package closing_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withVariances is withTemplate with company 2000's variance report
// against the prior period and the prior year, flagging variances of
// 100.00 and 10 %, and journal number ranges of 2025, so that its periods
// exist for the comparisons of 2026's first period.
func withVariances(doc *config.Config) {
	withTemplate(doc)
	doc.Company("2000").Close.Variance = &config.CloseVariance{Bases: []string{config.BasisPriorPeriod, config.BasisPriorYear},
		ThresholdAmount: "100.00", ThresholdPercent: "10"}
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_sa", FiscalYear: 2025, First: 3000000,
		Last: 3999999})
}

// post posts an entry of company 2000 in EUR on the day, as a context
// does: each line an account number of its chart and a signed amount in
// minor units, the bank taking the balance.
func post(t *testing.T, e fixture.Env, day string, lines map[string]money.Minor) {
	t.Helper()
	var ls []ledger.PostingLine
	var bank money.Minor
	for number, amount := range lines {
		ls, bank = append(ls, ledger.PostingLine{AccountID: e.Account(t, "2000", number), Amount: amount}), bank-amount
	}
	postOn(t, e, day, append(ls, ledger.PostingLine{AccountID: e.Account(t, "2000", "100000"), Amount: bank})...)
}

// activate makes the active document, changed by set, a new active
// version, as an approved change would; set may write what the
// configuration's validation would refuse, which only a document written
// past the configuration context has.
func activate(t *testing.T, tenant uuid.UUID, set func(*config.Config)) {
	t.Helper()
	var raw string
	pgtest.Scan(t, `SELECT v.document::text FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number
		WHERE v.tenant_id = $1`, []any{tenant}, &raw)
	var doc config.Config
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	set(&doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, $2::jsonb, v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		tenant, string(b))
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, tenant)
}

// variances reads a page of a close's variance report as its header,
// totals and one line per row ("number actual comparison variance
// percent flagged"), the next page's token last as "next:...".
func variances(t *testing.T, c apitest.Client, close, query string) (string, []string) {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/period-closes/"+close+"/variances?"+query, "", "")
	amount := func(m map[string]any, k string) any { return m[k].(map[string]any)["amount"] }
	tot := page["totals"].(map[string]any)
	head := fmt.Sprintf("%v %v %v/%v %v %v %v against %v/%v threshold %v %v %v: actual %v comparison %v variance %v, %v rows, %v flagged", page["company_code"],
		page["company_name"], page["fiscal_year"], page["fiscal_period"], page["ledger_code"], page["currency"], page["basis"], page["comparison_fiscal_year"],
		page["comparison_fiscal_period"], amount(page, "threshold_amount"), page["threshold_percent"], page["plan_category_code"], amount(tot, "actual"),
		amount(tot, "comparison"), amount(tot, "variance"), tot["rows"], tot["rows_requiring_comment"])
	var rows []string
	for _, x := range apitest.List(t, page, "items") {
		r := x.(map[string]any)
		pct, ok := r["variance_percent"]
		if !ok {
			pct = "-"
		}
		rows = append(rows, fmt.Sprintf("%v %v %v %v %v %v", r["account_number"], amount(r, "actual"), amount(r, "comparison"), amount(r, "variance"), pct,
			r["requires_comment"]))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		rows = append(rows, "next:"+tok)
	}
	return head, rows
}

// postedPeriods posts company 2000's entries: in 2025, office supplies
// 200.00 in January; office supplies 1,000.00, raw materials 4,800.00 and
// 500.00 of revenue in December. In 2026, office supplies 1,500.00, IT
// 50.00, raw materials 5,000.00 and 500.00 of revenue in January; office
// supplies 1,200.00, salaries 300.00 and 900.00 of revenue in February.
func postedPeriods(t *testing.T, e fixture.Env) {
	t.Helper()
	post(t, e, "2025-01-15", map[string]money.Minor{"520000": 20000})
	post(t, e, "2025-12-10", map[string]money.Minor{"520000": 100000, "510000": 480000, "400000": -50000})
	post(t, e, "2026-01-20", map[string]money.Minor{"520000": 150000, "530000": 5000, "510000": 500000, "400000": -50000})
	post(t, e, "2026-02-05", map[string]money.Minor{"520000": 120000, "610000": 30000, "400000": -90000})
}

// TestVariancesAgainstPriorPeriods: rows computed by hand. January 2026
// against December 2025 (the prior period of period 1 is the previous
// year's twelfth) and against January 2025; February against January.
// A row is flagged at 100.00 and 10 %: raw materials' 200.00 is only
// 4.17 %, so not; a row without a percent (a zero comparison) by the
// amount alone; IT's 50.00 is below the amount. Accounts at zero both
// sides have no row, an account at zero in the period but not in the
// comparison has one, and the totals' actuals are the period's profit and
// loss in the trial balance.
func TestVariancesAgainstPriorPeriods(t *testing.T) {
	e := fixture.NewWith(t, nil, withVariances, closePermissions...)
	postedPeriods(t, e)
	jan, _ := open(t, e.Client, e.DE, 1, "")
	feb, _ := open(t, e.Client, e.DE, 2, "")
	cases := []struct {
		name, close, basis, head string
		rows                     []string
	}{
		{"January against December", apitest.String(t, jan, "id"), "prior_period",
			"2000 Acme GmbH 2026/1 0L EUR prior_period against 2025/12 threshold 100.00 10 <nil>: actual 6050.00 comparison 5300.00 variance 750.00, 4 rows, 1 flagged",
			[]string{"400000 -500.00 -500.00 0.00 0.00 false", "510000 5000.00 4800.00 200.00 4.17 false", "520000 1500.00 1000.00 500.00 50.00 true",
				"530000 50.00 0.00 50.00 - false"}},
		{"January against January 2025", apitest.String(t, jan, "id"), "prior_year",
			"2000 Acme GmbH 2026/1 0L EUR prior_year against 2025/1 threshold 100.00 10 <nil>: actual 6050.00 comparison 200.00 variance 5850.00, 4 rows, 3 flagged",
			[]string{"400000 -500.00 0.00 -500.00 - true", "510000 5000.00 0.00 5000.00 - true", "520000 1500.00 200.00 1300.00 650.00 true",
				"530000 50.00 0.00 50.00 - false"}},
		{"February against January", apitest.String(t, feb, "id"), "prior_period",
			"2000 Acme GmbH 2026/2 0L EUR prior_period against 2026/1 threshold 100.00 10 <nil>: actual 600.00 comparison 6050.00 variance -5450.00, 5 rows, 4 flagged",
			[]string{"400000 -900.00 -500.00 -400.00 -80.00 true", "510000 0.00 5000.00 -5000.00 -100.00 true", "520000 1200.00 1500.00 -300.00 -20.00 true",
				"530000 0.00 50.00 -50.00 -100.00 false", "610000 300.00 0.00 300.00 - true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, rows := variances(t, e.Client, tc.close, "basis="+tc.basis)
			if head != tc.head {
				t.Errorf("report:\ngot  %q\nwant %q", head, tc.head)
			}
			wantLines(t, "rows", rows, tc.rows)
		})
	}
	for period, want := range map[int]string{1: "6050.00", 2: "600.00"} {
		if got := trialProfitAndLoss(t, e, period); got != want {
			t.Errorf("the trial balance's profit and loss of period %d: got %s, want %s, the report's", period, got, want)
		}
	}
}

// trialProfitAndLoss is the net of company 2000's profit and loss accounts
// in the trial balance of 2026's period, as the walkthrough adds it up.
func trialProfitAndLoss(t *testing.T, e fixture.Env, period int) string {
	t.Helper()
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/balances?legal_entity_id=%s&fiscal_year=2026&fiscal_period=%d&page_size=500", e.DE, period), "", "")
	pl := map[string]bool{}
	for _, n := range []string{"400000", "510000", "520000", "530000", "540000", "610000"} {
		pl[e.Account(t, "2000", n).String()] = true
	}
	var net money.Minor
	for _, x := range apitest.List(t, page, "items") {
		b := x.(map[string]any)
		if !pl[b["account_id"].(string)] {
			continue
		}
		d, err := money.ParseDecimal(b["debit_lc"].(map[string]any)["amount"].(string), 2)
		if err != nil {
			t.Fatal(err)
		}
		c, err := money.ParseDecimal(b["credit_lc"].(map[string]any)["amount"].(string), 2)
		if err != nil {
			t.Fatal(err)
		}
		net += d - c
	}
	return net.Plain(2)
}

// TestVariancesPagedAndFiltered: rows page by account number, the totals
// on every page the whole report's; the filter keeps the rows that do or
// do not require a comment, totals unchanged; a token is valid only with
// the filters it was made with.
func TestVariancesPagedAndFiltered(t *testing.T) {
	e := fixture.NewWith(t, nil, withVariances, closePermissions...)
	postedPeriods(t, e)
	feb, _ := open(t, e.Client, e.DE, 2, "")
	id := apitest.String(t, feb, "id")
	whole, _ := variances(t, e.Client, id, "basis=prior_period")
	head, first := variances(t, e.Client, id, "basis=prior_period&page_size=2")
	if head != whole {
		t.Errorf("first page's header and totals: got %q, want the whole report's %q", head, whole)
	}
	wantLines(t, "first page", first[:2], []string{"400000 -900.00 -500.00 -400.00 -80.00 true", "510000 0.00 5000.00 -5000.00 -100.00 true"})
	token := strings.TrimPrefix(first[2], "next:")
	head, second := variances(t, e.Client, id, "basis=prior_period&page_size=2&page_token="+url.QueryEscape(token))
	if head != whole {
		t.Errorf("second page's header and totals: got %q, want the whole report's %q", head, whole)
	}
	wantLines(t, "second page", second[:2], []string{"520000 1200.00 1500.00 -300.00 -20.00 true", "530000 0.00 50.00 -50.00 -100.00 false"})
	_, third := variances(t, e.Client, id, "basis=prior_period&page_size=2&page_token="+url.QueryEscape(strings.TrimPrefix(second[2], "next:")))
	wantLines(t, "last page", third, []string{"610000 300.00 0.00 300.00 - true"})

	_, flagged := variances(t, e.Client, id, "basis=prior_period&requires_comment=true&page_size=3")
	wantLines(t, "the flagged rows", flagged[:3], []string{"400000 -900.00 -500.00 -400.00 -80.00 true", "510000 0.00 5000.00 -5000.00 -100.00 true",
		"520000 1200.00 1500.00 -300.00 -20.00 true"})
	_, rest := variances(t, e.Client, id, "basis=prior_period&requires_comment=true&page_size=3&page_token="+url.QueryEscape(strings.TrimPrefix(flagged[3], "next:")))
	wantLines(t, "the flagged rows' last page", rest, []string{"610000 300.00 0.00 300.00 - true"})
	head, unflagged := variances(t, e.Client, id, "basis=prior_period&requires_comment=false")
	if head != whole {
		t.Errorf("filtered header and totals: got %q, want the whole report's %q", head, whole)
	}
	wantLines(t, "the rows below the thresholds", unflagged, []string{"530000 0.00 50.00 -50.00 -100.00 false"})

	for name, query := range map[string]string{"another filter": "basis=prior_period&requires_comment=true", "another basis": "basis=prior_year"} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(400, "request.invalid_page_token", "GET", "/v1/period-closes/"+id+"/variances?"+query+"&page_token="+url.QueryEscape(token), "", "")
		})
	}
}

// TestVariancesBasisNotConfigured: a basis the company does not configure,
// one no company can, and any basis of a company without variance settings
// is refused on the field; a close of a company the caller cannot act in
// is not found.
func TestVariancesBasisNotConfigured(t *testing.T) {
	e := fixture.NewWith(t, nil, withVariances, closePermissions...)
	de, _ := open(t, e.Client, e.DE, 1, "")
	us, _ := open(t, e.Client, e.US, 1, "")
	cases := map[string]string{
		"the plan, not configured":    apitest.String(t, de, "id") + "/variances?basis=plan",
		"a basis no company has":      apitest.String(t, de, "id") + "/variances?basis=forecast",
		"a company without variances": apitest.String(t, us, "id") + "/variances?basis=prior_period",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			p := e.In(t).Problem(422, "closing.basis_not_configured", "GET", "/v1/period-closes/"+path, "", "")
			if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "basis" {
				t.Errorf("field errors: got %v, want one on basis", p["errors"])
			}
		})
	}
	reader := e.As(e.Reader(t, "Reader of 1000", e.US))
	reader.Problem(404, "request.not_found", "GET", "/v1/period-closes/"+apitest.String(t, de, "id")+"/variances?basis=prior_period", "", "")
}
