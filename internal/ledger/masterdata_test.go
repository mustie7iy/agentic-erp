package ledger_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestFiscalPeriods(t *testing.T) {
	e := setup(t)
	list := func(q string) (int, []map[string]any, string) {
		page, _ := e.Must(200, "GET", "/v1/fiscal-periods?"+q, "", "")
		var out []map[string]any
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any))
		}
		tok, _ := page["next_page_token"].(string)
		return len(out), out, tok
	}
	de := "legal_entity_id=" + e.de.String()
	if n, ps, _ := list(de + "&fiscal_year=2026&order_by=period&page_size=50"); n != 16 || ps[12]["is_special"] != true ||
		ps[12]["start_date"] != "2026-12-01" || ps[15]["end_date"] != "2026-12-31" || ps[0]["status"] != "open" || ps[12]["status"] != "closed" {
		t.Fatalf("periods: %d %v", n, ps)
	}
	if n, _, _ := list(de + "&status=closed"); n != 4 {
		t.Fatalf("closed: %d", n)
	}
	n, first, tok := list(de + "&order_by=period%20desc&page_size=10")
	if n != 10 || first[0]["period"] != float64(16) || tok == "" {
		t.Fatalf("page 1: %d %v", n, first)
	}
	if n, _, tok := list(de + "&order_by=period%20desc&page_size=10&page_token=" + tok); n != 6 || tok != "" {
		t.Fatalf("page 2: %d", n)
	}
	if n, _, tok := list("page_size=20&fiscal_year=2026"); n != 20 || tok == "" {
		t.Fatalf("by year, all entities: %d", n)
	}
	_, ps, _ := list(de + "&order_by=period&page_size=1")
	ledgerID := ps[0]["ledger_id"].(string)
	if n, _, _ := list("ledger_id=" + ledgerID + "&page_size=100"); n != 32 {
		t.Fatalf("by ledger: %d", n)
	}
	if n, _, _ := list("ledger_id=" + uuid.NewString()); n != 0 {
		t.Fatalf("unknown ledger: %d", n)
	}

	id := ps[0]["id"].(string)
	got, tag := e.Must(200, "GET", "/v1/fiscal-periods/"+id, "", "")
	if got["period"] != float64(1) || tag != `"2"` {
		t.Fatalf("get: %v %s", got, tag)
	}
	e.Problem(409, "ledger.invalid_period_status", "POST", "/v1/fiscal-periods/"+id+":open", "", tag)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/fiscal-periods/"+id+":close", "", `"1"`)
	closed, tag := e.Must(200, "POST", "/v1/fiscal-periods/"+id+":close", "", tag)
	if closed["status"] != "closed" || tag != `"3"` {
		t.Fatalf("closed: %v", closed)
	}
	e.Problem(409, "ledger.invalid_period_status", "POST", "/v1/fiscal-periods/"+id+":close", "", tag)
	opened, _ := e.Must(200, "POST", "/v1/fiscal-periods/"+id+":open", "", tag)
	if opened["status"] != "open" || opened["updated_by"] != e.ten.Human.ID.String() {
		t.Fatalf("opened: %v", opened)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'locked', version = version + 1 WHERE id = $1`, id)
	e.Problem(409, "ledger.invalid_period_status", "POST", "/v1/fiscal-periods/"+id+":open", "", "*")
	e.Problem(404, "request.not_found", "GET", "/v1/fiscal-periods/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "POST", "/v1/fiscal-periods/"+uuid.NewString()+":open", "", "*")
	o, c := events(t, e.ten, "period_opened"), events(t, e.ten, "period_closed")
	if len(o) != 1 || len(c) != 1 || c[0]["previous_status"] != "open" || c[0]["start_date"] != "2026-01-01" {
		t.Fatalf("events: %v %v", o, c)
	}
	agent := e.Client
	agent.As = e.ten.Agent
	agent.Problem(404, "request.not_found", "GET", "/v1/fiscal-periods/"+id, "", "")
}

func TestExchangeRates(t *testing.T) {
	e := setup(t)
	r, tag := e.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": "2026-01-01", "rate": "0.92", "source": "ECB"}`, "")
	if r["rate"] != "0.920000000000" || len(tag) < 10 {
		t.Fatalf("rate: %v", r)
	}
	e.rate(t, "USD", "EUR", "2026-02-01", "0.93")
	e.rate(t, "GBP", "EUR", "2026-01-15", "1.17")
	for name, c := range map[string]struct{ body, code string }{
		"duplicate":     {`{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": "2026-01-01", "rate": "0.9", "source": "x"}`, "ledger.exchange_rate_exists"},
		"same currency": {`{"rate_type": "average", "from_currency": "USD", "to_currency": "USD", "valid_from": "2026-01-01", "rate": "1", "source": "x"}`, "ledger.same_currency"},
		"unknown from":  {`{"rate_type": "average", "from_currency": "XXQ", "to_currency": "USD", "valid_from": "2026-01-01", "rate": "1", "source": "x"}`, "ledger.unknown_currency"},
		"unknown to":    {`{"rate_type": "average", "from_currency": "USD", "to_currency": "XXQ", "valid_from": "2026-01-01", "rate": "1", "source": "x"}`, "ledger.unknown_currency"},
		"zero":          {`{"rate_type": "average", "from_currency": "USD", "to_currency": "GBP", "valid_from": "2026-01-01", "rate": "0", "source": "x"}`, "ledger.invalid_rate"},
		"type":          {`{"rate_type": "spot", "from_currency": "USD", "to_currency": "GBP", "valid_from": "2026-01-01", "rate": "1", "source": "x"}`, "ledger.unknown_rate_type"},
	} {
		code, m, _ := e.Do("POST", "/v1/exchange-rates", c.body, "")
		if m["code"] != c.code || (code != 409 && code != 422) {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	got, gotTag := e.Must(200, "GET", "/v1/exchange-rates/"+r["id"].(string), "", "")
	if got["source"] != "ECB" || gotTag != tag {
		t.Fatalf("get: %v %s %s", got, gotTag, tag)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/exchange-rates/"+uuid.NewString(), "", "")
	rates := func(q string) string {
		page, _ := e.Must(200, "GET", "/v1/exchange-rates?"+q, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["rate"].(string)[:4])
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return strings.Join(out, ",")
	}
	if got := rates("valid_on=2026-01-20"); got != "0.92,1.17" {
		t.Fatalf("valid on: %s", got)
	}
	if got := rates("from_currency=USD&to_currency=EUR&rate_type=average&order_by=valid_from%20desc"); got != "0.93,0.92" {
		t.Fatalf("pair: %s", got)
	}
	page1 := rates("page_size=2")
	parts := strings.Split(page1, ",")
	if len(parts) != 3 {
		t.Fatalf("page 1: %s", page1)
	}
	if got := rates("page_size=2&page_token=" + strings.TrimPrefix(parts[2], "next:")); got != "0.93" {
		t.Fatalf("page 2: %s", got)
	}
	if m := events(t, e.ten, "exchange_rate_maintained"); len(m) != 3 || m[0]["rate"] != "0.920000000000" {
		t.Fatalf("events: %v", m)
	}
}

func TestAccounts(t *testing.T) {
	e := setup(t)
	ap := e.account(t, "2000", "211000")
	a, tag := e.Must(200, "GET", "/v1/accounts/"+ap, "", "")
	s := a["company_settings"].([]any)
	if a["reconciliation_kind"] != "supplier" || a["is_balance_sheet"] != true || a["status"] != "active" || len(s) != 1 ||
		s[0].(map[string]any)["is_open_item_managed"] != true || tag != `"1"` {
		t.Fatalf("account: %v", a)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/accounts/"+uuid.NewString(), "", "")
	count := func(q string) (int, string) {
		page, _ := e.Must(200, "GET", "/v1/accounts?"+q, "", "")
		tok, _ := page["next_page_token"].(string)
		return len(page["items"].([]any)), tok
	}
	if n, _ := count("legal_entity_id=" + e.de.String()); n != 10 {
		t.Fatalf("by legal entity: %d", n)
	}
	if n, _ := count("chart_id=" + a["chart_id"].(string) + "&status=active&number=211000"); n != 1 {
		t.Fatalf("filtered: %d", n)
	}
	n, tok := count("page_size=15")
	if n != 15 || tok == "" {
		t.Fatalf("page 1: %d", n)
	}
	if n, tok := count("page_size=15&page_token=" + tok); n != 5 || tok != "" {
		t.Fatalf("page 2: %d", n)
	}
	agent := e.Client
	agent.As = e.ten.Agent
	if got, _ := agent.Must(200, "GET", "/v1/accounts/"+ap, "", ""); len(got["company_settings"].([]any)) != 0 {
		t.Fatalf("agent sees settings: %v", got)
	}
	if page, _ := agent.Must(200, "GET", "/v1/accounts?legal_entity_id="+e.de.String(), "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("agent lists: %v", page)
	}
}

// TestSetup checks the configuration section: what it refuses and how a
// later document changes accounts and periods.
func TestSetup(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	check := func(base, proposed *config.Config) []string {
		var out []string
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := s.Check(ctx, base, proposed)
			for _, f := range fs {
				out = append(out, f.Path+": "+f.Message)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := document(t)
	if got := check(base, document(t)); len(got) != 0 {
		t.Fatalf("unchanged document: %v", got)
	}
	bad := document(t)
	bad.Companies[0].Currency = "EUR"
	bad.Companies[0].FiscalYearStartMonth = 4
	bad.Companies[0].ChartOfAccounts[0].Number = "abc"
	bad.Companies[0].ChartOfAccounts[1].Type = core.Liability
	bad.Companies[0].ChartOfAccounts[2].VendorReconciliation = false
	extra := bad.Companies[1]
	extra.Code = "3000"
	bad.Companies[1] = extra
	got := strings.Join(check(base, bad), "\n")
	for _, want := range []string{"companies[1000].currency: must be the legal entity's local currency USD",
		"companies[1000].fiscal_year_start_month: the legal entity's fiscal calendar starts its years in month 1",
		"companies[1000].chart_of_accounts[abc]: account numbers", "chart_of_accounts[140000].type: an account's type cannot change",
		"chart_of_accounts[211000].vendor_reconciliation", "companies[3000].code: no legal entity has company code 3000",
		"companies[2000]: a company cannot be removed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if got := check(nil, bad); len(got) != 4 {
		t.Errorf("first version: %v", got)
	}

	// A later document: an account renamed, one removed and one added, a
	// new fiscal year, and a third company whose years start in April.
	cal := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), '3000', 'Acme UK', 'GB', 'GBP', $2, 'active', $3, $3)`, e.ten.ID, cal, e.ten.Human.ID)
	next := document(t)
	de := &next.Companies[1]
	de.ChartOfAccounts[4].Name = "Revenue"
	removed := de.ChartOfAccounts[len(de.ChartOfAccounts)-1].Number
	de.ChartOfAccounts = append(de.ChartOfAccounts[:len(de.ChartOfAccounts)-1], config.Account{Number: "999000", Name: "Suspense", Type: core.Asset})
	uk := next.Companies[1]
	uk.Code, uk.Currency, uk.FiscalYearStartMonth = "3000", "GBP", 4
	next.Companies = append(next.Companies, uk)
	next.NumberRanges = append(next.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_sa", FiscalYear: 2027, First: 1, Last: 9},
		config.NumberRange{Company: "3000", DocumentType: "journal_sa", FiscalYear: 2026, First: 1, Last: 9})
	if got := check(base, next); len(got) != 0 {
		t.Fatalf("next document: %v", got)
	}
	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	apply(next)
	apply(next) // applying the same document again changes nothing
	status := func(company, number string) (string, string) {
		var st, name string
		pgtest.Admin(t).QueryRow(context.Background(), `SELECT a.status, a.name FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id
			WHERE c.tenant_id = $1 AND c.code = $2 AND a.number = $3`, e.ten.ID, company, number).Scan(&st, &name)
		return st, name
	}
	if st, _ := status("2000", removed); st != "inactive" {
		t.Errorf("removed account: %s", st)
	}
	if _, name := status("2000", "400000"); name != "Revenue" {
		t.Errorf("renamed account: %s", name)
	}
	if st, _ := status("2000", "999000"); st != "active" {
		t.Errorf("added account: %s", st)
	}
	var periods2027, ukStart, calendars string
	admin := pgtest.Admin(t)
	admin.QueryRow(context.Background(), `SELECT count(*)::text FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year = 2027`, e.de).Scan(&periods2027)
	admin.QueryRow(context.Background(), `SELECT start_date::text FROM ledger.fiscal_period WHERE calendar_id = $1 AND fiscal_year = 2026 AND period = 1`, cal).Scan(&ukStart)
	admin.QueryRow(context.Background(), `SELECT string_agg(code, ',' ORDER BY code) FROM ledger.fiscal_calendar WHERE tenant_id = $1`, e.ten.ID).Scan(&calendars)
	if periods2027 != "16" || ukStart != "2025-04-01" || calendars != "C001,C002,C003" {
		t.Errorf("periods %s, UK start %s, calendars %s", periods2027, ukStart, calendars)
	}
	apply(document(t)) // the removed account comes back
	noRanges := document(t)
	noRanges.NumberRanges = nil
	apply(noRanges) // no fiscal year to set up
	orphan := document(t)
	orphan.Companies[0].Code = "4000"
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, orphan) }); err == nil ||
		!strings.Contains(err.Error(), "no legal entity 4000") {
		t.Errorf("apply without a legal entity: %v", err)
	}
	if st, _ := status("2000", removed); st != "active" {
		t.Errorf("re-added account: %s", st)
	}

	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		ap := uuid.MustParse(e.account(t, "2000", "211000"))
		bank := uuid.MustParse(e.account(t, "2000", "100000"))
		var a ledger.Accounts
		if ok, err := a.IsReconciliationAccount(ctx, e.de, ap, "supplier"); !ok || err != nil {
			t.Errorf("payables account: %v %v", ok, err)
		}
		if ok, err := a.IsReconciliationAccount(ctx, e.de, bank, "supplier"); ok || err != nil {
			t.Errorf("bank account: %v %v", ok, err)
		}
		if ok, _ := a.IsReconciliationAccount(ctx, e.us, ap, "supplier"); ok {
			t.Error("account of another legal entity")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The stored document matches what was applied.
	var stored json.RawMessage
	admin.QueryRow(context.Background(), `SELECT document FROM config.config_version WHERE tenant_id = $1`, e.ten.ID).Scan(&stored)
	if len(stored) == 0 {
		t.Fatal("no configuration version")
	}
}
