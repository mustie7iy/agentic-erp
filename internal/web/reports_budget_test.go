package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
)

// accountJSON is an account of the chart, a supplier reconciliation
// account when supplier.
func accountJSON(n int, supplier bool) string {
	kind := "none"
	if supplier {
		kind = "supplier"
	}
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","chart_id":%q,
		"number":"%06d","name":"An account named at the length of a long one, %d","account_type":"expense","is_balance_sheet":false,
		"reconciliation_kind":%q,"status":"active","company_settings":[]}`, uuid.New(), uuid.New(), uuid.New(), 100000+n, n, kind)
}

// many is n copies of one, as a JSON array's items.
func many(n int, one func() string) string {
	items := make([]string, n)
	for i := range items {
		items[i] = one()
	}
	return strings.Join(items, ",")
}

// TestReportBudgets holds each report's page to ADR-0052 §10's budgets of
// a page at its largest, whatever the company's volume (a page shows at
// most reportPageSize rows, the API's reads having their own budgets): at
// most six API calls (TestScreensAgainstTheContract), HTML of 100 KB at
// most, and the workspace's own time at most 30 ms p95 besides the API's,
// measured against a stub that answers at once with full pages, a chart
// of 500 accounts and five companies.
func TestReportBudgets(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	company, ledger, compare := uuid.New(), uuid.New(), uuid.New()
	m := moneyJSON("-1234567.89")
	accounts := make([]string, 500)
	for i := range accounts {
		accounts[i] = accountJSON(i, i < 3)
	}
	entities := many(5, func() string {
		return fmt.Sprintf(`{"id":%q,"code":"3000","name":"A company named at length, GmbH & Co. KG","country_code":"DE","local_currency":"EUR","status":"active"}`, uuid.New())
	})
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadEntries, permReadAccounts, permReadCompanies, permReadStatements, permReadAudit, permReadSuppliers)),
		"GET /v1/principal-names": answer(200, `{"items":[]}`),
		"GET /v1/ledgers":         answer(200, `{"items":[`+ledgerJSON(ledger, "0L", true, company)+","+ledgerJSON(compare, "2L", false, company)+`]}`),
		"GET /v1/accounts":        answer(200, `{"items":[`+strings.Join(accounts, ",")+`]}`),
		"GET /v1/legal-entities":  answer(200, `{"items":[`+entities+`]}`),
		"GET /v1/house-banks":     answer(200, `{"items":[]}`),
		"GET /v1/balances":        answer(200, `{"items":[`+many(reportPageSize, func() string { return balanceJSON(company) })+`],"next_page_token":"b2"}`),
		"GET /v1/open-items": answer(200, `{"items":[`+many(reportPageSize, func() string { return openItemJSON(company) })+`],"next_page_token":"o2","aging":[`+
			many(6, func() string {
				return `{"bucket":"overdue","days_from":1,"days_to":30,"currency":"EUR","item_count":1000,"amount_lc":` + m + `}`
			})+`]}`),
		"GET /v1/gr-ir-balances": answer(200, `{"items":[`+many(reportPageSize, func() string {
			return fmt.Sprintf(`{"legal_entity_id":%q,"purchase_order_id":%q,"po_item_no":2,"account_id":%q,"account_number":"219000","account_name":"GR/IR clearing","received_quantity":{"value":"3000","unit":"KG"},
				"invoiced_quantity":{"value":"1250.5","unit":"KG"},"cleared_quantity":{"value":"0","unit":"KG"},"migrated_quantity":{"value":"0","unit":"KG"},"balance":%s,"balance_lc":%s,"balance_gc":%s,"last_posting_date":"2026-03-05"}`,
				company, uuid.New(), uuid.New(), m, m, m)
		})+`],"next_page_token":"g2"}`),
		"GET /v1/ledger-reconciliation/entries": answer(200, `{"items":[`+many(reportPageSize, func() string {
			return fmt.Sprintf(`{"ledger_id":%q,"journal_entry_id":%q,"fiscal_year":2026,"line_no":12,"document_number":"1000004242","posting_date":"2026-03-16",
				"fiscal_period":3,"amount":%s,"amount_lc":%s,"amount_gc":%s}`, ledger, uuid.New(), m, m, m)
		})+`],"next_page_token":"e2"}`),
		"GET /v1/ledger-reconciliation": answer(200, fmt.Sprintf(`{"ledger_id":%q,"compare_ledger_id":%q,"trial_balances":[],"items":[%s],"next_page_token":"l2"}`,
			ledger, compare, many(reportPageSize, func() string {
				return fmt.Sprintf(`{"account_id":%q,"account_number":"400000","account_name":"Sales Revenue","currency":"EUR","balance":%s,"compare_balance":%s,"difference":%s,"difference_lc":%s,"difference_gc":%s}`,
					uuid.New(), m, m, m, m, m)
			}))),
		"GET /v1/audit-events": answer(200, `{"items":[`+many(auditPageSize, auditJSON)+`],"next_page_token":"a2"}`),
	}}
	_, b, _ := stubStack(t, st)
	co := url.Values{"legal_entity_id": {company.String()}, "fiscal_year": {"2026"}}
	entries := url.Values{"legal_entity_id": {company.String()}, "ledger_id": {ledger.String()}, "compare_ledger_id": {compare.String()},
		"account_id": {uuid.NewString()}, "currency": {"EUR"}}
	for _, path := range []string{"/reports", "/reports/trial-balance?" + co.Encode(), "/reports/open-items?" + co.Encode(), "/reports/gr-ir?" + co.Encode(),
		"/reports/ledger-reconciliation?" + co.Encode(), "/reports/ledger-reconciliation/entries?" + entries.Encode(),
		"/reports/audit?object_id=" + uuid.NewString()} {
		var times []time.Duration
		size := 0
		for range 20 {
			start := time.Now()
			r := b.get(path)
			times = append(times, time.Since(start))
			if r.status != http.StatusOK {
				t.Fatalf("%s: got %d", path, r.status)
			}
			size = len(r.body)
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("%s: %d bytes, median %v, p95 %v", path, size, times[len(times)/2], p95)
		if size > 100*1024 {
			t.Errorf("%s: %d bytes of HTML, want at most 100 KB", path, size)
		}
		if budget := nfr.Budget(30 * time.Millisecond); p95 > budget {
			t.Errorf("%s: p95 %v with an API answering at once, want at most %v (30 ms scaled by ERP_BUDGET_SCALE)", path, p95, budget)
		}
	}
}
