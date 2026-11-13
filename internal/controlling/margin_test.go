package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// margined is a tenant whose configuration counts company 2000's accounts
// 510000 and 530000 to 540000 as cost of sales, with a second profit
// center and the accounts' ids.
type margined struct {
	plan
	pc2                        uuid.UUID
	materials, services, wages uuid.UUID // 510000, 530000 (cost of sales), 610000 (an expense that is none)
}

// reconfigure makes the tenant's active configuration a new version whose
// document is the SQL expression over the current one (v.document, with
// $2 the argument), as an approved change would leave it.
func (p plan) reconfigure(t *testing.T, document, arg string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, `+document+`, v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		p.Tenant.ID, arg)
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, p.Tenant.ID)
}

// nameCostOfSales makes the configuration's margin report section the
// JSON given.
func (p plan) nameCostOfSales(t *testing.T, section string) {
	t.Helper()
	p.reconfigure(t, `jsonb_set(v.document, '{controlling,margin_report}', $2::jsonb)`, section)
}

// setupMargined posts company 2000's May, which the projector has
// reached, and June, which it has not: revenue and cost of sales on PC1
// (entered, and derived from cost center 1000), on PC2 and on no profit
// center (cost center 2000 has none), a credit note, and expenses that
// are no cost of sales.
func setupMargined(t *testing.T) margined {
	t.Helper()
	m := margined{plan: setup(t)}
	m.nameCostOfSales(t, `[{"chart": "2000", "cost_of_sales_accounts": [{"from": "510000"}, {"from": "530000", "to": "540000"}]}]`)
	pc, _ := m.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC2", "name": "PC 2", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, m.DE), "")
	m.Approve(t, pc["pending_change_id"].(string))
	m.pc2 = uuid.MustParse(pc["id"].(string))
	m.materials, m.services, m.wages = m.Account(t, "2000", "510000"), m.Account(t, "2000", "530000"), m.Account(t, "2000", "610000")
	in := func(l ledger.PostingLine, pc uuid.UUID) ledger.PostingLine {
		l.ProfitCenterID = &pc
		return l
	}
	m.post(t, "2026-05-10", in(on(m.revenue, -10000, nil), m.c.Profit), on(m.materials, 6000, &m.c.Admin), on(m.bank, 4000, nil))
	fixture.Project(t)
	m.post(t, "2026-06-15", on(m.revenue, -8000, &m.c.Sales), on(m.services, 3000, &m.c.Sales), in(on(m.revenue, -2500, nil), m.pc2),
		in(on(m.revenue, 500, nil), m.c.Profit), on(m.office, 1000, &m.c.Admin), on(m.wages, 700, &m.c.Sales), on(m.bank, 5300, nil))
	return m
}

// report reads a company's 2026 margin report as lines: the selection
// with its cost of sales accounts, each row, the totals, and the next
// page's token last as "next:...".
func (p plan) report(t *testing.T, le uuid.UUID, query string) []string {
	t.Helper()
	r, _ := p.Must(200, "GET", "/v1/margin-report?legal_entity_id="+le.String()+"&fiscal_year=2026"+query, "", "")
	amounts := func(v any) string {
		a := v.(map[string]any)
		part := func(k string) string {
			m := a[k].(map[string]any)
			return fmt.Sprint(m["amount"], " ", m["currency"])
		}
		return fmt.Sprintf("revenue %s cost %s margin %s", part("revenue"), part("cost_of_sales"), part("margin"))
	}
	var numbers []string
	for _, n := range r["cost_of_sales_accounts"].([]any) {
		numbers = append(numbers, n.(string))
	}
	out := []string{fmt.Sprintf("periods %v-%v in %v, cost of sales [%s] named %v", r["fiscal_period_from"], r["fiscal_period_to"], r["currency"],
		strings.Join(numbers, " "), r["cost_of_sales_named"])}
	for _, it := range r["items"].([]any) {
		row := it.(map[string]any)
		name := "unassigned"
		if row["unassigned"] != true {
			name = fmt.Sprint(row["profit_center_code"], " ", row["profit_center_name"])
			if _, err := uuid.Parse(fmt.Sprint(row["profit_center_id"])); err != nil {
				t.Errorf("row %s: profit_center_id %v is no id", name, row["profit_center_id"])
			}
		}
		out = append(out, name+": "+amounts(row))
	}
	out = append(out, "totals: "+amounts(r["totals"]))
	if tok, ok := r["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

// TestMarginReport: per profit center the revenue is the revenue
// accounts' credits less debits, the cost of sales the named accounts'
// debits less credits and the margin their difference, exact from the
// projected balances and the entries still queued; the lines without a
// profit center are one last row; the rows add up to the company's totals.
func TestMarginReport(t *testing.T) {
	m := setupMargined(t)
	const head = "cost of sales [510000 530000 540000] named true"
	year := []string{"periods 1-16 in EUR, " + head,
		"PC1 PC 1: revenue 95.00 EUR cost 60.00 EUR margin 35.00 EUR",
		"PC2 PC 2: revenue 25.00 EUR cost 0.00 EUR margin 25.00 EUR",
		"unassigned: revenue 80.00 EUR cost 30.00 EUR margin 50.00 EUR",
		"totals: revenue 200.00 EUR cost 90.00 EUR margin 110.00 EUR"}
	wantList(t, "the year, queued entries included", m.report(t, m.DE, ""), year)
	fixture.Project(t)
	wantList(t, "the year, every entry projected", m.report(t, m.DE, ""), year)
	wantList(t, "May alone", m.report(t, m.DE, "&fiscal_period_from=5&fiscal_period_to=5"), []string{"periods 5-5 in EUR, " + head,
		"PC1 PC 1: revenue 100.00 EUR cost 60.00 EUR margin 40.00 EUR", "totals: revenue 100.00 EUR cost 60.00 EUR margin 40.00 EUR"})
	wantList(t, "a period without postings", m.report(t, m.DE, "&fiscal_period_from=7"), []string{"periods 7-16 in EUR, " + head,
		"totals: revenue 0.00 EUR cost 0.00 EUR margin 0.00 EUR"})
	t.Run("a profit center whose revenue and cost of sales are zero has no row", func(t *testing.T) {
		wantList(t, "the year after PC2's revenue moved to no profit center", m.reversedPC2(t), []string{"periods 1-16 in EUR, " + head, year[1],
			"unassigned: revenue 105.00 EUR cost 30.00 EUR margin 75.00 EUR", year[4]})
	})
}

// reversedPC2 moves PC2's revenue to no profit center and returns the
// year's report.
func (m margined) reversedPC2(t *testing.T) []string {
	t.Helper()
	back := on(m.revenue, 2500, nil)
	back.ProfitCenterID = &m.pc2
	m.post(t, "2026-06-20", back, on(m.revenue, -2500, &m.c.Sales))
	return m.report(t, m.DE, "")
}

// TestMarginReportPages: profit centers by code, then the unassigned row,
// never more rows than asked for, with the totals on every page.
func TestMarginReportPages(t *testing.T) {
	m := setupMargined(t)
	all := m.report(t, m.DE, "")
	head, totals := all[0], all[4]
	for size, want := range map[int][][]string{
		1: {{all[1]}, {all[2]}, {all[3]}},
		2: {{all[1], all[2]}, {all[3]}},
		3: {{all[1], all[2], all[3]}},
	} {
		t.Run(fmt.Sprintf("pages of %d", size), func(t *testing.T) {
			token := ""
			for i, rows := range want {
				got := m.report(t, m.DE, fmt.Sprintf("&page_size=%d%s", size, token))
				next := ""
				if last := got[len(got)-1]; strings.HasPrefix(last, "next:") {
					got, next = got[:len(got)-1], last[len("next:"):]
				}
				wantList(t, fmt.Sprintf("page %d", i+1), got, append(append([]string{head}, rows...), totals))
				if (next != "") != (i < len(want)-1) {
					t.Fatalf("page %d: next page token %q, want one only before the last page", i+1, next)
				}
				token = "&page_token=" + next
			}
		})
	}
}

// TestMarginReportWithoutNamedAccounts: a chart the configuration names
// no cost of sales accounts for shows its revenue, a zero cost of sales,
// and says so; without profit center accounting every line is unassigned.
func TestMarginReportWithoutNamedAccounts(t *testing.T) {
	m := setupMargined(t)
	revenue, materials, bank := m.Account(t, "1000", "400000"), m.Account(t, "1000", "510000"), m.Account(t, "1000", "100000")
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'test', $2)`, m.Tenant.ID, m.Tenant.Human.ID)
	if err := apitest.Committed(t, m.Tenant, m.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}.Post(ctx,
			ledger.Posting{LegalEntityID: m.US, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "USD", SourceContext: "payables",
				SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{on(revenue, -5000, nil), on(materials, 2000, nil), on(bank, 3000, nil)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantList(t, "company 1000", m.report(t, m.US, ""), []string{"periods 1-16 in USD, cost of sales [] named false",
		"unassigned: revenue 50.00 USD cost 0.00 USD margin 50.00 USD", "totals: revenue 50.00 USD cost 0.00 USD margin 50.00 USD"})
}

// TestMarginReportRefusals: a wrong selection, a company the caller may
// not act in, and a caller without either permission.
func TestMarginReportRefusals(t *testing.T) {
	m := setupMargined(t)
	path := "/v1/margin-report?fiscal_year=2026&legal_entity_id="
	m.Problem(400, "request.invalid_parameter", "GET", path+m.DE.String()+"&fiscal_period_from=6&fiscal_period_to=5", "", "")
	m.Problem(400, "request.invalid_page_token", "GET", path+m.DE.String()+"&page_token=x", "", "")
	first := m.report(t, m.DE, "&page_size=1")
	m.Problem(400, "request.invalid_page_token", "GET", path+m.DE.String()+"&fiscal_period_to=6&page_token="+strings.TrimPrefix(first[len(first)-1], "next:"), "", "")
	m.Problem(422, "request.reference_not_found", "GET", path+uuid.NewString(), "", "")
	m.as(m.Reader(t, "us-only", m.US)).Problem(422, "request.reference_not_found", "GET", path+m.DE.String(), "", "")
	for _, perm := range []string{"controlling.margin.read", "ledger.entry.read"} {
		t.Run("without "+perm, func(t *testing.T) {
			p := setup(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, perm)
			p.Problem(403, "identity.permission_missing", "GET", path+p.DE.String(), "", "")
		})
	}
	h := endpoints(t)
	err := apitest.InRequest(t, m.Tenant, m.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetMarginReport(ctx, gen.GetMarginReportParams{LegalEntityID: m.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(0)})
		return err
	})
	want(t, "page size 0, which only a direct call can send", problem.From(err).Code, "request.invalid_page_size")
}

// TestMarginReportIsExactOrFails: a margin beyond what an amount holds
// fails the read; it is never shown wrapped.
func TestMarginReportIsExactOrFails(t *testing.T) {
	m := setupMargined(t)
	fixture.Project(t)
	pgtest.Exec(t, `UPDATE ledger.period_balance SET credit_lc = 9223372036854775807 WHERE tenant_id = $1 AND account_id = $2 AND fiscal_period = 5`,
		m.Tenant.ID, m.revenue)
	pgtest.Exec(t, `UPDATE ledger.period_balance SET credit_lc = 9223372036854775807 WHERE tenant_id = $1 AND account_id = $2 AND fiscal_period = 5`,
		m.Tenant.ID, m.materials)
	h := endpoints(t)
	err := apitest.InRequest(t, m.Tenant, m.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetMarginReport(ctx, gen.GetMarginReportParams{LegalEntityID: m.DE, FiscalYear: 2026})
		return err
	})
	if err == nil {
		t.Error("a margin that does not fit: got a report, want an error")
	}
}
