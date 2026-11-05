package consolidation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// accounts reads a page of group accounts as one line per account: its
// number, name and type, its role and elimination, and its mapped
// accounts.
func accounts(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		a := it.(map[string]any)
		el := "-"
		if e, ok := a["elimination"].(map[string]any); ok {
			el = fmt.Sprintf("%s %s", e["code"], e["name"])
		}
		role, _ := a["role"].(string)
		out = append(out, fmt.Sprintf("%s %s %s role=%s elimination=%s mapped=%v", a["number"], a["name"], a["type"], role, el, a["mapped_accounts"]))
	}
	return out
}

// The group chart of fixture.Consolidation as every company of withGroups
// maps to it: 1000's ten accounts; 2000's eleven and its investments
// 150000 and 151000; Maple's (3000) copy of 2000's eleven, taken before
// the investments were added, with its three USD bank accounts and two
// exchange accounts; and 4000's and 5000's copies of 2000's thirteen.
const (
	g1000 = "G1000 Other assets asset role= elimination=- mapped=17"
	g1250 = "G1250 Intercompany receivables asset role= elimination=IC_BALANCES Intercompany receivables and payables mapped=0"
	g1500 = "G1500 Investments in subsidiaries asset role= elimination=- mapped=6"
	g1900 = "G1900 Goodwill asset role=goodwill elimination=- mapped=0"
	g2000 = "G2000 Liabilities liability role= elimination=- mapped=5"
	g2250 = "G2250 Intercompany payables liability role= elimination=IC_BALANCES Intercompany receivables and payables mapped=0"
	g3000 = "G3000 Share capital equity role= elimination=- mapped=5"
	g3800 = "G3800 Retained earnings equity role=retained_earnings elimination=- mapped=0"
	g3900 = "G3900 Currency translation reserve equity role=cta elimination=- mapped=0"
	g3950 = "G3950 Non-controlling interests equity role=nci elimination=- mapped=0"
	g4000 = "G4000 Revenue revenue role= elimination=- mapped=6"
	g5000 = "G5000 Expenses expense role= elimination=- mapped=26"
	g6950 = "G6950 Intercompany differences expense role= elimination=- mapped=0"
)

// TestListGroupAccounts: the group chart in number order with each
// account's role, elimination and mapped company accounts, counted in the
// companies the caller may act in; filtered by type, role and elimination;
// paged by number, a token kept to its filters.
func TestListGroupAccounts(t *testing.T) {
	e := groupEnv(t)
	page, _ := e.Must(200, "GET", "/v1/group-accounts", "", "")
	all := []string{g1000, g1250, g1500, g1900, g2000, g2250, g3000, g3800, g3900, g3950, g4000, g5000, g6950}
	want(t, "the group chart", accounts(page), all...)
	cases := []struct {
		name, query string
		want        []string
	}{
		{"equity", "type=equity", []string{g3000, g3800, g3900, g3950}},
		{"the translation reserve", "role=cta", []string{g3900}},
		{"an elimination's", "elimination=IC_BALANCES", []string{g1250, g2250}},
		{"an elimination's assets", "elimination=IC_BALANCES&type=asset", []string{g1250}},
		{"an unknown elimination's", "elimination=NONE", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/group-accounts?"+tc.query, "", "")
			want(t, tc.query, accounts(page), tc.want...)
		})
	}
	var paged []string
	path := "/v1/group-accounts?type=asset&page_size=2"
	for range 3 {
		page, _ = e.Must(200, "GET", path, "", "")
		paged = append(paged, accounts(page)...)
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/group-accounts?type=asset&page_size=2&page_token=" + next
	}
	want(t, "assets two at a time", paged, g1000, g1250, g1500, g1900)
	token := strings.TrimPrefix(path, "/v1/group-accounts?type=asset&page_size=2&page_token=")
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/group-accounts?type=equity&page_token="+token, "", "")

	reader := e.As(e.Reader(t, "reader", e.US))
	page, _ = reader.Must(200, "GET", "/v1/group-accounts?type=asset", "", "")
	want(t, "assets as a reader of company 1000", accounts(page), strings.Replace(g1000, "mapped=17", "mapped=2", 1),
		g1250, strings.Replace(g1500, "mapped=6", "mapped=0", 1), g1900)
}

// section is the consolidation context's section of the configuration as
// the API wires it.
func section(t *testing.T) consolidation.Setup {
	t.Helper()
	for _, s := range app.Configuration(app.Config{}).Sections {
		if s, ok := s.(consolidation.Setup); ok {
			return s
		}
	}
	t.Fatal("the API wires no consolidation section")
	return consolidation.Setup{}
}

// TestSetup: the section refuses a group whose parent is not in the
// tenant's group currency (EUR) and a tolerance with more decimals than
// the group currency has, leaves a malformed tolerance to validation, and
// keeps nothing.
func TestSetup(t *testing.T) {
	e := groupEnv(t)
	setup := section(t)
	check := func(t *testing.T, mutate func(*config.Config)) []string {
		t.Helper()
		doc := fixture.Document(t)
		withGroups(doc)
		mutate(doc)
		var got []string
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			fs, err := setup.Check(ctx, nil, doc)
			for _, f := range fs {
				got = append(got, f.Path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   []string
	}{
		{"as configured", func(*config.Config) {}, nil},
		{"a parent in CAD", func(c *config.Config) { c.Consolidation.Groups[1].Parent = "3000" }, []string{"consolidation.groups[EURO].parent"}},
		{"a tolerance in tenths of a cent", func(c *config.Config) { c.Consolidation.Eliminations[0].Tolerance = "10.001" },
			[]string{"consolidation.eliminations[IC_BALANCES].tolerance"}},
		{"a malformed tolerance", func(c *config.Config) { c.Consolidation.Eliminations[0].Tolerance = "ten" }, nil},
		{"groups without eliminations", func(c *config.Config) { c.Consolidation.Eliminations = nil }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := check(t, tc.mutate); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("findings: got %v, want %v", got, tc.want)
			}
		})
	}
	// Without groups or eliminations nothing is read: no transaction needed.
	if fs, err := setup.Check(context.Background(), nil, fixture.Document(t)); fs != nil || err != nil {
		t.Errorf("a document without group reporting: got %v %v, want nothing", fs, err)
	}
	doc := fixture.Document(t)
	withGroups(doc)
	if _, err := setup.Check(context.Background(), nil, doc); err == nil {
		t.Errorf("a check outside a request: got nil, want an error")
	}
	if err := setup.Apply(context.Background(), fixture.Document(t)); err != nil {
		t.Errorf("apply: got %v, want nil", err)
	}
}
