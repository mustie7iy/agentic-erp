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

// withholdingCodes gives legal entity 1000 the ledger's withholding tax
// codes W1 and W2, on a liability account 236000.
func (e env) withholdingCodes(t *testing.T) {
	t.Helper()
	account := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
		SELECT $1, $2, chart_id, '236000', 'WHT payable', 'liability', 'none', 'active', $3, $3 FROM ledger.gl_account WHERE id = $4`,
		e.ten.ID, account, e.ten.Human.ID, e.recon)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
		e.ten.ID, e.le, account, e.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.withholding_tax_code (tenant_id, legal_entity_id, code, description, rate_pct, base, account_id, posting, created_by, updated_by)
		VALUES ($1, $2, 'W1', 'W1', 10, 'net', $3, 'invoice', $4, $4), ($1, $2, 'W2', 'W2', 15, 'gross', $3, 'payment', $4, $4)`,
		e.ten.ID, e.le, account, e.ten.Human.ID)
}

// withCodes is a supplier body whose company data has the codes.
func (e env) withCodes(name, codes string) string {
	return strings.Replace(e.supplier(name, 0), `"payment_term_code": "NT30"}`, `"payment_term_code": "NT30", "withholding_tax_codes": [`+codes+`]}`, 1)
}

// TestSupplierWithholding: a supplier's withholding tax codes per company
// (SAP LFBW; T46e) are active codes of the legal entity, each once; a
// change replaces them, taking codes off and putting them back, and the
// payables context reads them with the supplier.
func TestSupplierWithholding(t *testing.T) {
	e := setup(t)
	e.withholdingCodes(t)
	p, c := e.create(t, e.withCodes("Muster", `"W1"`))
	e.mustApply(t, id(c))
	codes := func() string {
		t.Helper()
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
		return fmt.Sprint(got["supplier_companies"].([]any)[0].(map[string]any)["withholding_tax_codes"])
	}
	if got := codes(); got != "[W1]" {
		t.Errorf("codes: got %s, want [W1]", got)
	}
	change := func(list string) {
		t.Helper()
		ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(id(p), fmt.Sprintf(`{"supplier_companies": [{"legal_entity_id": %q,
			"reconciliation_account_id": %q, "payment_term_code": "NT30", "withholding_tax_codes": [%s]}]}`, e.le, e.recon, list)), "")
		e.mustApply(t, id(ch))
	}
	change(`"W2"`)
	if got := codes(); got != "[W2]" {
		t.Errorf("codes after the change: got %s, want [W2]", got)
	}
	change(`"W1", "W2"`)
	if got := codes(); got != "[W1 W2]" {
		t.Errorf("codes put back: got %s, want [W1 W2]", got)
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		s, _, err := partner.Suppliers{}.Supplier(ctx, uuid.MustParse(id(p)), e.le)
		if err != nil {
			return err
		}
		if got := fmt.Sprint(s.WithholdingTaxCodes); got != "[W1 W2]" {
			t.Errorf("supplier's codes: got %s, want [W1 W2]", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ codes, code string }{
		"unknown code": {`"W9"`, "partner.unknown_withholding_tax_code"},
		"a code twice": {`"W1", "W1"`, "partner.duplicate_withholding_tax_code"},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(422, c.code, "POST", "/v1/business-partners", e.withCodes("Other "+name, c.codes), "")
		})
	}
}

// TestWithholdingCodesSection: a configuration that takes a withholding tax
// code away from a company while a supplier has it, or a pending partner
// change gives it, is refused when it is submitted and when it is
// activated; a company without a legal entity is passed over (T46e).
func TestWithholdingCodesSection(t *testing.T) {
	e := setup(t)
	e.withholdingCodes(t)
	p, c := e.create(t, e.withCodes("Muster", `"W1"`))
	e.mustApply(t, id(c))
	company := func(code string, wht ...string) config.Company {
		co := config.Company{Code: code}
		for _, w := range wht {
			co.WithholdingTaxCodes = append(co.WithholdingTaxCodes, config.WithholdingTaxCode{Code: w})
		}
		return co
	}
	section := partner.WithholdingCodes{LegalEntities: tenancy.Directory{}}
	findings := func(proposed *config.Config) []string {
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
		return paths
	}
	none := &config.Config{Companies: []config.Company{company("1000"), company("9999")}}
	if got := fmt.Sprint(findings(none)); got != "[companies[1000].withholding_tax_codes[W1]]" {
		t.Errorf("findings with W1 on a supplier: got %s, want [companies[1000].withholding_tax_codes[W1]]", got)
	}
	ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(id(p), fmt.Sprintf(`{"supplier_companies": [{"legal_entity_id": %q,
		"reconciliation_account_id": %q, "payment_term_code": "NT30", "withholding_tax_codes": ["W1", "W2"]}]}`, e.le, e.recon)), "")
	if got := fmt.Sprint(findings(&config.Config{Companies: []config.Company{company("1000", "W1")}})); got != "[companies[1000].withholding_tax_codes[W2]]" {
		t.Errorf("findings with W2 in a pending change: got %s, want [companies[1000].withholding_tax_codes[W2]]", got)
	}
	if got := findings(&config.Config{Companies: []config.Company{company("1000", "W1", "W2")}}); got != nil {
		t.Errorf("findings keeping both codes: got %v, want none", got)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		if fs, err := section.Check(ctx, nil, none); err != nil || fs != nil {
			t.Errorf("a first document: got %v %v, want nothing", fs, err)
		}
		return section.Apply(ctx, none)
	})
	pgtest.WantErr(t, err, "partner.withholding_tax_code_in_use")
	e.mustApply(t, id(ch))
	call := func(ctx context.Context) error {
		return section.Apply(ctx, &config.Config{Companies: []config.Company{company("1000", "W1", "W2")}})
	}
	if err := call(context.Background()); err == nil {
		t.Error("apply outside a unit of work: got nil error, want an error")
	}
	if _, err := section.Check(context.Background(), &config.Config{}, none); err == nil {
		t.Error("check outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "withholding codes apply", call)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "withholding codes check", func(ctx context.Context) error {
		_, err := section.Check(ctx, &config.Config{}, none)
		return err
	})
}
