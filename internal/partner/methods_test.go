package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// paymentMethods gives legal entity 1000 the payment methods T and U, and
// X, which the configuration no longer has.
func (e env) paymentMethods(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
		VALUES ($1, $2, 'T', 'Transfer', true, $3, $3), ($1, $2, 'U', 'Urgent', true, $3, $3), ($1, $2, 'X', 'Gone', false, $3, $3)`,
		e.ten.ID, e.le, e.ten.Human.ID)
}

// withMethods is a supplier body whose company data has the methods.
func (e env) withMethods(name, methods string) string {
	return strings.Replace(e.supplier(name, 0), `"payment_term_code": "NT30"}`, `"payment_term_code": "NT30", "payment_methods": [`+methods+`]}`, 1)
}

// methodsChange is a partner change giving the supplier the methods.
func (e env) methodsChange(partnerID, methods string) string {
	return e.update(partnerID, fmt.Sprintf(`{"supplier_companies": [{"legal_entity_id": %q,
		"reconciliation_account_id": %q, "payment_term_code": "NT30", "payment_methods": [%s]}]}`, e.le, e.recon, methods))
}

// TestSupplierPaymentMethods: a supplier's payment methods per company
// (SAP LFB1-ZWELS; ADR-0038) are active methods of the legal entity, each
// once, in the order given; a change replaces them, and the payments
// context reads them with the supplier.
func TestSupplierPaymentMethods(t *testing.T) {
	e := setup(t)
	e.paymentMethods(t)
	p, c := e.create(t, e.withMethods("Muster", `"U", "T"`))
	e.mustApply(t, id(c))
	methods := func() string {
		t.Helper()
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
		return fmt.Sprint(got["supplier_companies"].([]any)[0].(map[string]any)["payment_methods"])
	}
	if got := methods(); got != "[U T]" {
		t.Errorf("methods: got %s, want [U T]", got)
	}
	ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.methodsChange(id(p), `"T"`), "")
	e.mustApply(t, id(ch))
	if got := methods(); got != "[T]" {
		t.Errorf("methods after the change: got %s, want [T]", got)
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		s, _, err := partner.Suppliers{}.Supplier(ctx, uuid.MustParse(id(p)), e.le)
		if got := fmt.Sprint(s.PaymentMethods); got != "[T]" {
			t.Errorf("supplier's methods: got %s, want [T]", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ methods, code string }{
		"unknown method":  {`"Q"`, "partner.unknown_payment_method"},
		"inactive method": {`"X"`, "partner.unknown_payment_method"},
		"a method twice":  {`"T", "T"`, "partner.duplicate_payment_method"},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(422, c.code, "POST", "/v1/business-partners", e.withMethods("Other "+name, c.methods), "")
		})
	}
}

// TestPaymentMethodCodesSection: a configuration that takes a payment
// method away from a company while a supplier has it, or a pending partner
// change gives it, is refused when it is submitted and when it is
// activated (ADR-0038).
func TestPaymentMethodCodesSection(t *testing.T) {
	e := setup(t)
	e.paymentMethods(t)
	p, c := e.create(t, e.withMethods("Muster", `"T"`))
	e.mustApply(t, id(c))
	company := func(code string, methods ...string) config.Company {
		co := config.Company{Code: code}
		for _, m := range methods {
			co.PaymentMethods = append(co.PaymentMethods, config.PaymentMethod{Code: m})
		}
		return co
	}
	section := partner.PaymentMethodCodes{LegalEntities: tenancy.Directory{}}
	findings := func(proposed *config.Config) string {
		t.Helper()
		var paths []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := section.Check(ctx, &config.Config{}, proposed)
			for _, f := range fs {
				paths = append(paths, f.Path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(paths)
	}
	none := &config.Config{Companies: []config.Company{company("1000"), company("9999")}}
	if got := findings(none); got != "[companies[1000].payment_methods[T]]" {
		t.Errorf("findings with T on a supplier: got %s, want [companies[1000].payment_methods[T]]", got)
	}
	ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.methodsChange(id(p), `"T", "U"`), "")
	if got := findings(&config.Config{Companies: []config.Company{company("1000", "T")}}); got != "[companies[1000].payment_methods[U]]" {
		t.Errorf("findings with U in a pending change: got %s, want [companies[1000].payment_methods[U]]", got)
	}
	if got := findings(&config.Config{Companies: []config.Company{company("1000", "T", "U")}}); got != "[]" {
		t.Errorf("findings keeping both methods: got %s, want none", got)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return section.Apply(ctx, none) })
	pgtest.WantErr(t, err, "partner.payment_method_in_use")
	e.mustApply(t, id(ch))
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return section.Apply(ctx, &config.Config{Companies: []config.Company{company("1000", "T", "U")}})
	}); err != nil {
		t.Errorf("apply keeping both methods: got %v, want nil", err)
	}
}
