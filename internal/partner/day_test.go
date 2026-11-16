package partner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestCompanyEveningBankAccount: at 02:30 UTC on 16 November 2026, 21:30
// on the 15th in Toronto, an employee is set up as a supplier of a Toronto
// company with his bank account from the company's day, the 15th. It is
// accepted, not refused as backdated by UTC's day; approved, it keeps that
// first day rather than starting on UTC's, the 16th; and the payee lookup
// of a payment run dated on the company's day finds it (walkthrough steps
// 91 and 97, which failed every day from 19:00 to 24:00 in Toronto). At
// 00:30 on the 16th in Toronto, and with every company in UTC at 02:30,
// the 15th is the day before partner's day and refused.
func TestCompanyEveningBankAccount(t *testing.T) {
	evening := time.Date(2026, 11, 16, 2, 30, 0, 0, time.UTC)
	companyDay := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		zone     string
		at       time.Time
		accepted bool
	}{
		"Toronto's evening, UTC's next day":   {"America/Toronto", evening, true},
		"Toronto's next day":                  {"America/Toronto", evening.Add(3 * time.Hour), false},
		"every company in UTC, UTC's evening": {"UTC", evening, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) {
				for i := range doc.Companies {
					doc.Companies[i].TimeZone = c.zone
				}
			}, perms...)
			now := func() time.Time { return c.at }
			ports := partner.Ports{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: apitest.FieldKeys(),
				PaymentMethods: payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, PlanningGroups: cash.PlanningGroups{},
				Schemas: configuration.Settings{}, Rules: configuration.Settings{}, Now: now}
			in := partner.NewPartner{Kind: "person", Name: "Ben Buyer", CountryCode: "CA", Roles: []string{"supplier"}, Reason: "Employee",
				SupplierCompanies: []partner.SupplierCompanyInput{{LegalEntityID: f.DE, ReconciliationAccountID: f.Account(t, "2000", "211000"),
					PaymentTermCode: "NT30"}},
				BankAccounts: []partner.BankInput{{Purpose: "payments", AccountScheme: "ca", InstitutionNumber: "003", TransitNumber: "00002",
					AccountNumber: "7000123", BankCountryCode: "CA", HolderName: "Ben Buyer", ValidFrom: companyDay}}}
			var created partner.Partner
			var change partner.Change
			err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
				created, change, err = partner.CreatePartner(ctx, ports, in)
				return err
			})
			var p *problem.Problem
			if !c.accepted {
				if !errors.As(err, &p) || p.Code != "partner.backdated_bank_account" {
					t.Fatalf("bank account from %s at %s: got %v, want partner.backdated_bank_account", companyDay.Format(time.DateOnly), c.at, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bank account from %s at %s: got %v, want accepted", companyDay.Format(time.DateOnly), c.at, err)
			}
			err = apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
				return partner.Approvals{Schemas: configuration.Settings{}, Now: now}.Approved(ctx, change.ID, uuid.Nil)
			})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			err = apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
				payees, err := partner.Payees{}.Payees(ctx, f.DE, []uuid.UUID{created.ID}, companyDay)
				if err != nil {
					return err
				}
				if got := payees[created.ID]; got.BankAccountID == nil || !got.BankValidFrom.Equal(companyDay) {
					t.Errorf("payee on %s: got bank account %v from %s, want the account from %s", companyDay.Format(time.DateOnly),
						got.BankAccountID, got.BankValidFrom.Format(time.DateOnly), companyDay.Format(time.DateOnly))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestDay: partner's day is the earliest of the companies' days, UTC's
// when the configuration lists no company; a stored zone the binary
// cannot load fails rather than giving UTC's day.
func TestDay(t *testing.T) {
	at := time.Date(2026, 11, 16, 2, 30, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		zones []string
		want  string
	}{
		"no company":                   {nil, "2026-11-16"},
		"Dubai alone":                  {[]string{"Asia/Dubai"}, "2026-11-16"},
		"Dubai and Toronto":            {[]string{"Asia/Dubai", "America/Toronto"}, "2026-11-15"},
		"Toronto, then UTC":            {[]string{"America/Toronto", "UTC"}, "2026-11-15"},
		"a company that names no zone": {[]string{""}, "2026-11-16"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := &config.Config{}
			for i, z := range c.zones {
				doc.Companies = append(doc.Companies, config.Company{Code: string(rune('1'+i)) + "000", TimeZone: z})
			}
			got, err := partner.Day(doc, at)
			if err != nil || got.Format(time.DateOnly) != c.want {
				t.Errorf("day at %s: got %s (%v), want %s", at, got.Format(time.DateOnly), err, c.want)
			}
		})
	}
	doc := &config.Config{Companies: []config.Company{{Code: "1000", TimeZone: "America/Nowhere"}}}
	if _, err := partner.Day(doc, at); err == nil {
		t.Error("a zone that does not load: got no error, want one")
	}
}

// TestMigrationToday: a migration run's day is partner's, read in the
// request's transaction, at the injected instant when there is one.
func TestMigrationToday(t *testing.T) {
	f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = "America/Toronto" }, perms...)
	at := time.Date(2026, 11, 16, 2, 30, 0, 0, time.UTC)
	m := partner.Migration{Ports: partner.Ports{Schemas: configuration.Settings{}, Now: func() time.Time { return at }}}
	if _, err := m.Today(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		got, err := m.Today(ctx)
		if err == nil && got.Format(time.DateOnly) != "2026-11-15" {
			t.Errorf("migration's day at %s: got %s, want 2026-11-15, Toronto's", at, got.Format(time.DateOnly))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
