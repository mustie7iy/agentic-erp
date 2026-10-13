package ledger_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// project runs the projector until nothing is pending in any tenant.
func project(t testing.TB) {
	t.Helper()
	p := &ledger.Projector{Pool: pgtest.Pool(t), Role: "erp_projector"}
	for {
		n, err := p.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// trialBalance lists the balances of a query as "account period debit/credit lc".
func (e env) trialBalance(t testing.TB, q string) []string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/balances?"+q, "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		b := it.(map[string]any)
		out = append(out, fmt.Sprintf("%s/%v %s/%s %s", b["account_id"].(string)[:8], b["fiscal_period"], b["debit_lc"].(map[string]any)["amount"],
			b["credit_lc"].(map[string]any)["amount"], b["currency"]))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

func TestBalancesAndProjector(t *testing.T) {
	e := setup(t)
	project(t)
	bank, revenue, expense := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "520000")
	e.rate(t, "USD", "EUR", "2026-01-01", "0.5")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "100.00", ""}, line{revenue, "-100.00", ""}), "")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{expense, "30.00", ""}, line{bank, "-30.00", ""}), "")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-04-02", "USD", line{expense, "10.00", ""}, line{bank, "-10.00", ""}), "")
	q := "legal_entity_id=" + e.de.String() + "&fiscal_year=2026"
	before := e.trialBalance(t, q+"&page_size=500")
	if len(before) != 5 {
		t.Fatalf("balances before projection: %v", before)
	}
	var pending int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM ledger.balance_projection WHERE tenant_id = $1 AND projected_at IS NULL`, e.ten.ID).Scan(&pending); err != nil {
		t.Fatalf("count pending projections: %v", err)
	}
	if pending != 3 {
		t.Fatalf("pending: %d", pending)
	}
	project(t)
	after := e.trialBalance(t, q+"&page_size=500")
	if strings.Join(before, "|") != strings.Join(after, "|") {
		t.Fatalf("projection changed the figures:\n%v\n%v", before, after)
	}
	// Projected and pending amounts add up in one row.
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	mixed := e.trialBalance(t, q+"&fiscal_period=3&account_id="+bank)
	if len(mixed) != 1 || !strings.HasSuffix(mixed[0], "101.00/30.00 EUR") {
		t.Fatalf("bank in March: %v", mixed)
	}
	var debit, credit int64
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT debit_lc, credit_lc FROM ledger.period_balance WHERE account_id = $1 AND fiscal_period = 3`, bank).Scan(&debit, &credit); err != nil {
		t.Fatalf("read projected period balance: %v", err)
	}
	if debit != 10000 || credit != 3000 {
		t.Fatalf("projected row: %d/%d", debit, credit)
	}
	usd := e.trialBalance(t, q+"&fiscal_period=4&account_id="+bank)
	if len(usd) != 1 || !strings.HasSuffix(usd[0], "0.00/5.00 USD") {
		t.Fatalf("converted balance: %v", usd)
	}
	byAccount := e.trialBalance(t, q+"&page_size=4")
	if len(byAccount) != 5 || !strings.HasPrefix(byAccount[4], "next:") {
		t.Fatalf("by account: %v", byAccount)
	}
	if rest := e.trialBalance(t, q+"&page_size=4&page_token="+strings.TrimPrefix(byAccount[4], "next:")); len(rest) != 1 {
		t.Fatalf("by account, page 2: %v", rest)
	}
	byPeriod := e.trialBalance(t, q+"&order_by=fiscal_period%20desc&page_size=2")
	if len(byPeriod) != 3 || !strings.Contains(byPeriod[0], "/4 ") || !strings.HasPrefix(byPeriod[2], "next:") {
		t.Fatalf("by period: %v", byPeriod)
	}
	if rest := e.trialBalance(t, q+"&order_by=fiscal_period%20desc&page_size=2&page_token="+strings.TrimPrefix(byPeriod[2], "next:")); len(rest) != 3 {
		t.Fatalf("page 2: %v", rest)
	}
	for name, query := range map[string]string{
		"unknown ledger": q + "&ledger_id=" + uuid.NewString(),
		"unknown entity": "legal_entity_id=" + uuid.NewString() + "&fiscal_year=2026",
		"other year":     "legal_entity_id=" + e.de.String() + "&fiscal_year=2025",
	} {
		if got := e.trialBalance(t, query); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	agent := e.Client
	agent.As = e.ten.Agent
	if page, _ := agent.Must(200, "GET", "/v1/balances?"+q, "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("agent sees balances: %v", page)
	}
	e.Problem(400, "request.invalid_parameter", "GET", "/v1/balances?legal_entity_id="+e.de.String(), "", "")
}

func TestProjectorRuns(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	for range 3 {
		e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	if _, err := (&ledger.Projector{Pool: pgtest.Pool(t), Role: "no_such_role"}).RunOnce(context.Background()); err == nil {
		t.Fatal("projected with an unknown role")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&ledger.Projector{Pool: pgtest.Pool(t), Role: "no_such_role", Interval: time.Millisecond, Log: log}).Run(ctx)
		close(done)
	}()
	// The failure is logged within an interval or two; under a loaded
	// race-detector run that can take longer than a fixed sleep.
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), "balance projection failed"); {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("logs: %s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	// Batches of one: full batches follow each other at once.
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		(&ledger.Projector{Pool: pgtest.Pool(t), Role: "erp_projector", Batch: 1, Log: log}).Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pending int
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM ledger.balance_projection WHERE tenant_id = $1 AND projected_at IS NULL`, e.ten.ID).Scan(&pending); err != nil {
			t.Fatalf("count pending projections: %v", err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("projector did not catch up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	(&ledger.Projector{Pool: pgtest.Pool(t), Role: "erp_projector"}).Run(canceled())
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestTrialBalanceBudget checks 01-requirements' budget for a trial balance
// of one company and period (p95 1 s) at the scale of the largest tenant's
// company: 5,000 accounts with three years of projected balances in two
// currencies and 2,000 lines the projector has not reached yet.
func TestTrialBalanceBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setup(t)
	project(t)
	var chart uuid.UUID
	admin := pgtest.Admin(t)
	if err := admin.QueryRow(context.Background(), `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, e.ten.ID).Scan(&chart); err != nil {
		t.Fatalf("read chart 2000: %v", err)
	}
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'P' || n, 'Account ' || n, 'expense', 'active', $3, $3 FROM generate_series(1, 5000) n`, e.ten.ID, chart, e.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number LIKE 'P%'`, e.ten.ID, e.de, e.ten.Human.ID, chart)
	pgtest.Exec(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
			debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, $2, a.id, y, p, c, 1000, 10, 1000, 10, 1000, 10
		FROM ledger.gl_account a, ledger.ledger l, generate_series(2024, 2026) y, generate_series(1, 12) p, unnest(ARRAY['EUR', 'USD']) c
		WHERE a.chart_id = $3 AND a.number LIKE 'P%' AND l.tenant_id = $1`, e.ten.ID, e.de, chart)
	var accounts []string
	rows, _ := admin.Query(context.Background(), `SELECT id::text FROM ledger.gl_account WHERE chart_id = $1 AND number LIKE 'P%' LIMIT 20`, chart)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan account: %v", err)
		}
		accounts = append(accounts, id)
	}
	pgtest.Exec(t, `ANALYZE ledger.period_balance`)
	lines := make([]line, 0, 20)
	for i, a := range accounts {
		amount := "1.00"
		if i%2 == 1 {
			amount = "-1.00"
		}
		lines = append(lines, line{a, amount, ""})
	}
	for range 100 {
		e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-06-15", "EUR", lines...), "")
	}

	pgtest.Exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.balance_projection`)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		rows, token := 0, ""
		for {
			q := "legal_entity_id=" + e.de.String() + "&fiscal_year=2026&fiscal_period=6&page_size=500"
			if token != "" {
				q += "&page_token=" + token
			}
			page, _ := e.Must(200, "GET", "/v1/balances?"+q, "", "")
			rows += len(page["items"].([]any))
			next, ok := page["next_page_token"].(string)
			if !ok {
				break
			}
			token = next
		}
		times = append(times, time.Since(start))
		if rows != 10000 {
			t.Fatalf("trial balance rows: %d", rows)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p95 := times[len(times)*95/100]
	t.Logf("trial balance of 10,000 rows: median %v, p95 %v", times[len(times)/2], p95)
	if p95 > time.Second {
		t.Fatalf("trial balance p95 %v exceeds the 1 s budget", p95)
	}
}

// lockedBuffer is a log sink the test may read while the projector writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
