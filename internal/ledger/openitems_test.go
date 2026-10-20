package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
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

// TestOpenItemsOnlyUnposted: an account becomes open-item managed only
// while it has no postings in the legal entity (product owner
// 2026-10-20), on the first activation as on a later one; an account
// already open-item managed keeps it, and never loses it.
func TestOpenItemsOnlyUnposted(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	base := document(t)
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
