package payables_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// regressionOf is the regression evidence of a change request: its summary
// and detail.
func regressionOf(t *testing.T, cr map[string]any) (string, map[string]any) {
	t.Helper()
	for _, ev := range apitest.List(t, cr, "evidence") {
		if m := ev.(map[string]any); m["kind"] == "regression" {
			if m["passed"] != true {
				t.Errorf("passed: got %v, want true, the change's verdict for every reader", m["passed"])
			}
			return m["summary"].(string), apitest.Object(t, m, "detail")
		}
	}
	t.Fatalf("regression evidence: got none in %v", cr["evidence"])
	return "", nil
}

// companiesOf lists the company of each case in the list, a case itself or
// the case of a replayed one.
func companiesOf(t *testing.T, list []any) []string {
	t.Helper()
	out := make([]string, 0, len(list))
	for _, v := range list {
		m := v.(map[string]any)
		if c, ok := m["case"].(map[string]any); ok {
			m = c
		}
		out = append(out, apitest.String(t, m, "company"))
	}
	return out
}

// onlyOf checks that every company listed is the one wanted ("": any).
func onlyOf(t *testing.T, what string, companies []string, want string) {
	t.Helper()
	for _, c := range companies {
		if want != "" && c != want {
			t.Errorf("%s of company %s: got one, want only %s", what, c, want)
		}
	}
}

// TestRegressionShownByCompany: one change request's regression evidence,
// and the suite it replayed, read by people of different company access,
// show each the cases of their own companies alone, numbered and counted
// over those; a
// person of no company sees none. The summary recorded (in the operation's
// result and the audit trail too) has no count, nor has the audit entry of
// the capture.
func TestRegressionShownByCompany(t *testing.T) {
	e, path, opPath := shownByCompany(t)
	cases := map[string]struct {
		as      pgtest.Actor
		summary string
		cases   int
		changed int
		company string // of every case shown
	}{
		"every company": {e.Tenant.Human, "Suite RS-0001: 2 of 4 cases decided differently under the proposal", 4, 2, ""},
		"company 2000":  {e.Reader(t, "DE", e.DE), "Suite RS-0001: 1 of 3 cases decided differently under the proposal", 3, 1, "2000"},
		"company 1000":  {e.Reader(t, "US", e.US), "Suite RS-0001: 1 of 1 cases decided differently under the proposal", 1, 1, "1000"},
		"no company":    {e.Reader(t, "None"), "Suite RS-0001: 0 of 0 cases decided differently under the proposal", 0, 0, "none"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := e.As(c.as)
			api.T = t
			got, _ := api.Must(200, "GET", path, "", "")
			summary, detail := regressionOf(t, got)
			if summary != c.summary {
				t.Errorf("summary: got %q, want %q", summary, c.summary)
			}
			if n := detail["cases"]; n != float64(c.cases) {
				t.Errorf("cases: got %v, want %d", n, c.cases)
			}
			changed := companiesOf(t, apitest.List(t, detail, "changed"))
			if len(changed) != c.changed {
				t.Errorf("changed: got %d, want %d", len(changed), c.changed)
			}
			onlyOf(t, "changed case", changed, c.company)
			for _, f := range apitest.List(t, detail, "changed") {
				if no := apitest.Object(t, f.(map[string]any), "case")["case_no"].(float64); no < 1 || int(no) > c.cases {
					t.Errorf("changed case number: got %v, want one of 1 to %d, numbered among the cases shown", no, c.cases)
				}
			}
			if n := len(apitest.List(t, detail, "regressions")); n != 0 {
				t.Errorf("regressions: got %d, want 0", n)
			}
			for _, key := range []string{"cases_by_company", "stale_suite"} {
				if v, ok := detail[key]; ok {
					t.Errorf("detail %s: got %v, want it withheld", key, v)
				}
			}
			list, _ := api.Must(200, "GET", "/v1/change-requests", "", "")
			if listed, _ := regressionOf(t, apitest.List(t, list, "items")[0].(map[string]any)); listed != c.summary {
				t.Errorf("listed summary: got %q, want %q", listed, c.summary)
			}
			suite, _ := api.Must(200, "GET", "/v1/regression-suites/"+apitest.String(t, detail, "suite_id"), "", "")
			checkSuite(t, suite, c.cases, c.company)
			numbered := map[string]any{}
			for _, cs := range apitest.List(t, suite, "cases") {
				numbered[apitest.String(t, cs.(map[string]any), "supplier_invoice_id")] = cs.(map[string]any)["case_no"]
			}
			for _, f := range apitest.List(t, detail, "changed") {
				cs := apitest.Object(t, f.(map[string]any), "case")
				if got, want := cs["case_no"], numbered[apitest.String(t, cs, "supplier_invoice_id")]; got != want {
					t.Errorf("changed case number: got %v, want %v, the suite's number for this reader", got, want)
				}
			}
			page, _ := api.Must(200, "GET", "/v1/regression-suites", "", "")
			checkSuite(t, apitest.List(t, page, "items")[0].(map[string]any), c.cases, c.company)
		})
	}
	t.Run("recorded", func(t *testing.T) {
		done, _ := e.Must(200, "GET", opPath, "", "")
		if got, want := apitest.Object(t, done, "result")["regression_summary"], "Suite RS-0001 replayed under the proposal."; got != want {
			t.Errorf("operation result regression_summary: got %v, want %q", got, want)
		}
		var audited string
		pgtest.Scan(t, `SELECT detail->>'regression_summary' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'config.change_request.evidence'`,
			[]any{e.Tenant.ID}, &audited)
		if audited != "Suite RS-0001 replayed under the proposal." {
			t.Errorf("audited regression_summary: got %q, want no count", audited)
		}
		var counted bool
		pgtest.Scan(t, `SELECT detail ? 'cases' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'config.regression_suite.capture'`,
			[]any{e.Tenant.ID}, &counted)
		if counted {
			t.Error("audited capture: got a count of cases, want none")
		}
	})
}

// checkSuite checks what one reader is shown of the suite: the cases of
// their companies, counted and numbered among themselves, with exactly the
// strata that selected them.
func checkSuite(t *testing.T, suite map[string]any, cases int, company string) {
	t.Helper()
	listed := apitest.List(t, suite, "cases")
	if n := suite["case_count"]; n != float64(cases) || len(listed) != cases {
		t.Errorf("suite cases: got %v counted and %d listed, want %d", n, len(listed), cases)
	}
	onlyOf(t, "suite case", companiesOf(t, listed), company)
	want := []any{}
	for i, c := range listed {
		m := c.(map[string]any)
		if no := m["case_no"]; no != float64(i+1) {
			t.Errorf("suite case %d: got number %v, want %d, numbered among the cases shown", i+1, no, i+1)
		}
		for _, why := range strings.Split(apitest.String(t, m, "why"), "; ") {
			if !slices.Contains(want, any(why)) {
				want = append(want, why)
			}
		}
	}
	if got := apitest.List(t, suite, "strata"); !slices.Equal(got, want) {
		t.Errorf("suite strata: got %v, want %v", got, want)
	}
}
