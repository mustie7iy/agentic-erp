package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withOpenItems is the document with company 2000's bank account 100000
// and a new account 102000 open-item managed (ADR-0047).
func withOpenItems(tb testing.TB) *config.Config {
	tb.Helper()
	doc := document(tb)
	de := &doc.Companies[1]
	de.Account("100000").OpenItems = true
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "102000", Name: "Bank clearing", Type: config.Asset, OpenItems: true})
	return doc
}

// with102000OpenItems is the document with company 2000's new account
// 102000 open-item managed, which has no postings.
func with102000OpenItems(tb testing.TB) *config.Config {
	tb.Helper()
	doc := withUnmanaged102000(tb)
	doc.Companies[1].Account("102000").OpenItems = true
	return doc
}

// withUnmanaged102000 is the document with company 2000's account 102000
// applied without open-item management.
func withUnmanaged102000(tb testing.TB) *config.Config {
	tb.Helper()
	doc := document(tb)
	doc.Companies[1].ChartOfAccounts = append(doc.Companies[1].ChartOfAccounts, config.Account{Number: "102000", Name: "Bank clearing", Type: config.Asset})
	return doc
}

// TestOpenItemsOnlyUnposted: an account becomes open-item managed only
// while it has no postings in the legal entity (product owner
// 2026-10-20), on the first activation as on a later one; an account
// already open-item managed keeps it, and never loses it.
func TestOpenItemsOnlyUnposted(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	base := withUnmanaged102000(t)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, base) }); err != nil {
		t.Fatal(err)
	}
	bank := e.account(t, "2000", "100000")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{e.account(t, "2000", "510000"), "10.00", ""}, line{bank, "-10.00", ""}), "")
	posted := "companies[2000].chart_of_accounts[100000].open_items: account 100000 has postings, so it cannot become open-item managed; " +
		"name a new clearing account and transfer the old balance to it"
	turnedOff := "companies[2000].chart_of_accounts[100000].open_items: open-item management cannot be turned off once activated"
	for _, c := range []struct {
		name           string
		base, proposed *config.Config
		want           string
	}{
		{"turned on with postings", base, withOpenItems(t), posted},
		{"turned on with postings, first activation", nil, withOpenItems(t), posted},
		{"already open-item managed", withOpenItems(t), withOpenItems(t), ""},
		{"turned off", withOpenItems(t), document(t), turnedOff},
		{"turned on for an applied account without postings", withUnmanaged102000(t), withOpenItems(t), posted},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				fs, err := s.Check(ctx, c.base, c.proposed)
				for _, f := range fs {
					got = append(got, f.Path+": "+f.Message)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestOpenItemsRefusedAtActivation: an entry posted to an account after
// the change making it open-item managed was submitted, and before it is
// activated, refuses the activation (ADR-0047); an unposted account is
// made open-item managed, and its company account shows it.
func TestOpenItemsRefusedAtActivation(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	base := withUnmanaged102000(t)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, base) }); err != nil {
		t.Fatal(err)
	}
	proposed := withOpenItems(t)
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := s.Check(ctx, base, proposed)
		if len(fs) != 0 {
			t.Errorf("findings on submission: got %v, want none", fs)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bank := e.account(t, "2000", "100000")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{e.account(t, "2000", "510000"), "10.00", ""}, line{bank, "-10.00", ""}), "")
	err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, proposed) })
	if p := (*problem.Problem)(nil); !errors.As(err, &p) || p.Code != "ledger.open_items_account_posted" {
		t.Fatalf("activation after a posting: got %v, want ledger.open_items_account_posted", err)
	}
	if got := openItemManaged(t, e, bank); got {
		t.Errorf("account 100000 open-item managed after the refused activation: got %v, want false", got)
	}
	proposed.Companies[1].Account("100000").OpenItems = false
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, proposed) }); err != nil {
		t.Fatal(err)
	}
	if got := openItemManaged(t, e, e.account(t, "2000", "102000")); !got {
		t.Errorf("account 102000 open-item managed: got %v, want true", got)
	}
}

// openItemManaged reads the account's open-item flag in the entity of
// company 2000 through the API.
func openItemManaged(t *testing.T, e env, account string) bool {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/accounts/"+account, "", "")
	for _, c := range got["company_settings"].([]any) {
		if c := c.(map[string]any); c["legal_entity_id"] == e.de.String() {
			return c["is_open_item_managed"] == true
		}
	}
	t.Fatalf("account %s has no company account in %s", account, e.de)
	return false
}
