package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// parallel is the test document with a local-GAAP ledger 2L kept by
// company 2000, a group of both ledgers and a group of 2L alone.
func parallel(tb testing.TB) *config.Config {
	tb.Helper()
	c := document(tb)
	c.Ledgers = []config.Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "HGB", AccountingPrinciple: "HGB"}}
	c.LedgerGroups = []config.LedgerGroup{{Code: "ALL", Name: "Every ledger", Representative: "0L", Ledgers: []string{"0L", "2L"}},
		{Code: "LOC", Name: "Local", Representative: "2L", Ledgers: []string{"2L"}}}
	c.Companies[1].Ledgers = []string{"0L", "2L"}
	return c
}

// TestParallelLedgers: the configuration's ledgers, company ledgers and
// groups are applied, with periods of their own for a parallel ledger,
// read through the API, and kept once activated (T52a, ADR-0045).
func TestParallelLedgers(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	apply(parallel(t))
	apply(parallel(t)) // applying the same document again changes nothing
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pgtest.Admin(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	periods := `SELECT count(*) FROM ledger.period_status ps JOIN ledger.ledger l ON l.tenant_id = ps.tenant_id AND l.id = ps.ledger_id
		WHERE ps.legal_entity_id = $1 AND l.code = $2 AND ps.fiscal_year = 2026 AND ps.status = $3`
	for _, c := range []struct {
		name string
		sql  string
		args []any
		want int
	}{
		{"2L periods of 2000, closed", periods, []any{e.de, "2L", "closed"}, 16},
		{"2L periods of 1000", `SELECT count(*) FROM ledger.period_status ps JOIN ledger.ledger l ON l.id = ps.ledger_id WHERE ps.legal_entity_id = $1 AND l.code = '2L'`, []any{e.us}, 0},
		{"0L periods of 2000 still open", periods, []any{e.de, "0L", "open"}, 12},
		{"group members", `SELECT count(*) FROM ledger.ledger_group_member WHERE tenant_id = $1`, []any{e.ten.ID}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := count(c.sql, c.args...); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}

	t.Run("read", func(t *testing.T) { readLedgers(t, e.in(t)) })

	// Names change and groups are added; the document is the one later
	// changes are checked against.
	renamed := func() *config.Config {
		c := parallel(t)
		c.Ledgers[1].Name = "Local GAAP"
		c.LedgerGroups[0].Name = "All ledgers"
		c.LedgerGroups = append(c.LedgerGroups, config.LedgerGroup{Code: "NEW", Name: "New", Representative: "0L", Ledgers: []string{"0L"}})
		return c
	}
	apply(renamed())
	if got := count(`SELECT count(*) FROM ledger.ledger WHERE tenant_id = $1 AND name = 'Local GAAP' AND version = 2`, e.ten.ID); got != 1 {
		t.Errorf("renamed ledger: got %d, want 1 at version 2", got)
	}
	if got := count(`SELECT count(*) FROM ledger.ledger_group WHERE tenant_id = $1 AND name = 'All ledgers' AND version = 2`, e.ten.ID); got != 1 {
		t.Errorf("renamed group: got %d, want 1 at version 2", got)
	}
	t.Run("kept once activated", func(t *testing.T) { keptOnceActivated(t, e.in(t), s, renamed) })
}

func readLedgers(t *testing.T, e env) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/ledgers", "", "")
	items := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("ledgers: got %d, want 2", len(items))
	}
	leading, local := items[0].(map[string]any), items[1].(map[string]any)
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"leading code", leading["code"], "0L"}, {"leading", leading["is_leading"], true},
		{"leading companies", len(leading["legal_entity_ids"].([]any)), 2},
		{"local code", local["code"], "2L"}, {"local principle", local["accounting_principle"], "HGB"}, {"local leading", local["is_leading"], false},
		{"local companies", local["legal_entity_ids"].([]any)[0], e.de.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if page, _ := e.Must(200, "GET", "/v1/ledgers?legal_entity_id="+e.us.String(), "", ""); len(page["items"].([]any)) != 1 {
		t.Errorf("ledgers of 1000: got %v, want 0L alone", page["items"])
	}
	first, _ := e.Must(200, "GET", "/v1/ledgers?page_size=1&order_by=code%20desc", "", "")
	tok, _ := first["next_page_token"].(string)
	if tok == "" {
		t.Fatal("page 1: got no next page token, want one")
	}
	if second, _ := e.Must(200, "GET", "/v1/ledgers?page_size=1&order_by=code%20desc&page_token="+tok, "", ""); second["items"].([]any)[0].(map[string]any)["code"] != "0L" {
		t.Errorf("page 2: got %v, want 0L", second["items"])
	}

	groups, _ := e.Must(200, "GET", "/v1/ledger-groups", "", "")
	if got := len(groups["items"].([]any)); got != 2 {
		t.Fatalf("groups: got %d, want 2", got)
	}
	all := groups["items"].([]any)[0].(map[string]any)
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"group code", all["code"], "ALL"}, {"representative", all["representative_ledger_id"], leading["id"]},
		{"members", len(all["ledger_ids"].([]any)), 2},
	} {
		if c.got != c.want {
			t.Errorf("group ALL %s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if page, _ := e.Must(200, "GET", "/v1/ledger-groups?ledger_id="+leading["id"].(string), "", ""); len(page["items"].([]any)) != 1 {
		t.Errorf("groups with 0L: got %v, want ALL alone", page["items"])
	}
	first, _ = e.Must(200, "GET", "/v1/ledger-groups?page_size=1", "", "")
	if tok, _ := first["next_page_token"].(string); tok == "" {
		t.Error("groups page 1: got no next page token, want one")
	}
}

func keptOnceActivated(t *testing.T, e env, s ledger.Setup, activated func() *config.Config) {
	t.Helper()
	check := func(doc *config.Config) string {
		var out []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := s.Check(ctx, nil, doc)
			for _, f := range fs {
				out = append(out, f.Path+": "+f.Message)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "\n")
	}
	if got := check(activated()); got != "" {
		t.Errorf("unchanged document: got %s, want no findings", got)
	}
	for _, c := range []struct {
		name   string
		mutate func(c *config.Config)
		want   string
	}{
		{"ledger removed", func(c *config.Config) {
			c.Ledgers = c.Ledgers[:1]
			c.LedgerGroups = c.LedgerGroups[2:]
			c.Companies[1].Ledgers = nil
		},
			"ledgers[2L]: a ledger cannot be removed once activated"},
		{"principle changed", func(c *config.Config) { c.Ledgers[1].AccountingPrinciple = "US_GAAP" }, "ledgers[2L].accounting_principle: a ledger's accounting principle cannot change"},
		{"company ledger taken", func(c *config.Config) { c.Companies[1].Ledgers = nil }, "companies[2000].ledgers[2L]: a company keeps a ledger once assigned"},
		{"group removed", func(c *config.Config) { c.LedgerGroups = c.LedgerGroups[:1] }, "ledger_groups[LOC]: a ledger group cannot be removed once activated"},
		{"representative changed", func(c *config.Config) {
			c.LedgerGroups[1].Representative = "0L"
			c.LedgerGroups[1].Ledgers = []string{"0L", "2L"}
		},
			"ledger_groups[LOC].representative: a ledger group's representative ledger cannot change"},
		{"member taken", func(c *config.Config) { c.LedgerGroups[0].Ledgers = []string{"0L"} }, "ledger_groups[ALL].ledgers[2L]: a ledger cannot leave a group"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := activated()
			c.mutate(doc)
			if got := check(doc); !strings.Contains(got, c.want) {
				t.Errorf("findings: got\n%s\nwant one containing %q", got, c.want)
			}
		})
	}
}
