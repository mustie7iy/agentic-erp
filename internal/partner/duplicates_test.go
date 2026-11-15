package partner_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Duplicate check when a business partner is created (T89a; 03-data-model
// §3.6): the create change lists the partners with one of its tax
// registration numbers or its normalized name and country; a tax number
// in use is refused unless the creator confirms it.

// named is the body of a partner of the kind, roles and country with the
// name and, unless vat is empty, the VAT number; extra holds further
// fields, from a comma.
func named(kind, name, country, vat, roles, extra string) string {
	taxes := ""
	if vat != "" {
		taxes = fmt.Sprintf(`, "tax_identifiers": [{"scheme": "EU_VAT", "value": %q}]`, vat)
	}
	return fmt.Sprintf(`{"kind": %q, "name": %q, "country_code": %q, "roles": [%s]%s%s, "reason": "New partner"}`, kind, name, country, roles, taxes, extra)
}

// duplicates are a create change's possible duplicates as number name
// matched, in their order.
func duplicates(c map[string]any) string {
	var out []string
	for _, d := range c["possible_duplicates"].([]any) {
		m := d.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v", m["number"], m["name"], m["matched"]))
	}
	return strings.Join(out, ", ")
}

// TestDuplicateCheck: a partner with another's tax registration number is
// refused without the confirmation and listed with it, whatever the roles
// and a blocked one included; a match on name and country, white space
// and case aside, is listed and never refused; a partner whose create was
// rejected is no duplicate; another tenant's partners are never seen.
func TestDuplicateCheck(t *testing.T) {
	e := setup(t)
	first, c := e.create(t, named("organization", "Muster Industrie GmbH", "DE", "DE111111111", `"supplier"`, ""))
	if got := duplicates(c); got != "" {
		t.Errorf("the first partner's duplicates: got %s, want none", got)
	}
	e.mustApply(t, id(c))
	number := first["number"].(string)
	customer := named("person", "Erika Muster", "DE", "de 111111111", `"customer"`, "")
	p := e.Problem(409, "partner.duplicate_tax_number", "POST", "/v1/business-partners", customer, "")
	if !strings.Contains(p["detail"].(string), number+" Muster Industrie GmbH") {
		t.Errorf("refusal: got %v, want it to name %s", p["detail"], number)
	}
	list, _ := e.Must(200, "GET", "/v1/business-partners", "", "")
	if n := len(list["items"].([]any)); n != 1 {
		t.Errorf("partners after the refusal: got %d, want 1", n)
	}
	_, c = e.create(t, strings.Replace(customer, `"reason"`, `"confirm_duplicate": true, "reason"`, 1))
	if got, want := duplicates(c), number+" Muster Industrie GmbH [tax_registration_number]"; got != want {
		t.Errorf("a confirmed duplicate's change: got %s, want %s", got, want)
	}
	// A pending partner counts: the customer's create change is not decided yet.
	// In order: each case's partner is one the next may duplicate.
	for _, c := range []struct{ name, body, want string }{
		{"the name, white space and case aside", named("organization", "  muster   INDUSTRIE\tgmbh ", "DE", "DE222222222", `"supplier"`, ""),
			number + " Muster Industrie GmbH [name_and_country]"},
		{"the name in another country", named("organization", "Muster Industrie GmbH", "US", "", `"supplier"`, ""), ""},
		{"the name and the number", named("organization", "MUSTER INDUSTRIE GMBH", "DE", "DE111111111", `"supplier"`, `, "confirm_duplicate": true`),
			number + " Muster Industrie GmbH [tax_registration_number name_and_country], BP00000002 Erika Muster [tax_registration_number], " +
				"BP00000003   muster   INDUSTRIE\tgmbh  [name_and_country]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ch := e.in(t).create(t, c.body)
			if got := duplicates(ch); got != c.want {
				t.Errorf("possible duplicates: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("a blocked partner counts", func(t *testing.T) {
		e := e.in(t)
		got, tag := e.Must(200, "GET", "/v1/business-partners/"+id(first), "", "")
		if got["status"] != "active" {
			t.Fatalf("first partner: got %v, want active", got["status"])
		}
		e.Must(200, "POST", "/v1/business-partners/"+id(first)+":block", `{"reason": "Fraud suspected"}`, tag)
		e.Problem(409, "partner.duplicate_tax_number", "POST", "/v1/business-partners", named("organization", "Other", "US", "DE111111111", `"supplier"`, ""), "")
	})
	t.Run("a rejected creation is no duplicate", func(t *testing.T) {
		e := e.in(t)
		_, c := e.create(t, named("organization", "Gone", "DE", "DE333333333", `"supplier"`, ""))
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := partner.RejectChange(ctx, uuid.MustParse(id(c)))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, c = e.create(t, named("organization", "Gone", "DE", "DE333333333", `"supplier"`, ""))
		if got := duplicates(c); got != "" {
			t.Errorf("possible duplicates: got %s, want none", got)
		}
	})
	t.Run("another tenant's partners are not seen", func(t *testing.T) {
		other := setup(t)
		_, c := other.create(t, named("organization", "Muster Industrie GmbH", "DE", "DE111111111", `"supplier"`, ""))
		if got := duplicates(c); got != "" {
			t.Errorf("possible duplicates in another tenant: got %s, want none", got)
		}
	})
	t.Run("a migration run loads a duplicate and lists it", func(t *testing.T) {
		e := e.in(t)
		in := e.migrated("Muster Industrie GmbH", 0)
		in.TaxIdentifiers = []partner.TaxInput{{Scheme: "EU_VAT", Value: "DE111111111"}}
		pid, err := e.migrate(t, in)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		changes, _ := e.Must(200, "GET", "/v1/partner-changes?kind=create&partner_id="+pid.String(), "", "")
		if got := duplicates(changes["items"].([]any)[0].(map[string]any)); !strings.HasPrefix(got, number+" Muster Industrie GmbH [tax_registration_number name_and_country]") {
			t.Errorf("the migrated partner's duplicates: got %s, want %s first", got, number)
		}
	})
}

// TestDuplicateTaxNumberAtOnce: two partners with the same tax
// registration number created at once wait for one another on the
// number's lock, so the second finds the first and is refused (ADR-0008).
func TestDuplicateTaxNumberAtOnce(t *testing.T) {
	e := setup(t)
	in := partner.NewPartner{Kind: "organization", Name: "Twin", CountryCode: "DE", Roles: []string{"customer"}, Reason: "r",
		TaxIdentifiers: []partner.TaxInput{{Scheme: "EU_VAT", Value: "DE444444444"}}}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			errs[i] = apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, _, err := partner.CreatePartner(ctx, e.ports(), in)
				return err
			})
		})
	}
	wg.Wait()
	refused := 0
	for _, err := range errs {
		if p := problem.From(err); p != nil && p.Code == "partner.duplicate_tax_number" {
			refused++
		} else if err != nil {
			t.Errorf("create: got %v, want created or partner.duplicate_tax_number", err)
		}
	}
	if refused != 1 {
		t.Errorf("refused: got %d of 2, want 1", refused)
	}
}

// TestNormalizedName: the Go key and the database's
// partner.normalized_name agree, so the index finds what the key names:
// white space of every kind trimmed and collapsed, capitals by Unicode.
func TestNormalizedName(t *testing.T) {
	for _, name := range []string{"Muster Industrie GmbH", "  muster \t industrie\ngmbh  ", "Café Noir", "straße ǆ", "　x y\u0085", ""} {
		var db string
		pgtest.Scan(t, `SELECT partner.normalized_name($1)`, []any{name}, &db)
		if got := partner.NormalizedName(name); got != db {
			t.Errorf("%q: got %q in Go, %q in the database", name, got, db)
		}
	}
}
