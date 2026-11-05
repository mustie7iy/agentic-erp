package consolidation_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// france and italia are companies 4000 and 5000, in EUR: the parent and
// the member of the second group, EURO.
var (
	france = fixture.Company{Code: "4000", Name: "Acme France", Country: "FR", Currency: "EUR"}
	italia = fixture.Company{Code: "5000", Name: "Acme Italia", Country: "IT", Currency: "EUR"}
)

// withGroups is fixture.Consolidation's group ACME, company 2000 holding
// 80% of 1000, to which it adds all of Maple (3000, in CAD) from 2026-01 on
// a new account 151000, named before 1000 so the members are sorted; and
// the group EURO: company 4000 holding 60% of 5000 from 2026-06 on its
// account 150000, both copies of 2000 with house banks of their own.
func withGroups(doc *config.Config) {
	fixture.Maple(doc)
	fixture.Consolidation(doc)
	parent := doc.Company("2000")
	parent.ChartOfAccounts = append(parent.ChartOfAccounts, config.Account{Number: "151000", Name: "Investment in Maple Inc", Type: config.Asset,
		GroupAccount: "G1500"})
	g := &doc.Consolidation.Groups[0]
	g.Members = append([]config.GroupMember{{Company: "3000", OwnershipPercent: "100", FirstPeriod: "2026-01", InvestmentAccount: "151000"}}, g.Members...)
	for _, c := range []struct {
		co        fixture.Company
		iban, bic string
	}{{france, "FR7630006000011234567890189", "BNPAFRPPXXX"},
		{italia, "IT60X0542811101000000123456", "BCITITMMXXX"}} {
		co := *doc.Company("2000")
		co.Code, co.Name, co.Country = c.co.Code, c.co.Name, c.co.Country
		co.ChartOfAccounts = slices.Clone(co.ChartOfAccounts)
		co.Account("150000").Name = "Investment in Acme Italia"
		co.TaxCodes = slices.Clone(co.TaxCodes)
		co.HouseBank = config.HouseBank{Name: "MAIN", IBAN: c.iban, BIC: c.bic}
		doc.Companies = append(doc.Companies, co)
	}
	doc.Consolidation.Groups = append(doc.Consolidation.Groups, config.ConsolidationGroup{Code: "EURO", Name: "Euro group", Parent: "4000",
		Members: []config.GroupMember{{Company: "5000", OwnershipPercent: "60", FirstPeriod: "2026-06", InvestmentAccount: "150000"}}})
}

// groupEnv is a tenant with the groups of withGroups whose people hold the
// consolidation read and the ledger's entry read.
func groupEnv(t *testing.T) fixture.Env {
	t.Helper()
	return fixture.NewWith(t, []fixture.Company{fixture.MapleCompany, france, italia}, withGroups, "consolidation.read", "ledger.entry.read")
}

// groups reads a page of groups as one line per group: its code, name and
// currency, its parent and its members with their ownership, first period
// and investment account.
func groups(t *testing.T, page map[string]any) []string {
	t.Helper()
	var out []string
	for _, it := range page["items"].([]any) {
		out = append(out, groupLine(it.(map[string]any)))
	}
	return out
}

func groupLine(g map[string]any) string {
	company := func(c map[string]any) string {
		return fmt.Sprintf("%s %s %s", c["company_code"], c["company_name"], c["local_currency"])
	}
	var members []string
	for _, m := range g["members"].([]any) {
		m := m.(map[string]any)
		inv := m["investment_account"].(map[string]any)
		members = append(members, fmt.Sprintf("%s at %s%% from %v-%v on %s %s", company(m), m["ownership_percent"], m["first_fiscal_year"],
			m["first_fiscal_period"], inv["number"], inv["name"]))
	}
	return fmt.Sprintf("%s %s in %s: %s holds [%s]", g["code"], g["name"], g["group_currency"], company(g["parent"].(map[string]any)),
		strings.Join(members, "; "))
}

const (
	acme = "ACME Acme group in EUR: 2000 Acme GmbH EUR holds [1000 Acme Corp US USD at 80% from 2026-11 on 150000 Investment in Acme Corp US; " +
		"3000 Maple Inc CAD at 100% from 2026-1 on 151000 Investment in Maple Inc]"
	euro = "EURO Euro group in EUR: 4000 Acme France EUR holds [5000 Acme Italia EUR at 60% from 2026-6 on 150000 Investment in Acme Italia]"
)

func want(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// TestListConsolidationGroups: the groups in code order, each with its
// companies named, their currencies, and each member's ownership, first
// period and investment account; paged by code; a group is listed only to
// a caller who may act in its parent and every member (ADR-0059 §12).
func TestListConsolidationGroups(t *testing.T) {
	e := groupEnv(t)
	page, _ := e.Must(200, "GET", "/v1/consolidation-groups", "", "")
	want(t, "all groups", groups(t, page), acme, euro)
	ids := map[string]string{}
	for _, it := range page["items"].([]any) {
		g := it.(map[string]any)
		ids[g["code"].(string)] = fmt.Sprint(g["parent"].(map[string]any)["legal_entity_id"], " ", g["members"].([]any)[0].(map[string]any)["legal_entity_id"])
	}
	if got, w := ids["ACME"], e.DE.String()+" "+e.US.String(); got != w {
		t.Errorf("ACME's legal entities: got %s, want %s", got, w)
	}

	var paged []string
	path := "/v1/consolidation-groups?page_size=1"
	for range 3 {
		page, _ = e.Must(200, "GET", path, "", "")
		paged = append(paged, groups(t, page)...)
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/consolidation-groups?page_size=1&page_token=" + next
	}
	want(t, "groups one page at a time", paged, acme, euro)
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/consolidation-groups?page_token=x"+strings.Repeat("A", 20), "", "")

	cases := []struct {
		name      string
		companies []string
		want      []string
	}{
		{"a reader of the parent and its members", []string{"1000", "2000", "3000"}, []string{acme}},
		{"a reader of the parent and one of its members", []string{"1000", "2000"}, nil},
		{"a reader of the parent alone", []string{"2000"}, nil},
		{"a reader of the members alone", []string{"1000", "3000"}, nil},
		{"a reader of the other group's companies", []string{"4000", "5000"}, []string{euro}},
		{"a reader of every company", []string{"1000", "2000", "3000", "4000", "5000"}, []string{acme, euro}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := e.As(e.Reader(t, "reader", entities(t, e, tc.companies...)...)).In(t)
			page, _ := reader.Must(200, "GET", "/v1/consolidation-groups", "", "")
			want(t, "groups", groups(t, page), tc.want...)
		})
	}
}

// entities are the legal entities of the company codes.
func entities(t *testing.T, e fixture.Env, codes ...string) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for _, code := range codes {
		var id uuid.UUID
		pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = $2`, []any{e.Tenant.ID, code}, &id)
		out = append(out, id)
	}
	return out
}

// TestGetConsolidationGroup: a group by its code, as the list gives it;
// one that does not exist, or that the caller may not see whole, is
// unknown (404 consolidation.group_unknown).
func TestGetConsolidationGroup(t *testing.T) {
	e := groupEnv(t)
	g, _ := e.Must(200, "GET", "/v1/consolidation-groups/EURO", "", "")
	want(t, "the group EURO", []string{groupLine(g)}, euro)
	e.Problem(404, "consolidation.group_unknown", "GET", "/v1/consolidation-groups/NONE", "", "")
	reader := e.As(e.Reader(t, "reader", entities(t, e, "1000", "2000", "3000")...))
	g, _ = reader.Must(200, "GET", "/v1/consolidation-groups/ACME", "", "")
	want(t, "the group ACME as a reader of its companies", []string{groupLine(g)}, acme)
	p := reader.Problem(404, "consolidation.group_unknown", "GET", "/v1/consolidation-groups/EURO", "", "")
	if detail := fmt.Sprint(p["detail"]); strings.Contains(detail, "4000") || strings.Contains(detail, "5000") {
		t.Errorf("the refusal names a company of the group: got %q, want none", detail)
	}
}

// TestConsolidationGroupsRefusals: without a configuration there is no
// group (409 config.not_bootstrapped); the groups name the companies whose
// figures later slices show, so the ledger's entry read is needed besides
// consolidation.read (403 identity.permission_missing).
func TestConsolidationGroupsRefusals(t *testing.T) {
	ten := pgtest.NewTenant(t, "consolidation.read", "ledger.entry.read")
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	for _, path := range []string{"/v1/consolidation-groups", "/v1/consolidation-groups/ACME", "/v1/group-accounts"} {
		c.Problem(409, "config.not_bootstrapped", "GET", path, "", "")
	}
	e := fixture.NewWith(t, nil, fixture.Consolidation, "consolidation.read")
	e.Problem(403, "identity.permission_missing", "GET", "/v1/consolidation-groups", "", "")
	e.Problem(403, "identity.permission_missing", "GET", "/v1/consolidation-groups/ACME", "", "")
	if page, _ := e.Must(200, "GET", "/v1/group-accounts?page_size=1", "", ""); len(page["items"].([]any)) != 1 {
		t.Errorf("group accounts with consolidation.read alone: got %v, want one", page)
	}
}

// TestDatabaseFailures calls each handler and the configuration section
// with each of their database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := groupEnv(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Consolidation(app.Config{Pages: pages})
	doc := fixture.Document(t)
	withGroups(doc)
	setup := section(t)
	for name, fn := range map[string]func(ctx context.Context) error{
		"list groups": func(ctx context.Context) error {
			_, err := h.ListConsolidationGroups(ctx, gen.ListConsolidationGroupsParams{})
			return err
		},
		"get a group": func(ctx context.Context) error {
			_, err := h.GetConsolidationGroup(ctx, gen.GetConsolidationGroupParams{Code: "ACME"})
			return err
		},
		"list group accounts": func(ctx context.Context) error {
			_, err := h.ListGroupAccounts(ctx, gen.ListGroupAccountsParams{})
			return err
		},
		"check the configuration": func(ctx context.Context) error {
			_, err := setup.Check(ctx, nil, doc)
			return err
		},
	} {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
	}
	// The contract refuses a page size of 0 before the handler; called
	// directly, the handlers refuse it themselves.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListConsolidationGroups(ctx, gen.ListConsolidationGroupsParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListGroupAccounts(ctx, gen.ListGroupAccountsParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
}
