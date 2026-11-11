package workflows_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
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

// budgetRunLimit ends a budget's consolidation run that hangs; it is no
// budget. It bounds the run alone, from after the seeding: 8 minutes is
// twice the run's budget at the gates' ERP_BUDGET_SCALE=2 (2 x 120 s), so
// TestConsolidationRunBudget's slow run fails on its budget, with its
// time, before this stops it (TestConsolidatedStatementBudget measures
// its pages and only logs its run's time), and over twenty times what
// the run takes (19 s, and 22 s on the 5,000 group accounts,
// 2026-11-11). Until then it was started before the
// seeding, whose time it counted: main's run 43007697247 seeded for most
// of the 8 minutes (seedBudgetGroup) and the run that followed was cut
// off. The seeding is bounded by the package's limit (scripts/budgets.sh).
const budgetRunLimit = 8 * time.Minute

// budgetTypes are the account types budget accounts take in turn.
var budgetTypes = []config.AccountType{config.Asset, config.Liability, config.Equity, config.Revenue, config.Expense}

// budgetChart is a chart of budgetAccounts accounts, B00000 to B00999, a
// fifth of each type, each mapped to the group account of its type or,
// wide, to a group account of its own, GB00000 to GB00999
// (wideGroupChart).
func budgetChart(wide bool) []config.Account {
	group := map[config.AccountType]string{config.Asset: "G1000", config.Liability: "G2000", config.Equity: "G3000", config.Revenue: "G4000",
		config.Expense: "G5000"}
	out := make([]config.Account, budgetAccounts)
	for i := range out {
		t := budgetTypes[i%len(budgetTypes)]
		out[i] = config.Account{Number: fmt.Sprintf("B%05d", i), Name: fmt.Sprintf("Budget account %d", i), Type: t, GroupAccount: group[t]}
		if wide {
			out[i].GroupAccount = fmt.Sprintf("GB%05d", i)
		}
	}
	return out
}

// wideGroupChart extends the group chart to its largest, 5,000 accounts
// (ADR-0059 §3): GB00000 to GB00999, the budget accounts' own, and more
// accounts without figures, GX00000 on, the types in turn.
func wideGroupChart(cs *config.Consolidation) {
	for i := range budgetAccounts {
		cs.Accounts = append(cs.Accounts, config.GroupAccount{Number: fmt.Sprintf("GB%05d", i), Name: fmt.Sprintf("Budget group account %d", i),
			Type: budgetTypes[i%len(budgetTypes)]})
	}
	for i := 0; len(cs.Accounts) < 5000; i++ {
		cs.Accounts = append(cs.Accounts, config.GroupAccount{Number: fmt.Sprintf("GX%05d", i), Name: fmt.Sprintf("Group account %d", i),
			Type: budgetTypes[i%len(budgetTypes)]})
	}
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
// its assets), the asset and liability accounts' group accounts in the
// elimination, and the parent's investment in each member its share
// capital. The run is measured once, from its start (POST) to
// computed, carried out by the in-process runner (Consolidator), and
// checked: every company translated, two rows per account, each company's
// rows summing to zero, the 99 pairs of the parent and a member matched
// and eliminated and each company's rows naming no trading partner
// listed.
func TestConsolidationRunBudget(t *testing.T) {
	nfr.Measure(t)
	e := fixture.NewWith(t, nil, fixture.Consolidation, "consolidation.read", "consolidation.run.manage", "ledger.entry.read")
	start := time.Now()
	seedBudgetGroup(t, e, false)
	t.Logf("seeded a group of 100 companies with 1,000 accounts and 1,000 trading partner balances each in %v", time.Since(start))
	ctx, cancel := context.WithTimeout(context.Background(), budgetRunLimit)
	defer cancel()
	c := &workflows.Consolidations{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Runs: app.Consolidation(app.Config{}),
		Actors: identity.Actors{}}
	w := &workflows.Consolidator{Consolidations: c, Log: slog.New(slog.DiscardHandler)}
	start = time.Now()
	s := &conScenario{e: e, c: c}
	in := s.start(t)
	if err := w.Consolidate(ctx, in); err != nil {
		t.Fatalf("the run, after %v: %v", time.Since(start), err)
	}
	elapsed := time.Since(start)
	var got string
	pgtest.Scan(t, `SELECT r.status || ' ' || (SELECT count(*) FROM consolidation.run_member m WHERE m.run_id = r.id AND m.translated_at IS NOT NULL)
			|| ' ' || (SELECT count(*) FROM consolidation.run_balance b WHERE b.run_id = r.id AND b.kind = 'balance')
			|| ' ' || (SELECT count(*) FROM (SELECT legal_entity_id FROM consolidation.run_balance b WHERE b.run_id = r.id
				GROUP BY 1 HAVING sum(amount_gc) <> 0) x)
			|| ' ' || (SELECT count(*) FROM consolidation.run_difference d WHERE d.run_id = r.id AND d.partner_id IS NOT NULL)
			|| ' ' || (SELECT count(*) FROM consolidation.run_difference d WHERE d.run_id = r.id AND d.status = 'no_trading_partner')
			|| ' ' || (SELECT count(*) FROM consolidation.run_entry e WHERE e.run_id = r.id)
			|| ' ' || coalesce(r.problem->>'detail', '')
		FROM consolidation.run r WHERE r.id = $1`, []any{in.RunID}, &got)
	if want := fmt.Sprintf("computed 100 %d 0 99 100 198 ", 100*2*budgetAccounts+100); got != want {
		t.Errorf("the run: got %s, want %s (computed, 100 companies translated, two rows per account and the parent's investments and bank, each "+
			"company's rows summing to zero, 99 pairs and 100 companies' rows without a trading partner matched, 99 intercompany and 99 investment "+
			"entries)", got, want)
	}
	t.Logf("consolidation run of 100 companies: %v", elapsed)
	if budget := nfr.Budget(120 * time.Second); elapsed > budget {
		t.Errorf("consolidation run: %v exceeds the %v budget (p95 120 s scaled by ERP_BUDGET_SCALE)", elapsed, budget)
	}
}

// seedBudgetGroup writes TestConsolidationRunBudget's group (see there);
// wide, with wideGroupChart's group chart, each budget account mapped to
// a group account of its own.
//
// Its statements must plan alike whatever the package's database already
// holds. The second test to seed finds the first one's tenant in the
// tables and their statistics, which know no row of the new tenant: the
// planner then expected one row where there were 100,000 and joined by
// nested loops, and a join on an id alone walked the whole (tenant_id,
// id) index for every row. The company accounts' and the balances'
// statements took 16 s and 270 s so, instead of 1.4 s and 0.9 s (329 s
// for TestConsolidatedStatementBudget here, over 450 s on main's run
// 43007697247, which cut the run off). So each table is analyzed once it
// is written, before a statement reads it, and every join between
// tables names the tenant (a row looked up by an id given as a parameter
// needs none).
func seedBudgetGroup(t *testing.T, e fixture.Env, wide bool) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		pgtest.Exec(t, sql, args...)
	}
	doc := fixture.Document(t)
	fixture.Consolidation(doc)
	// The elimination holds the group accounts of the budget's asset and
	// liability accounts, so that matching and eliminating (ADR-0059 §8)
	// work on 400 rows with a trading partner per company.
	el := &doc.Consolidation.Eliminations[0]
	el.Accounts = []string{"G1000", "G2000"}
	if wide {
		wideGroupChart(&doc.Consolidation)
		el.Accounts = nil
		for i := range budgetAccounts {
			if ty := budgetTypes[i%len(budgetTypes)]; ty == config.Asset || ty == config.Liability {
				el.Accounts = append(el.Accounts, fmt.Sprintf("GB%05d", i))
			}
		}
	}
	group := &doc.Consolidation.Groups[0]
	for i := 1; i <= 98; i++ {
		co := *doc.Company("1000")
		co.Code, co.Name = fmt.Sprintf("C%03d", i), fmt.Sprintf("Budget member %d", i)
		doc.Companies = append(doc.Companies, co)
		// Looked up after the append, which may move the companies.
		parent := doc.Company("2000")
		parent.ChartOfAccounts = append(parent.ChartOfAccounts, config.Account{Number: fmt.Sprintf("15%04d", i), Name: "Investment", Type: config.Asset,
			GroupAccount: "G1500"})
		group.Members = append(group.Members, config.GroupMember{Company: co.Code, OwnershipPercent: "100", Origin: config.OriginIncorporated, FirstPeriod: "2026-11",
			InvestmentAccount: fmt.Sprintf("15%04d", i)})
	}
	for i := range doc.Companies {
		doc.Companies[i].ChartOfAccounts = append(doc.Companies[i].ChartOfAccounts, budgetChart(wide)...)
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
	// leading ledger's October (where their acquisitions are measured, ADR-0059 §9) and November.
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
	exec(`ANALYZE tenancy.legal_entity, ledger.chart_of_accounts, ledger.gl_account`)
	exec(`INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, le.id, a.id, $2, $2 FROM tenancy.legal_entity le
		JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.tenant_id = c.tenant_id AND a.chart_id = c.id AND a.number LIKE 'B%'
		WHERE le.tenant_id = $1`, e.Tenant.ID, e.Tenant.Human.ID)
	exec(`UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, e.US, e.DE)
	// The parent's investment accounts of the members (ADR-0059 §9); their
	// balances are written with the members' below. Every member's net
	// assets at acquisition are zero: October holds nothing.
	exec(`INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), c.id, '15' || lpad(n::text, 4, '0'), 'Investment', 'asset', 'active', $2, $2
		FROM ledger.chart_of_accounts c, generate_series(1, 98) n WHERE c.tenant_id = $1 AND c.code = '2000'`, e.Tenant.ID, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, a.id, $3, $3 FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.tenant_id = a.tenant_id AND c.id = a.chart_id
		WHERE a.tenant_id = $1 AND c.code = '2000' AND a.number ~ '^15[0-9]{4}$' AND a.number <> '150000'`, e.Tenant.ID, e.DE, e.Tenant.Human.ID)
	exec(`ANALYZE ledger.gl_account, ledger.company_account`)
	exec(`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
		SELECT $1, gen_random_uuid(), le.id, l.id, le.fiscal_calendar_id, 2026, p, CASE p WHEN 11 THEN 'open' ELSE 'closed' END, $2
		FROM tenancy.legal_entity le, ledger.ledger l, (VALUES (10), (11)) AS x(p) WHERE le.tenant_id = $1 AND le.code LIKE 'C%' AND l.tenant_id = $1 AND l.is_leading`,
		e.Tenant.ID, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.9, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	// Each account's balance, alternately a debit and a credit of 100.00
	// to 1,099.00, so each company's sum is zero, its group amount at
	// 0.90; half of it with the trading partner.
	exec(`INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc, debit_tc,
			credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, ca.legal_entity_id, ca.account_id, 2026, 11, le.local_currency, d, c, d, c, d * 9 / 10, c * 9 / 10
		FROM ledger.company_account ca JOIN ledger.gl_account a ON a.tenant_id = ca.tenant_id AND a.id = ca.account_id AND a.number LIKE 'B%'
		JOIN tenancy.legal_entity le ON le.tenant_id = ca.tenant_id AND le.id = ca.legal_entity_id, ledger.ledger l,
		LATERAL (SELECT 10000 * (substr(a.number, 2)::int / 2 + 1) AS m) x,
		LATERAL (SELECT CASE WHEN substr(a.number, 2)::int % 2 = 0 THEN x.m ELSE 0 END AS d, CASE WHEN substr(a.number, 2)::int % 2 = 1 THEN x.m ELSE 0 END AS c) y
		WHERE ca.tenant_id = $1 AND l.tenant_id = $1 AND l.is_leading`, e.Tenant.ID)
	exec(`ANALYZE ledger.period_balance`)
	exec(`INSERT INTO ledger.trading_partner_balance (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year, fiscal_period,
			currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT tenant_id, ledger_id, legal_entity_id, account_id, CASE WHEN legal_entity_id = $2 THEN $3::uuid ELSE $2::uuid END, fiscal_year, fiscal_period,
			currency_tc, debit_tc / 2, credit_tc / 2, debit_lc / 2, credit_lc / 2, debit_gc / 2, credit_gc / 2
		FROM ledger.period_balance WHERE tenant_id = $1 AND fiscal_year = 2026 AND fiscal_period = 11`, e.Tenant.ID, e.DE, e.US)
	// The parent's investment in each member, the member's share capital
	// (ADR-0059 As built T68h2): minus its equity balances in the group
	// currency, every member's the same, against the parent's bank, so
	// that each investment entry eliminates it.
	exec(`WITH capital AS (SELECT -sum(pb.debit_gc - pb.credit_gc) AS x FROM ledger.period_balance pb
			JOIN ledger.gl_account a ON a.tenant_id = pb.tenant_id AND a.id = pb.account_id AND a.account_type = 'equity'
			WHERE pb.tenant_id = $1 AND pb.legal_entity_id = $2),
		amounts AS (SELECT a.id, capital.x FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.tenant_id = a.tenant_id AND c.id = a.chart_id, capital
			WHERE a.tenant_id = $1 AND c.code = '2000' AND a.number ~ '^15[0-9]{4}$' UNION ALL SELECT $4::uuid, -99 * capital.x FROM capital)
		INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc, debit_tc,
			credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, $3, m.id, 2026, 11, 'EUR', greatest(m.x, 0), greatest(-m.x, 0), greatest(m.x, 0), greatest(-m.x, 0), greatest(m.x, 0),
			greatest(-m.x, 0) FROM amounts m, ledger.ledger l WHERE l.tenant_id = $1 AND l.is_leading`, e.Tenant.ID, e.US, e.DE, e.Account(t, "2000", "100000"))
	exec(`ANALYZE ledger.period_balance, ledger.trading_partner_balance, ledger.gl_account, ledger.company_account`)
}

// TestConsolidatedStatementBudget checks ADR-0059 §14's budget for a
// consolidated statement of one run, every page (01-requirements §1.3:
// p95 1 s, p99 3 s): TestConsolidationRunBudget's group of 100 companies,
// each with 1,000 accounts with balances and 1,000 trading partner
// balances, computed by its run, its 200,000 translated rows on a group
// chart of 5,000 accounts, the largest allowed (wideGroupChart), each
// budget account on one of its own. Each statement is read ten times by
// its first page of the default size and its first, a middle and its last
// page of 500, and checked: the balance sheet's totals and the profit
// summing to zero.
func TestConsolidatedStatementBudget(t *testing.T) {
	nfr.Measure(t)
	e := fixture.NewWith(t, nil, fixture.Consolidation, "consolidation.read", "consolidation.run.manage", "ledger.entry.read")
	start := time.Now()
	seedBudgetGroup(t, e, true)
	t.Logf("seeded a group of 100 companies with 1,000 accounts and 1,000 trading partner balances each on 5,000 group accounts in %v", time.Since(start))
	ctx, cancel := context.WithTimeout(context.Background(), budgetRunLimit)
	defer cancel()
	c := &workflows.Consolidations{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Runs: app.Consolidation(app.Config{}),
		Actors: identity.Actors{}}
	s := &conScenario{e: e, c: c}
	in := s.start(t)
	start = time.Now()
	if err := (&workflows.Consolidator{Consolidations: c, Log: slog.New(slog.DiscardHandler)}).Consolidate(ctx, in); err != nil {
		t.Fatalf("the run, after %v: %v", time.Since(start), err)
	}
	t.Logf("consolidation run of 100 companies on 5,000 group accounts: %v", time.Since(start))
	base := "/v1/consolidation-runs/" + in.RunID.String() + "/statements/"
	for _, statement := range []string{"balance_sheet", "profit_and_loss"} {
		pages := statementPages(t, e, base+statement+"?page_size=500")
		for name, query := range map[string]string{"first page of 50": "", "first page of 500": "?page_size=500",
			"a middle page of 500": "?page_size=500&page_token=" + pages[len(pages)/2], "the last page of 500": "?page_size=500&page_token=" + pages[len(pages)-1]} {
			var times []time.Duration
			for range 10 {
				start := time.Now()
				page, _ := e.Must(200, "GET", base+statement+query, "", "")
				times = append(times, time.Since(start))
				if len(page["items"].([]any)) == 0 {
					t.Fatalf("%s, %s: got no rows", statement, name)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("%s, %s, of a run of 100 companies on 5,000 group accounts: median %v, p95 %v", statement, name, times[len(times)/2], p95)
			if budget := nfr.Budget(time.Second); p95 > budget {
				t.Errorf("%s, %s: p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", statement, name, p95, budget)
			}
		}
	}
	bs, _ := e.Must(200, "GET", base+"balance_sheet", "", "")
	var sum int64
	for _, k := range []string{"assets", "liabilities", "equity", "profit"} {
		a := bs["totals"].(map[string]any)[k].(map[string]any)["consolidated"].(map[string]any)["amount"].(string)
		cents, err := strconv.ParseInt(strings.Replace(a, ".", "", 1), 10, 64)
		if err != nil {
			t.Fatalf("the balance sheet's %s: %v", k, err)
		}
		sum += cents
	}
	if sum != 0 {
		t.Errorf("the balance sheet's totals and the profit: got a sum of %d cents, want zero", sum)
	}
}

// statementPages are the page tokens of a statement's pages after its
// first, in order.
func statementPages(t *testing.T, e fixture.Env, path string) []string {
	t.Helper()
	var tokens []string
	page, _ := e.Must(200, "GET", path, "", "")
	for page["next_page_token"] != nil {
		tok := page["next_page_token"].(string)
		tokens = append(tokens, tok)
		page, _ = e.Must(200, "GET", path+"&page_token="+tok, "", "")
	}
	return tokens
}
