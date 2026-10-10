package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func endpoints() *partner.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &partner.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: apitest.FieldKeys(), Approvals: fixture.Approvals()}
}

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	u := func(m map[string]any) uuid.UUID { return uuid.MustParse(id(m)) }
	bankOf := func(p map[string]any) uuid.UUID {
		page, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p)+"/bank-accounts", "", "")
		return u(page["items"].([]any)[0].(map[string]any))
	}
	// a: active with a pending update and a pending bank account end.
	a := e.active(t, "A", 0)
	update, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(id(a), fmt.Sprintf(`{"name": "A2", "roles": ["supplier", "customer"],
		"supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": "NT60"}],
		"addresses": [{"purpose": "legal", "line1": "L", "city": "C", "country_code": "DE", "valid_from": %q}],
		"tax_identifiers": [{"scheme": "EU_VAT", "value": "DE1"}], "extensions": {"x": 1}}`, e.le, e.recon, e.date(0))), "")
	_, aTag := e.Must(200, "GET", "/v1/business-partners/"+id(a)+"/bank-accounts/"+bankOf(a).String(), "", "")
	bankEnd, _ := e.Must(200, "POST", "/v1/business-partners/"+id(a)+"/bank-accounts/"+bankOf(a).String()+":end",
		fmt.Sprintf(`{"valid_to": %q, "reason": "r"}`, e.date(10)), aTag)
	// c: active with a pending new bank account.
	c := e.active(t, "C", 0)
	bankAdd, _ := e.Must(201, "POST", "/v1/business-partners/"+id(c)+"/bank-accounts", fmt.Sprintf(`{"account_number": "12345678",
		"account_scheme": "domestic", "bank_country_code": "DE", "holder_name": "C", "valid_from": %q, "reason": "r"}`, e.date(1)), "")
	// d: blocked with a pending unblock; f: blocked without one.
	d := e.active(t, "D", 0)
	e.Must(200, "POST", "/v1/business-partners/"+id(d)+":block", `{"reason": "r"}`, `"2"`)
	unblock, _ := e.Must(200, "POST", "/v1/business-partners/"+id(d)+":unblock", `{"reason": "r"}`, `"3"`)
	f := e.active(t, "F", 0)
	e.Must(200, "POST", "/v1/business-partners/"+id(f)+":block", `{"reason": "r"}`, `"2"`)
	// p: pending with its create change.
	p, create := e.create(t, e.supplier("P", 0))

	h := endpoints()
	when := e.day.AddDate(0, 0, 3)
	newPartner := &gen.BusinessPartnerCreate{Kind: "organization", Name: "N", CountryCode: "DE", Roles: []string{"supplier"}, Reason: "r",
		LegalName: gen.NewOptString("N GmbH"), Language: gen.NewOptString("de"),
		SupplierCompanies: []gen.SupplierCompanyInput{{LegalEntityID: e.le, ReconciliationAccountID: e.recon, PaymentTermCode: "NT30"}},
		Addresses: []gen.Address{{Purpose: "legal", Line1: "L", City: "C", CountryCode: "DE", ValidFrom: e.day, ValidTo: gen.NewOptDate(when),
			Line2: gen.NewOptString("2"), PostalCode: gen.NewOptString("1"), Region: gen.NewOptString("BE")}},
		TaxIdentifiers: []gen.TaxIdentifierInput{{Scheme: "EU_VAT", Value: "DE123456789"}},
		BankAccounts: []gen.BankAccountInput{{AccountNumber: iban, AccountScheme: "iban", BankCountryCode: "DE", HolderName: "N", ValidFrom: e.day,
			Bic: gen.NewOptString("COBADEFFXXX")}},
		Extensions: gen.NewOptExtensions(gen.Extensions{"x": []byte("1")})}
	calls := map[string]func(ctx context.Context) error{
		"create partner": func(ctx context.Context) error {
			_, err := h.CreateBusinessPartner(ctx, newPartner, gen.CreateBusinessPartnerParams{})
			return err
		},
		"get partner": func(ctx context.Context) error {
			_, err := h.GetBusinessPartner(ctx, gen.GetBusinessPartnerParams{ID: u(a)})
			return err
		},
		"list partners": func(ctx context.Context) error {
			_, err := h.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{NamePrefix: gen.NewOptString("A")})
			return err
		},
		"block": func(ctx context.Context) error {
			_, err := h.BlockBusinessPartner(ctx, &gen.ReasonInput{Reason: "r"}, gen.BlockBusinessPartnerParams{ID: u(a), IfMatch: "*"})
			return err
		},
		"unblock": func(ctx context.Context) error {
			_, err := h.UnblockBusinessPartner(ctx, &gen.ReasonInput{Reason: "r"}, gen.UnblockBusinessPartnerParams{ID: u(f), IfMatch: "*"})
			return err
		},
		"request update": func(ctx context.Context) error {
			_, err := h.CreatePartnerChange(ctx, &gen.PartnerChangeCreate{PartnerID: u(c), Kind: "update", Reason: "r", Payload: gen.BusinessPartnerChanges{
				Name: gen.NewOptString("C2"), LegalName: gen.NewOptNilString("C2 GmbH"), Roles: []string{"supplier"},
				SupplierCompanies: []gen.SupplierCompanyInput{{LegalEntityID: e.le, ReconciliationAccountID: e.recon, PaymentTermCode: "NT60"}},
				Addresses:         []gen.Address{{Purpose: "legal", Line1: "L", City: "C", CountryCode: "DE", ValidFrom: e.day}},
				TaxIdentifiers:    []gen.TaxIdentifierInput{{Scheme: "EU_VAT", Value: "DE2"}}}}, gen.CreatePartnerChangeParams{})
			return err
		},
		"get change": func(ctx context.Context) error {
			_, err := h.GetPartnerChange(ctx, gen.GetPartnerChangeParams{ID: u(update)})
			return err
		},
		"list changes": func(ctx context.Context) error {
			_, err := h.ListPartnerChanges(ctx, gen.ListPartnerChangesParams{PartnerID: gen.NewOptUUID(u(a))})
			return err
		},
		"request bank account": func(ctx context.Context) error {
			_, err := h.RequestBankAccountChange(ctx, &gen.BankChangeCreate{AccountNumber: iban, AccountScheme: "iban", BankCountryCode: "DE",
				HolderName: "P", ValidFrom: e.day, Reason: "r"}, gen.RequestBankAccountChangeParams{ID: u(p)})
			return err
		},
		"request bank account end": func(ctx context.Context) error {
			_, err := h.RequestBankAccountEnd(ctx, &gen.BankAccountEnd{ValidTo: when, Reason: "r"},
				gen.RequestBankAccountEndParams{ID: u(f), BankAccountID: bankOf(f), IfMatch: "*"})
			return err
		},
		"get bank account": func(ctx context.Context) error {
			_, err := h.GetBankAccount(ctx, gen.GetBankAccountParams{ID: u(a), BankAccountID: bankOf(a)})
			return err
		},
		"list bank accounts": func(ctx context.Context) error {
			_, err := h.ListBankAccounts(ctx, gen.ListBankAccountsParams{ID: u(a), ValidOn: gen.NewOptDate(e.day)})
			return err
		},
	}
	for kind, change := range map[string]map[string]any{"create": create, "update": update, "bank add": bankAdd, "bank end": bankEnd, "unblock": unblock} {
		calls["apply "+kind] = func(ctx context.Context) error {
			_, err := partner.ApplyChange(ctx, u(change))
			return err
		}
	}
	calls["reject"] = func(ctx context.Context) error {
		_, err := partner.RejectChange(ctx, u(create))
		return err
	}
	calls["cancel"] = func(ctx context.Context) error {
		_, err := partner.CancelChange(ctx, u(create))
		return err
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
	}

	// Page sizes are checked by the handlers too, for callers other than the generated router.
	zero := gen.NewOptInt32(0)
	for name, call := range map[string]func(ctx context.Context) error{
		"partners": func(ctx context.Context) error {
			_, err := h.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{PageSize: zero})
			return err
		},
		"changes": func(ctx context.Context) error {
			_, err := h.ListPartnerChanges(ctx, gen.ListPartnerChangesParams{PageSize: zero})
			return err
		},
		"bank accounts": func(ctx context.Context) error {
			_, err := h.ListBankAccounts(ctx, gen.ListBankAccountsParams{ID: u(a), PageSize: zero})
			return err
		},
	} {
		if err := apitest.InRequest(t, e.ten, e.ten.Human, call); err == nil {
			t.Errorf("%s: page_size 0 accepted", name)
		}
	}
}
