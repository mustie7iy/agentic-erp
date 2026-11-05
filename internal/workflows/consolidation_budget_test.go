package workflows_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// budgetAccounts is the number of accounts with balances of each company
// of the budget's group.
const budgetAccounts = 1000

// budgetChart is a chart of budgetAccounts accounts, B00000 to B00999, a
// fifth of each type, each mapped to the group account of its type.
func budgetChart() []config.Account {
	types := []config.AccountType{config.Asset, config.Liability, config.Equity, config.Revenue, config.Expense}
	group := map[config.AccountType]string{config.Asset: "G1000", config.Liability: "G2000", config.Equity: "G3000", config.Revenue: "G4000",
		config.Expense: "G5000"}
	out := make([]config.Account, budgetAccounts)
	for i := range out {
		t := types[i%len(types)]
		out[i] = config.Account{Number: fmt.Sprintf("B%05d", i), Name: fmt.Sprintf("Budget account %d", i), Type: t, GroupAccount: group[t]}
	}
	return out
}

// TestConsolidationRunBudget checks ADR-0059 §14's budget for a
// consolidation run (01-requirements §1.3: asynchronous, p95 120 s, p99
// 240 s, as a depreciation run's): a group of 100 companies, each with
// 1,000 accounts with balances and 1,000 trading partner balances. The
// fixture's group ACME (company 2000, in EUR, the group currency) gains
// 98 members in USD beside 1000, every company a chart of 1,000 accounts
// (budgetChart) in a configuration version written straight into the
// tables, as are the companies' accounts, November's period and its
// balances: each account's balance and half of it with a trading
// partner, the parent's naming 1000 and each member's the parent (written
// without the triggers a posting runs, as TestDepreciationRunBudget seeds
// its assets). The run is measured once, from its start (POST) to
// computed, carried out by the in-process runner (Consolidator), and
// checked: every company translated, two rows per account, each company's
// rows summing to zero.
func TestConsolidationRunBudget(t *testing.T) {
	nfr.Measure(t)
	e := fixture.NewWith(t, nil, fixture.Consolidation, "consolidation.read", "consolidation.run.manage", "ledger.entry.read")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	start := time.Now()
	seedBudgetGroup(t, e)
	t.Logf("seeded a group of 100 companies with 1,000 accounts and 1,000 trading partner balances each in %v", time.Since(start))
	c := &workflows.Consolidations{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Runs: app.Consolidation(app.Config{}),
		Actors: identity.Actors{}}
	w := &workflows.Consolidator{Consolidations: c, Log: slog.New(slog.DiscardHandler)}
	start = time.Now()
	s := &conScenario{e: e, c: c}
	in := s.start(t)
	if err := w.Consolidate(ctx, in); err != nil {
		t.Fatalf("the run: %v", err)
	}
	elapsed := time.Since(start)
	var got string
	pgtest.Scan(t, `SELECT r.status || ' ' || (SELECT count(*) FROM consolidation.run_member m WHERE m.run_id = r.id AND m.translated_at IS NOT NULL)
			|| ' ' || (SELECT count(*) FROM consolidation.run_balance b WHERE b.run_id = r.id AND b.kind = 'balance')
			|| ' ' || (SELECT count(*) FROM (SELECT legal_entity_id FROM consolidation.run_balance b WHERE b.run_id = r.id
				GROUP BY 1 HAVING sum(amount_gc) <> 0) x)
		FROM consolidation.run r WHERE r.id = $1`, []any{in.RunID}, &got)
	if want := fmt.Sprintf("computed 100 %d 0", 100*2*budgetAccounts); got != want {
		t.Errorf("the run: got %s, want %s (computed, 100 companies translated, two rows per account, each company's rows summing to zero)", got, want)
	}
	t.Logf("consolidation run of 100 companies: %v", elapsed)
	if budget := nfr.Budget(120 * time.Second); elapsed > budget {
		t.Errorf("consolidation run: %v exceeds the %v budget (p95 120 s scaled by ERP_BUDGET_SCALE)", elapsed, budget)
	}
}

// seedBudgetGroup writes TestConsolidationRunBudget's group (see there).
func seedBudgetGroup(t *testing.T, e fixture.Env) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		pgtest.Exec(t, sql, args...)
	}
	doc := fixture.Document(t)
	fixture.Consolidation(doc)
	group := &doc.Consolidation.Groups[0]
	parent := doc.Company("2000")
	for i := 1; i <= 98; i++ {
		co := *doc.Company("1000")
		co.Code, co.Name = fmt.Sprintf("C%03d", i), fmt.Sprintf("Budget member %d", i)
		doc.Companies = append(doc.Companies, co)
		parent.ChartOfAccounts = append(parent.ChartOfAccounts, config.Account{Number: fmt.Sprintf("15%04d", i), Name: "Investment", Type: config.Asset,
			GroupAccount: "G1500"})
		group.Members = append(group.Members, config.GroupMember{Company: co.Code, OwnershipPercent: "100", FirstPeriod: "2026-11",
			InvestmentAccount: fmt.Sprintf("15%04d", i)})
	}
	for i := range doc.Companies {
		doc.Companies[i].ChartOfAccounts = append(doc.Companies[i].ChartOfAccounts, budgetChart()...)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	exec(`INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, activated_by) VALUES ($1, gen_random_uuid(), 2, $2, $3, 'bootstrap', $4)`,
		e.Tenant.ID, raw, sum[:], e.Tenant.Human.ID)
	exec(`UPDATE config.active_version SET number = 2, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	// The members on the parent's calendar, in USD; their charts and the
	// leading ledger's November.
	exec(`UPDATE tenancy.legal_entity u SET fiscal_calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.id = $1 AND d.id = $2`, e.US, e.DE)
	exec(`INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'C' || lpad(n::text, 3, '0'), 'Budget member ' || n, 'US', 'USD', d.fiscal_calendar_id, 'active', $3, $3
		FROM generate_series(1, 98) n, tenancy.legal_entity d WHERE d.id = $2`, e.Tenant.ID, e.DE, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.chart_of_accounts (tenant_id, id, code, name, created_by)
		SELECT $1, gen_random_uuid(), code, name, $2 FROM tenancy.legal_entity WHERE tenant_id = $1 AND code LIKE 'C%'`, e.Tenant.ID, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), c.id, 'B' || lpad(n::text, 5, '0'), 'Budget account ' || n,
			(ARRAY['asset', 'liability', 'equity', 'revenue', 'expense'])[n % 5 + 1], 'active', $2, $2
		FROM ledger.chart_of_accounts c, generate_series(0, $3 - 1) n WHERE c.tenant_id = $1`, e.Tenant.ID, e.Tenant.Human.ID, budgetAccounts)
	exec(`INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, le.id, a.id, $2, $2 FROM tenancy.legal_entity le
		JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.tenant_id = c.tenant_id AND a.chart_id = c.id AND a.number LIKE 'B%'
		WHERE le.tenant_id = $1`, e.Tenant.ID, e.Tenant.Human.ID)
	exec(`UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, e.US, e.DE)
	exec(`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
		SELECT $1, gen_random_uuid(), le.id, l.id, le.fiscal_calendar_id, 2026, 11, 'open', $2
		FROM tenancy.legal_entity le, ledger.ledger l WHERE le.tenant_id = $1 AND le.code LIKE 'C%' AND l.tenant_id = $1 AND l.is_leading`,
		e.Tenant.ID, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	// Each account's balance, alternately a debit and a credit of 100.00
	// to 1,099.00, so each company's sum is zero, its group amount at
	// 0.90; half of it with the trading partner.
	exec(`INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc, debit_tc,
			credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, ca.legal_entity_id, ca.account_id, 2026, 11, le.local_currency, d, c, d, c, d * 9 / 10, c * 9 / 10
		FROM ledger.company_account ca JOIN ledger.gl_account a ON a.id = ca.account_id AND a.number LIKE 'B%'
		JOIN tenancy.legal_entity le ON le.id = ca.legal_entity_id, ledger.ledger l,
		LATERAL (SELECT 10000 * (substr(a.number, 2)::int / 2 + 1) AS m) x,
		LATERAL (SELECT CASE WHEN substr(a.number, 2)::int % 2 = 0 THEN x.m ELSE 0 END AS d, CASE WHEN substr(a.number, 2)::int % 2 = 1 THEN x.m ELSE 0 END AS c) y
		WHERE ca.tenant_id = $1 AND l.tenant_id = $1 AND l.is_leading`, e.Tenant.ID)
	exec(`INSERT INTO ledger.trading_partner_balance (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year, fiscal_period,
			currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT tenant_id, ledger_id, legal_entity_id, account_id, CASE WHEN legal_entity_id = $2 THEN $3::uuid ELSE $2::uuid END, fiscal_year, fiscal_period,
			currency_tc, debit_tc / 2, credit_tc / 2, debit_lc / 2, credit_lc / 2, debit_gc / 2, credit_gc / 2
		FROM ledger.period_balance WHERE tenant_id = $1 AND fiscal_year = 2026 AND fiscal_period = 11`, e.Tenant.ID, e.DE, e.US)
	exec(`ANALYZE ledger.period_balance, ledger.trading_partner_balance, ledger.gl_account, ledger.company_account`)
}
