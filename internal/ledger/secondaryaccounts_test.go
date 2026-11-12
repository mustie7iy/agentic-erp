package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Secondary cost elements (T60g, ADR-0064 §4).

// withSecondary is doc with company 2000's account 695000 a secondary
// cost element, new unless doc has it, and the numbers' cost elements
// secondary too.
func withSecondary(doc *config.Config, numbers ...string) *config.Config {
	de := &doc.Companies[1]
	if de.Account("695000") == nil {
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "695000", Name: "Assessed overhead", Type: config.Expense})
	}
	for _, n := range append(numbers, "695000") {
		de.Account(n).CostElement = config.CostElementSecondary
	}
	return doc
}

// costElement reads the account's cost element.
func costElement(t *testing.T, e env, number string) string {
	t.Helper()
	var element string
	pgtest.Scan(t, `SELECT cost_element FROM ledger.gl_account WHERE tenant_id = $1 AND id = $2`, []any{e.ten.ID, e.account(t, "2000", number)}, &element)
	return element
}

// TestSecondaryAccountSetup: only an account the activation creates is a
// secondary cost element, and it stays one (ADR-0064 §4, As built T60g):
// an existing account, posted or not, is refused on submission and at
// activation, since lines other contexts posted could be on it; one
// turned into another kind would pass its assessments off as primary
// costs.
func TestSecondaryAccountSetup(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	existing := func(number string) string {
		return "companies[2000].chart_of_accounts[" + number + "].cost_element: account " + number + " exists already, so it cannot become " +
			"a secondary cost element; add a new account with cost_element secondary"
	}
	check := func(t *testing.T, proposed *config.Config) string {
		t.Helper()
		var got []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := s.Check(ctx, document(t), proposed)
			for _, f := range fs {
				got = append(got, f.Path+": "+f.Message)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, "\n")
	}
	for name, c := range map[string]struct {
		proposed *config.Config
		want     string
	}{
		"a new account":                    {withSecondary(document(t)), ""},
		"an existing primary cost account": {withSecondary(document(t), "520000"), existing("520000")},
		"two existing accounts, by number": {withSecondary(document(t), "530000", "520000"), existing("520000") + "\n" + existing("530000")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := check(t, c.proposed); got != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
	// The database refuses the turn, and the API answers its refusal as the problem.
	err := apply(t, e, withSecondary(document(t), "520000"))
	if p := problem.From(err); p.Code != "ledger.secondary_account_not_new" || p.Status != 409 {
		t.Fatalf("activation turning an existing account: got %v, want 409 ledger.secondary_account_not_new", err)
	}
	if got := costElement(t, e, "520000"); got != "primary_cost" {
		t.Errorf("account 520000 after the refused activation: got %s, want primary_cost", got)
	}
	if err := apply(t, e, withSecondary(document(t))); err != nil {
		t.Fatal(err)
	}
	if got := costElement(t, e, "695000"); got != "secondary" {
		t.Errorf("account 695000: got cost element %s, want secondary", got)
	}
	t.Run("activated again as it is", func(t *testing.T) {
		if got := check(t, withSecondary(document(t))); got != "" {
			t.Errorf("findings: got %q, want none", got)
		}
		if err := apply(t, e, withSecondary(document(t))); err != nil {
			t.Errorf("the same document again: got %v, want it applied", err)
		}
	})
	t.Run("turned into a primary cost account", func(t *testing.T) {
		primary := withSecondary(document(t))
		primary.Companies[1].Account("695000").CostElement = config.CostElementPrimaryCost
		want := "companies[2000].chart_of_accounts[695000].cost_element: account 695000 is a secondary cost element and stays one once activated"
		if got := check(t, primary); got != want {
			t.Errorf("findings: got %q, want %q", got, want)
		}
		if err := apply(t, e, primary); problem.From(err).Code != "ledger.secondary_account_not_new" {
			t.Errorf("activation turning the account: got %v, want ledger.secondary_account_not_new", err)
		}
		if got := costElement(t, e, "695000"); got != "secondary" {
			t.Errorf("account 695000 after the refused activation: got %s, want secondary", got)
		}
	})
}

// TestSecondaryAccountPostings: a line on a secondary cost element is
// refused in a manual entry, a parked entry, a template and every
// context's posting but controlling's (422 ledger.secondary_account,
// naming the line), a migration run's among them; controlling's line
// names its cost center, which is validated as a cost line's is; the
// database's refusals map to the same problems (ADR-0064 §4).
func TestSecondaryAccountPostings(t *testing.T) {
	a := setupAssigned(t)
	doc := parallel(t)
	co := doc.Company("2000")
	co.CostCenterAccounting, co.ProfitCenterAccounting, co.DefaultProfitCenter = true, true, "DUMMY"
	co.DefaultAssignments = []config.DefaultAssignment{{Account: "520000", CostCenter: "1000"}, {Account: "540000", CostCenter: "4000"}}
	if err := a.apply(t, "2026-09-01", withSecondary(doc)); err != nil {
		t.Fatal(err)
	}
	secondary, bank := a.account(t, "2000", "695000"), a.bank.String()
	body := entry(a.de, "2026-05-15", "EUR", line{bank, "-10.00", ""}, line{secondary, "10.00", ""})
	for _, c := range []struct{ name, path, body string }{
		{"a manual entry", "/v1/journal-entries", body},
		{"a parked entry", "/v1/parked-journal-entries", body},
		{"a template", "/v1/entry-templates", templateBody(a.de, "Overhead", "", line{bank, "-10.00", ""}, line{secondary, "10.00", ""})},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := a.in(t).Problem(422, "ledger.secondary_account", "POST", c.path, c.body, "")
			if errs, _ := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "lines[1].account_id" {
				t.Errorf("refusal's field: got %v, want lines[1].account_id", m["errors"])
			}
		})
	}
	account := uuid.MustParse(secondary)
	post := func(source, day string, lines ...ledger.PostingLine) (ledger.Entry, error) {
		var posted ledger.Entry
		err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
			posted, err = postings().Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
				Currency: "EUR", SourceContext: source, SourceDocumentID: uuid.New(), Lines: lines})
			return err
		})
		return posted, err
	}
	for _, source := range []string{"payables", "payments", "assets", "integration"} {
		t.Run("from "+source, func(t *testing.T) {
			_, err := post(source, "2026-05-10", centered(account, 5000, &a.cc1000, nil, nil), centered(a.bank, -5000, nil, nil, nil))
			pgtest.WantErr(t, err, "ledger.secondary_account")
		})
	}
	t.Run("from controlling", func(t *testing.T) {
		posted, err := post("controlling", "2026-05-10", centered(account, -5000, &a.cc1000, nil, nil), centered(account, 5000, &a.cc2000, nil, nil))
		if err != nil {
			t.Fatalf("an allocation's entry: got %v, want it posted", err)
		}
		var got string
		pgtest.Scan(t, `SELECT string_agg(l.amount_lc || ' ' || c.code || ' ' || p.code, '; ' ORDER BY l.line_no) FROM ledger.journal_line l
			JOIN ledger.cost_center c ON c.tenant_id = l.tenant_id AND c.id = l.cost_center_id
			JOIN ledger.profit_center p ON p.tenant_id = l.tenant_id AND p.id = l.profit_center_id
			WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{a.ten.ID, posted.ID}, &got)
		// Each line takes its own center's profit center: 1000's is PC1, 2000 has none and takes the company's default.
		if want := "-5000 1000 PC1; 5000 2000 DUMMY"; got != want {
			t.Errorf("the entry's lines with their centers: got %s, want %s", got, want)
		}
	})
	t.Run("from controlling without a cost center", func(t *testing.T) {
		_, err := post("controlling", "2026-05-10", centered(account, -5000, &a.cc1000, nil, nil), centered(account, 5000, nil, nil, nil))
		pgtest.WantErr(t, err, "ledger.cost_center_required")
		if p := problem.From(err); len(p.Errors) != 1 || p.Errors[0].Field != "lines[1].cost_center_id" {
			t.Errorf("refusal's field: got %v, want lines[1].cost_center_id", p.Errors)
		}
	})
	t.Run("from controlling to a center locked for actual costs", func(t *testing.T) {
		_, err := post("controlling", "2026-02-10", centered(account, -5000, &a.cc1000, nil, nil), centered(account, 5000, &a.cc2000, nil, nil))
		pgtest.WantErr(t, err, "ledger.cost_center_invalid")
		if p := problem.From(err); len(p.Errors) != 1 || fmt.Sprint(p.Errors[0].Field, " ", p.Errors[0].Code) != "lines[1].cost_center_id locked_for_costs" {
			t.Errorf("refusal: got %v, want lines[1].cost_center_id locked_for_costs", p.Errors)
		}
	})
	t.Run("no account another context may be given to post to", func(t *testing.T) {
		// What procurement, payables, receivables, accruals and assets ask
		// before they take an account on an item (Accounts.IsPostable's rule).
		for _, c := range []struct {
			name    string
			account uuid.UUID
			want    bool
		}{{"the secondary cost element", account, false}, {"a primary cost account", a.office, true}} {
			var postable, closePostable, offset bool
			if err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
				if postable, err = (ledger.Accounts{}).IsPostable(ctx, a.de, c.account); err != nil {
					return err
				}
				if _, offset, err = (ledger.AssetReads{}).OffsetAccount(ctx, a.de, c.account); err != nil {
					return err
				}
				accounts, err := ledger.CloseAccounts{}.Accounts(ctx, a.de, []uuid.UUID{c.account}, nil)
				if err == nil && len(accounts) == 1 {
					closePostable = accounts[0].Postable
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(postable, " ", offset, " ", closePostable); got != fmt.Sprint(c.want, " ", c.want, " ", c.want) {
				t.Errorf("%s postable, an asset's offset, an accrual's: got %s, want %v for each", c.name, got, c.want)
			}
		}
	})
	for constraint, want := range map[string]string{"journal_line_secondary_account": "422 ledger.secondary_account",
		"gl_account_secondary_kept": "409 ledger.secondary_account_not_new"} {
		p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint})
		if got := fmt.Sprint(p.Status, " ", p.Code); got != want {
			t.Errorf("constraint %s: got %s, want %s", constraint, got, want)
		}
	}
}
