package ledger_test

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Actual reports by account assignment (T56f, ADR-0050 §7).

// reported is setupAssigned's tenant with four entries in company 2000,
// all in both ledgers: in May, 100.00 on office supplies (520000, the
// default cost center 1000: PC1, SEG1) against revenue (400000, the
// default profit center DUMMY); in June, 30.00 on office supplies against
// the bank, 50.00 of revenue on PC1 against the bank with PC1 entered (a
// balance sheet line, which no report shows), and 5.00 on office supplies
// against the bank that the projector has not reached.
type reported struct {
	assigned
	ccRoot uuid.UUID // the standard cost center hierarchy's root, above its leaf
	local  uuid.UUID // ledger 2L
}

func setupReported(t *testing.T) reported {
	t.Helper()
	r := reported{assigned: setupAssigned(t)}
	for _, p := range []struct {
		day   string
		lines []ledger.PostingLine
	}{
		{"2026-05-10", []ledger.PostingLine{centered(r.office, 10000, nil, nil, nil), centered(r.revenue, -10000, nil, nil, nil)}},
		{"2026-06-15", []ledger.PostingLine{centered(r.office, 3000, nil, nil, nil), centered(r.bank, -3000, nil, nil, nil)}},
		{"2026-06-15", []ledger.PostingLine{centered(r.revenue, -5000, nil, &r.pc1, nil), centered(r.bank, 5000, nil, &r.pc1, nil)}},
	} {
		if _, err := r.post(t, p.day, p.lines...); err != nil {
			t.Fatalf("post on %s: %v", p.day, err)
		}
	}
	project(t)
	if _, err := r.post(t, "2026-06-20", centered(r.office, 500, nil, nil, nil), centered(r.bank, -500, nil, nil, nil)); err != nil {
		t.Fatalf("post the pending entry: %v", err)
	}
	h, _ := r.Must(200, "GET", "/v1/hierarchies/"+r.ccTree.String(), "", "")
	r.ccRoot = uuid.MustParse(h["root_node_id"].(string))
	pgtest.Scan(t, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND code = '2L'`, []any{r.ten.ID}, &r.local)
	return r
}

func (r reported) in(t *testing.T) reported {
	t.Helper()
	r.assigned = r.assigned.in(t)
	return r
}

// centerRows lists a report's rows as "center/account/period debit/credit"
// in local currency with the transaction currency, the center and account
// as their codes, and the segment's code after a profit center's; and the
// next page's token as "next:" last.
func (r reported) centerRows(t *testing.T, path, query string) []string {
	t.Helper()
	page, _ := r.Must(200, "GET", path+"?legal_entity_id="+r.de.String()+"&fiscal_year=2026"+query, "", "")
	accounts := map[string]string{r.office.String(): "520000", r.revenue.String(): "400000", r.bank.String(): "100000"}
	var out []string
	for _, it := range page["items"].([]any) {
		b := it.(map[string]any)
		center, _ := b["cost_center_code"].(string)
		if pc, ok := b["profit_center_code"].(string); ok {
			center = pc
			if seg, ok := b["segment_code"].(string); ok {
				center += "(" + seg + ")"
			}
		}
		out = append(out, fmt.Sprintf("%s/%s/%v %s/%s %s", center, accounts[b["account_id"].(string)], b["fiscal_period"],
			b["debit_lc"].(map[string]any)["amount"], b["credit_lc"].(map[string]any)["amount"], b["currency"]))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

func sorted(rows []string) []string {
	rows = slices.Clone(rows)
	slices.Sort(rows)
	return rows
}

func TestCostCenterBalances(t *testing.T) {
	r := setupReported(t)
	all := []string{"1000/520000/5 100.00/0.00 EUR", "1000/520000/6 35.00/0.00 EUR"}
	for name, c := range map[string]struct {
		query string
		want  []string
	}{
		"every cost center":                   {"", all},
		"the last periods":                    {"&fiscal_period_from=6", all[1:]},
		"the first periods":                   {"&fiscal_period_to=5", all[:1]},
		"the cost center":                     {"&cost_center_id=" + r.cc1000.String(), all},
		"another cost center":                 {"&cost_center_id=" + r.cc2000.String(), nil},
		"the standard hierarchy's root":       {"&hierarchy_node_id=" + r.ccRoot.String(), all},
		"the root and the cost center":        {"&hierarchy_node_id=" + r.ccRoot.String() + "&cost_center_id=" + r.cc1000.String(), all},
		"the root and a cost center not used": {"&hierarchy_node_id=" + r.ccRoot.String() + "&cost_center_id=" + r.cc2000.String(), nil},
		"the root and a center not in it":     {"&hierarchy_node_id=" + r.ccRoot.String() + "&cost_center_id=" + uuid.NewString(), nil},
		"a node of a profit center hierarchy": {"&hierarchy_node_id=" + r.pcLeaf.String(), nil},
		"an unknown node":                     {"&hierarchy_node_id=" + uuid.NewString(), nil},
		"the cost center's segment":           {"&segment_id=" + r.seg1.String(), all},
		"another segment":                     {"&segment_id=" + r.seg2.String(), nil},
		"the account":                         {"&account_id=" + r.office.String(), all},
		"an account without cost centers":     {"&account_id=" + r.revenue.String(), nil},
		"the parallel ledger":                 {"&ledger_id=" + r.local.String(), all},
		"an unknown ledger":                   {"&ledger_id=" + uuid.NewString(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sorted(r.in(t).centerRows(t, "/v1/cost-center-balances", c.query)); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("another legal entity or year", func(t *testing.T) {
		for _, q := range []string{"legal_entity_id=" + uuid.NewString() + "&fiscal_year=2026", "legal_entity_id=" + r.us.String() + "&fiscal_year=2026",
			"legal_entity_id=" + r.de.String() + "&fiscal_year=2025"} {
			if page, _ := r.Must(200, "GET", "/v1/cost-center-balances?"+q, "", ""); len(page["items"].([]any)) != 0 {
				t.Errorf("%s: got %v, want no balances", q, page["items"])
			}
		}
	})
	t.Run("a period range ending before it starts", func(t *testing.T) {
		r.in(t).Problem(400, "request.invalid_parameter", "GET", "/v1/cost-center-balances?legal_entity_id="+r.de.String()+
			"&fiscal_year=2026&fiscal_period_from=7&fiscal_period_to=6", "", "")
	})
}

func TestProfitCenterBalances(t *testing.T) {
	r := setupReported(t)
	// The bank line with PC1 entered is a balance sheet line: not shown.
	all := []string{"DUMMY/400000/5 0.00/100.00 EUR", "PC1(SEG1)/400000/6 0.00/50.00 EUR", "PC1(SEG1)/520000/5 100.00/0.00 EUR",
		"PC1(SEG1)/520000/6 35.00/0.00 EUR"}
	for name, c := range map[string]struct {
		query string
		want  []string
	}{
		"every profit center":                {"", all},
		"the profit center":                  {"&profit_center_id=" + r.pc1.String(), all[1:]},
		"the standard hierarchy's leaf":      {"&hierarchy_node_id=" + r.pcLeaf.String(), all},
		"a node of a cost center hierarchy":  {"&hierarchy_node_id=" + r.ccRoot.String(), nil},
		"the segment":                        {"&segment_id=" + r.seg1.String(), all[1:]},
		"the revenue account in June":        {"&account_id=" + r.revenue.String() + "&fiscal_period_from=6&fiscal_period_to=6", all[1:2]},
		"a balance sheet account":            {"&account_id=" + r.bank.String(), nil},
		"the parallel ledger's":              {"&ledger_id=" + r.local.String(), all},
		"the default profit center's period": {"&profit_center_id=" + r.dummy.String() + "&fiscal_period_to=5", all[:1]},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sorted(r.in(t).centerRows(t, "/v1/profit-center-balances", c.query)); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestCenterBalancesExactAndPaged: the reports are the same before and
// after the projector runs, a row adds projected and pending lines, every
// order pages, and an agent of the tenant without company access sees
// nothing.
func TestCenterBalancesExactAndPaged(t *testing.T) {
	r := setupReported(t)
	before := sorted(r.centerRows(t, "/v1/profit-center-balances", ""))
	project(t)
	if after := sorted(r.centerRows(t, "/v1/profit-center-balances", "")); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("projection changed the figures: got %v, want %v", after, before)
	}
	// The pending 5.00 in June and the projected 30.00 are one row.
	if _, err := r.post(t, "2026-06-25", centered(r.office, 100, nil, nil, nil), centered(r.bank, -100, nil, nil, nil)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got, want := r.centerRows(t, "/v1/cost-center-balances", "&fiscal_period_from=6"), []string{"1000/520000/6 36.00/0.00 EUR"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("projected and pending: got %v, want %v", got, want)
	}
	for _, order := range []string{"", "profit_center_id desc", "account_id", "fiscal_period desc"} {
		t.Run("by "+order, func(t *testing.T) {
			var rows []string
			token := ""
			for range 5 {
				q := "&page_size=1"
				if order != "" {
					q += "&order_by=" + url.QueryEscape(order)
				}
				if token != "" {
					q += "&page_token=" + token
				}
				page := r.in(t).centerRows(t, "/v1/profit-center-balances", q)
				last := page[len(page)-1]
				if !strings.HasPrefix(last, "next:") {
					rows = append(rows, page...)
					break
				}
				rows, token = append(rows, page[:len(page)-1]...), strings.TrimPrefix(last, "next:")
			}
			if len(rows) != 4 {
				t.Errorf("rows over every page: got %v, want 4", rows)
			}
		})
	}
	if page := r.centerRows(t, "/v1/cost-center-balances", "&page_size=1"); len(page) != 2 || !strings.HasPrefix(page[1], "next:") {
		t.Errorf("cost centers' first page of one: got %v, want a row and a next page token", page)
	}
	r.Problem(400, "request.invalid_order_by", "GET", "/v1/cost-center-balances?legal_entity_id="+r.de.String()+"&fiscal_year=2026&order_by=code", "", "")
	agent := r.Client
	agent.As = r.ten.Agent
	if page, _ := agent.Must(200, "GET", "/v1/profit-center-balances?legal_entity_id="+r.de.String()+"&fiscal_year=2026", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("agent without company access: got %v, want no balances", page["items"])
	}
}

// TestCenterBalancesDrillDown: the lines behind a cost or profit center's
// rows are the entries the journal list gives for the center (T56d2).
func TestCenterBalancesDrillDown(t *testing.T) {
	r := setupReported(t)
	for name, c := range map[string]struct {
		filter string
		want   int
	}{
		"cost center 1000":  {"cost_center_id=" + r.cc1000.String(), 3},
		"profit center PC1": {"profit_center_id=" + r.pc1.String(), 4},
		"DUMMY":             {"profit_center_id=" + r.dummy.String(), 1},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := r.in(t).Must(200, "GET", "/v1/journal-entries?legal_entity_id="+r.de.String()+"&fiscal_year=2026&"+c.filter, "", "")
			if got := len(page["items"].([]any)); got != c.want {
				t.Errorf("entries: got %d, want %d", got, c.want)
			}
		})
	}
}

// TestAssignmentBalancesAddUp (ADR-0050 §7): with profit center accounting
// on, every profit and loss line carries a profit center, so per ledger,
// company, account and period the assignment balances of the P&L accounts
// add up to period_balance. It reads the projected tables: period_balance
// is kept by the projector, whose code the db package's schema tests
// cannot import.
func TestAssignmentBalancesAddUp(t *testing.T) {
	r := setupReported(t)
	project(t)
	var compared, differing int
	pgtest.Scan(t, `WITH pb AS (
			SELECT b.ledger_id, b.legal_entity_id, b.account_id, b.fiscal_year, b.fiscal_period, b.currency_tc, b.debit_tc, b.credit_tc, b.debit_lc, b.credit_lc,
				b.debit_gc, b.credit_gc
			FROM ledger.period_balance b JOIN ledger.gl_account a ON a.id = b.account_id AND NOT a.is_balance_sheet WHERE b.tenant_id = $1
		), ab AS (
			SELECT b.ledger_id, b.legal_entity_id, b.account_id, b.fiscal_year, b.fiscal_period, b.currency_tc, sum(b.debit_tc) AS debit_tc,
				sum(b.credit_tc) AS credit_tc, sum(b.debit_lc) AS debit_lc, sum(b.credit_lc) AS credit_lc, sum(b.debit_gc) AS debit_gc,
				sum(b.credit_gc) AS credit_gc
			FROM ledger.assignment_balance b JOIN ledger.gl_account a ON a.id = b.account_id AND NOT a.is_balance_sheet
			WHERE b.tenant_id = $1 AND b.profit_center_id IS NOT NULL GROUP BY 1, 2, 3, 4, 5, 6
		)
		SELECT count(*), count(*) FILTER (WHERE (pb.debit_tc, pb.credit_tc, pb.debit_lc, pb.credit_lc, pb.debit_gc, pb.credit_gc)
			IS DISTINCT FROM (ab.debit_tc, ab.credit_tc, ab.debit_lc, ab.credit_lc, ab.debit_gc, ab.credit_gc))
		FROM pb FULL JOIN ab USING (ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc)`, []any{r.ten.ID}, &compared, &differing)
	// Two accounts in May and June, in two ledgers.
	if compared != 8 {
		t.Errorf("P&L balances compared: got %d, want 8", compared)
	}
	if differing != 0 {
		t.Errorf("P&L balances whose assignment balances differ: got %d, want 0", differing)
	}
}

// TestAssignmentProjectionRunsOnce: projectors running side by side and
// again after the queue is drained count each line once, as the lines
// themselves sum.
func TestAssignmentProjectionRunsOnce(t *testing.T) {
	r := setupReported(t)
	for range 3 {
		if _, err := r.post(t, "2026-06-28", centered(r.office, 700, nil, nil, nil), centered(r.revenue, -700, nil, nil, nil)); err != nil {
			t.Fatalf("post: %v", err)
		}
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			p := &ledger.Projector{Pool: pgtest.Pool(t), Role: "erp_projector", Batch: 1}
			for {
				n, err := p.RunOnce(context.Background())
				if err != nil {
					t.Errorf("projector: %v", err)
				}
				if n == 0 || err != nil {
					return
				}
			}
		})
	}
	wg.Wait()
	sums := func() (lines, balances money.Minor) {
		pgtest.Scan(t, `SELECT (SELECT coalesce(sum(abs(amount_lc)), 0) FROM ledger.journal_line WHERE tenant_id = $1
				AND (cost_center_id IS NOT NULL OR profit_center_id IS NOT NULL OR segment_id IS NOT NULL)),
			(SELECT coalesce(sum(debit_lc + credit_lc), 0) FROM ledger.assignment_balance WHERE tenant_id = $1)`, []any{r.ten.ID}, &lines, &balances)
		return lines, balances
	}
	lines, once := sums()
	if once != lines {
		t.Errorf("assignment balances after projectors side by side: got %d, want the lines' %d", once, lines)
	}
	project(t)
	if _, twice := sums(); twice != once {
		t.Errorf("assignment balances after running again: got %d, want %d", twice, once)
	}
}

// TestCenterBalancesDatabaseFailures calls both reports, with a node, with
// each statement failing in turn and outside a unit of work.
func TestCenterBalancesDatabaseFailures(t *testing.T) {
	r := setupReported(t)
	h := endpoints()
	node := gen.NewOptUUID(r.ccRoot)
	for name, call := range map[string]func(ctx context.Context) error{
		"cost center balances": func(ctx context.Context) error {
			_, err := h.ListCostCenterBalances(ctx, gen.ListCostCenterBalancesParams{LegalEntityID: r.de, FiscalYear: 2026, HierarchyNodeID: node})
			return err
		},
		"profit center balances": func(ctx context.Context) error {
			_, err := h.ListProfitCenterBalances(ctx, gen.ListProfitCenterBalancesParams{LegalEntityID: r.de, FiscalYear: 2026,
				HierarchyNodeID: gen.NewOptUUID(r.pcLeaf)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, r.ten, r.ten.Human, name, call)
		})
	}
	t.Run("page sizes", func(t *testing.T) {
		zero := gen.NewOptInt32(0)
		if err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) error {
			_, err := h.ListCostCenterBalances(ctx, gen.ListCostCenterBalancesParams{LegalEntityID: r.de, FiscalYear: 2026, PageSize: zero})
			return err
		}); err == nil {
			t.Error("page size 0: got no error, want the call refused")
		}
	})
}
