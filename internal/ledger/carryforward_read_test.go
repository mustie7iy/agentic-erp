package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// setPeriod sets one period of a company's year, in every ledger, to the
// status, as the period lock's transitions leave it.
func (s cfEnv) setPeriod(tb testing.TB, le uuid.UUID, year, period int, status string) {
	tb.Helper()
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = $5, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = $3 AND period = $4`, s.ten.ID, le, year, period, status)
}

// get reads a run as GET shows it.
func (s cfEnv) get(tb testing.TB, run any) map[string]any {
	tb.Helper()
	got, _ := s.Must(200, "GET", fmt.Sprint("/v1/balance-carry-forwards/", run), "", "")
	return got
}

// staleness is a run's stale flag and the periods opened since, as
// "true 2025/13 0L".
func staleness(tb testing.TB, run map[string]any) string {
	tb.Helper()
	out := fmt.Sprint(run["stale"])
	for _, p := range apitest.List(tb, run, "opened_since") {
		m := p.(map[string]any)
		out += fmt.Sprintf(" %v/%v %v", m["fiscal_year"], m["period"], m["ledger_code"])
	}
	return out
}

// TestCarryForwardStaleOnRead: a run is stale on every read once a period
// of its year or an earlier one is opened, open or closed again since,
// naming the period, and a later year's opening leaves it current
// (ADR-0060 §6); carried forward again, the new run supersedes it and is
// not stale, nor is the next year's run chained from it.
func TestCarryForwardStaleOnRead(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	y25 := s.run(t, s.us, 2025)[0]
	y26 := s.run(t, s.us, 2026)[0]
	if got := staleness(t, y26); got != "false" {
		t.Errorf("2026 as run: got %s, want false", got)
	}
	s.setPeriod(t, s.us, 2027, 1, ledger.Open)
	if got := staleness(t, s.get(t, y26["id"])); got != "false" {
		t.Errorf("2026 with 2027/1 open: got %s, want false: a later year is not carried", got)
	}
	s.setPeriod(t, s.us, 2025, 13, ledger.Open)
	for name, want := range map[string]string{"open": "true 2025/13 0L", "closed again": "true 2025/13 0L"} {
		if name == "closed again" {
			s.setPeriod(t, s.us, 2025, 13, ledger.Closed)
		}
		for year, run := range map[int]map[string]any{2025: y25, 2026: y26} {
			if got := staleness(t, s.get(t, run["id"])); got != want {
				t.Errorf("%d with 2025/13 %s: got %s, want %s", year, name, got, want)
			}
		}
	}
	page, _ := s.Must(200, "GET", fmt.Sprintf("/v1/balance-carry-forwards?legal_entity_id=%s&fiscal_year=2026", s.us), "", "")
	if got := staleness(t, apitest.List(t, page, "items")[0].(map[string]any)); got != "true 2025/13 0L" {
		t.Errorf("2026 listed: got %s, want true 2025/13 0L", got)
	}
	again := s.run(t, s.us, 2025)[0]
	if got := staleness(t, again); got != "false" {
		t.Errorf("2025 carried forward again: got %s, want false", got)
	}
	if old := s.get(t, y25["id"]); old["status"] != "superseded" || old["superseded_by_id"] != again["id"] {
		t.Errorf("the first 2025 run: got %v by %v, want superseded by %v", old["status"], old["superseded_by_id"], again["id"])
	}
	next := s.run(t, s.us, 2026)[0]
	if next["chained_from_id"] != again["id"] || staleness(t, next) != "false" {
		t.Errorf("2026 again: got chained from %v, %s, want from %v and not stale", next["chained_from_id"], staleness(t, next), again["id"])
	}
}

// TestListBalanceCarryForwards: the runs listed, filtered by company,
// year, ledger and status, paged in either order, and only those of the
// companies the caller may act in.
func TestListBalanceCarryForwards(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	s.run(t, s.de, 2025)
	s.run(t, s.de, 2026)
	first := s.run(t, s.us, 2026)[0]
	s.run(t, s.us, 2026)
	list := func(c apitest.Client, query string) []map[string]any {
		var out []map[string]any
		for path := "/v1/balance-carry-forwards?" + query; ; {
			page, _ := c.Must(200, "GET", path, "", "")
			for _, it := range apitest.List(t, page, "items") {
				out = append(out, it.(map[string]any))
			}
			next, ok := page["next_page_token"].(string)
			if !ok {
				return out
			}
			path = "/v1/balance-carry-forwards?" + query + "&page_token=" + url.QueryEscape(next)
		}
	}
	key := func(runs []map[string]any) string {
		var keys []string
		for _, r := range runs {
			keys = append(keys, fmt.Sprintf("%v %v %v %v", r["company_code"], r["from_fiscal_year"], r["ledger_code"], r["status"]))
		}
		return strings.Join(keys, ", ")
	}
	for _, c := range []struct{ name, query, want string }{
		{"a company's", "legal_entity_id=" + s.de.String(), "2000 2025 0L current, 2000 2025 2L current, 2000 2026 0L current, 2000 2026 2L current"},
		{"a year's", fmt.Sprintf("legal_entity_id=%s&fiscal_year=2025", s.de), "2000 2025 0L current, 2000 2025 2L current"},
		{"a ledger's", "ledger_id=" + s.local, "2000 2025 2L current, 2000 2026 2L current"},
		{"the superseded", "status=superseded", "1000 2026 0L superseded"},
		{"a year's, a page at a time", fmt.Sprintf("legal_entity_id=%s&fiscal_year=2026&page_size=1", s.de), "2000 2026 0L current, 2000 2026 2L current"},
		{"by year, newest first, a page at a time", "order_by=fiscal_year%20desc&page_size=2&status=current",
			"1000 2026 0L current, 2000 2026 2L current, 2000 2026 0L current, 2000 2025 2L current, 2000 2025 0L current"}, // ties by id, descending too
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := key(list(s.In(t), c.query)); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("a run listed as GET shows it", func(t *testing.T) {
		listed := list(s.In(t), "status=superseded")[0]
		got, _ := json.Marshal(listed)
		want, _ := json.Marshal(s.get(t, first["id"]))
		if string(got) != string(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("the companies the caller may act in", func(t *testing.T) {
		if got := key(list(s.actingIn(t, s.us).In(t), "")); got != "1000 2026 0L superseded, 1000 2026 0L current" {
			t.Errorf("got %s, want 1000's runs alone", got)
		}
	})
	t.Run("an order not offered", func(t *testing.T) {
		s.In(t).Problem(400, "request.invalid_order_by", "GET", "/v1/balance-carry-forwards?order_by=status", "", "")
	})
}

// minor is an API amount in minor units ("-3000.00" is -300000).
func minor(tb testing.TB, m any) int64 {
	tb.Helper()
	n, err := strconv.ParseInt(strings.Replace(m.(map[string]any)["amount"].(string), ".", "", 1), 10, 64)
	if err != nil {
		tb.Fatalf("amount %v: %v", m, err)
	}
	return n
}

// TestCarryForwardBalances: a run's rows paged by account number,
// currency and kind, each with its account's number, name and type and
// its amounts, as stored, and the whole run's totals on every page,
// whatever the filter (ADR-0060 §7).
func TestCarryForwardBalances(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	run := s.run(t, s.de, 2026)[0]
	path := fmt.Sprintf("/v1/balance-carry-forwards/%v/balances", run["id"])
	totals := func(page map[string]any) string {
		b, _ := json.Marshal([]any{page["totals"], page["result_lc"], page["result_gc"], page["debit_lc"], page["credit_lc"], page["debit_gc"], page["credit_gc"]})
		return string(b)
	}
	b, _ := json.Marshal([]any{run["totals"], run["result_lc"], run["result_gc"], run["debit_lc"], run["credit_lc"], run["debit_gc"], run["credit_gc"]})
	wantTotals := string(b)
	var rows []string
	pages := 0
	for at := path + "?page_size=2"; ; pages++ {
		page, _ := s.Must(200, "GET", at, "", "")
		if got := totals(page); got != wantTotals {
			t.Errorf("page %d's totals: got %s, want the run's %s", pages, got, wantTotals)
		}
		if page["run_id"] != run["id"] || page["local_currency"] != "EUR" || page["group_currency"] != "EUR" {
			t.Errorf("page %d: got run %v in %v/%v, want %v in EUR/EUR", pages, page["run_id"], page["local_currency"], page["group_currency"], run["id"])
		}
		for _, it := range apitest.List(t, page, "items") {
			r := it.(map[string]any)
			a := apitest.Object(t, r, "account")
			if a["name"] == "" || a["type"] == "" {
				t.Errorf("row %v: got account %v, want its name and type", r, a)
			}
			rows = append(rows, fmt.Sprintf("%v %v %v %d %d %d", a["number"], r["kind"], r["currency"], minor(t, r["amount"]), minor(t, r["amount_lc"]), minor(t, r["amount_gc"])))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		at = path + "?page_size=2&page_token=" + url.QueryEscape(next)
	}
	if want := s.rowsInOrder(t, run["id"]); !slices.Equal(rows, want) {
		t.Errorf("the rows over %d pages: got %v, want %v", pages, rows, want)
	}
	retained := s.account(t, "2000", "330000")
	page, _ := s.Must(200, "GET", path+"?account_id="+retained, "", "")
	var kinds []string
	for _, it := range apitest.List(t, page, "items") {
		r := it.(map[string]any)
		kinds = append(kinds, fmt.Sprintf("%v %v %v %v", apitest.Object(t, r, "account")["number"], apitest.Object(t, r, "account")["type"], r["currency"], r["kind"]))
	}
	if got := strings.Join(kinds, ", "); !strings.Contains(got, "330000 equity EUR result") || strings.Contains(got, "100000") {
		t.Errorf("the retained earnings account's rows: got %s, want its own, a result among them", got)
	}
	if got := totals(page); got != wantTotals {
		t.Errorf("the filtered page's totals: got %s, want the whole run's %s", got, wantTotals)
	}
	for _, c := range []struct {
		name, path, code string
		status           int
	}{
		{"a token of another filter", path + "?account_id=" + retained + "&page_token=" + url.QueryEscape(s.token(t, path+"?page_size=1")), "request.invalid_page_token", 400},
		{"a run that does not exist", fmt.Sprintf("/v1/balance-carry-forwards/%s/balances", uuid.New()), "request.not_found", 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.In(t).Problem(c.status, c.code, "GET", c.path, "", "")
		})
	}
	t.Run("a run of a company the caller cannot act in", func(t *testing.T) {
		s.actingIn(t, s.us).In(t).Problem(404, "request.not_found", "GET", path, "", "")
	})
}

// token is the next page token of a page.
func (s cfEnv) token(tb testing.TB, path string) string {
	tb.Helper()
	page, _ := s.Must(200, "GET", path, "", "")
	next, ok := page["next_page_token"].(string)
	if !ok {
		tb.Fatalf("%s: got no next page, want one", path)
	}
	return next
}

// rowsInOrder are a run's rows as "account kind currency tc lc gc" in the
// API's order: account number byte by byte, currency, kind.
func (s cfEnv) rowsInOrder(tb testing.TB, run any) []string {
	tb.Helper()
	return s.strings(tb, `SELECT a.number || ' ' || b.kind || ' ' || b.currency_tc || ' ' || b.amount_tc || ' ' || b.amount_lc || ' ' || b.amount_gc
		FROM ledger.carry_forward_balance b JOIN ledger.gl_account a ON a.id = b.account_id WHERE b.run_id = $1
		ORDER BY a.number COLLATE "C", b.currency_tc COLLATE "C", b.kind COLLATE "C"`, run)
}

// TestCarryForwardReadFailures: the list and the balances fail with each
// statement they make.
func TestCarryForwardReadFailures(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	run := s.run(t, s.de, 2026)[0]
	h := endpoints()
	id := uuid.MustParse(run["id"].(string))
	for name, call := range map[string]func(ctx context.Context) error{
		"list": func(ctx context.Context) error {
			_, err := h.ListBalanceCarryForwards(ctx, gen.ListBalanceCarryForwardsParams{})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := h.ListBalanceCarryForwardBalances(ctx, gen.ListBalanceCarryForwardBalancesParams{ID: id})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, s.ten, s.ten.Human, name, call)
		})
	}
	// The contract bounds the page size; the handler refuses one too.
	t.Run("a page size out of range", func(t *testing.T) {
		err := apitest.InRequest(t, s.ten, s.ten.Human, func(ctx context.Context) error {
			_, err := h.ListBalanceCarryForwardBalances(ctx, gen.ListBalanceCarryForwardBalancesParams{ID: id, PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := (*problem.Problem)(nil); !errors.As(err, &p) || p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", err)
		}
	})
}
