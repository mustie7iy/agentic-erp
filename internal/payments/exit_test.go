package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestCashDiscountExitCriterion walks T36's exit criterion through the API.
// Payment terms carry discount tiers (3% within 14 days, 2% within 30, net
// 45), activated as a configuration change activates them. An invoice of
// 119.00 (19.00 input tax) and a credit memo of 11.90 naming it post with
// those terms. A run on 10 March pays the invoice, not yet due, within the
// first tier: 3% of 119.00 is 3.57, less 3% of the memo's 11.90 (0.36), so
// 103.89 is paid. The release posts the bank at 103.89, the discount's net
// 2.70 to cash discount and its tax share 0.51 to input tax V1, and clears
// both items at their full amounts; the bank file pays 103.89.
func TestCashDiscountExitCriterion(t *testing.T) {
	e := at(t, "2026-03-12")
	doc := fixture.Document(t)
	doc.PaymentTerms = append(doc.PaymentTerms, config.PaymentTerm{Key: "ZB01", Description: "3% 14, 2% 30, net 45", NetDays: 45,
		Discounts: []config.Discount{{Days: 14, Percent: "3"}, {Days: 30, Percent: "2"}}})
	for i := range doc.Companies {
		doc.Companies[i].AccountDetermination.CashDiscount = "400000"
	}
	if fs := config.Validate(doc, currency.Seeded()); config.HasErrors(fs) {
		t.Fatalf("configuration with discount tiers: %v", fs)
	}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if err := (partner.PaymentTerms{}).Apply(ctx, doc); err != nil {
			return err
		}
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_term_code = 'ZB01', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`,
		e.Tenant.ID, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	taxed := func(kind, ref, gross, tax, net, reference string) map[string]any {
		t.Helper()
		v, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
			"invoice_date": "2026-03-01", "kind": %q, %s "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"},
			"items": [{"account_id": %q, "description": "Paper", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"},
				"tax_code": "V1"}]}`, e.DE, e.Supplier, ref, kind, reference, gross, tax, e.expense, net), "")
		posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(v)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
		return posted
	}
	invoice := taxed("invoice", "INV-1", "119.00", "19.00", "100.00", "")
	if got := fmt.Sprint(invoice["payment_terms"]); got != "map[baseline_date:2026-03-01 code:ZB01 discounts:[map[percent:3 until:2026-03-15] map[percent:2 until:2026-03-31]] net_due_date:2026-04-15]" {
		t.Errorf("invoice payment terms: got %s", got)
	}
	taxed("credit_memo", "CM-1", "11.90", "1.90", "10.00", fmt.Sprintf(`"reference_invoice_id": %q,`, id(invoice)))

	run := e.run(t, "2026-03-10", "2026-03-20")
	if got, want := e.paid(t, run, e.Supplier), "103.89 3.21 [119.00-3.57 -11.90--0.36]"; got != want {
		t.Errorf("proposed payment: got %s, want %s", got, want)
	}
	proposed := e.payments(t, run)
	if len(proposed) != 1 {
		t.Fatalf("payments: got %d, want 1", len(proposed))
	}
	if until := proposed[0].(map[string]any)["discount_until"]; until != "2026-03-15" {
		t.Errorf("payment's discount earned until: got %v, want 2026-03-15, the first tier's last day", until)
	}
	run = e.release(t, run, "")
	paid := e.payments(t, run)
	if len(paid) != 1 {
		t.Fatalf("payments after the release: got %d, want 1", len(paid))
	}
	pay := paid[0].(map[string]any)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", pay["journal_entry_id"]), "", "")
	got := map[string]string{}
	for _, l := range je["lines"].([]any) {
		line := l.(map[string]any)
		key := line["account_id"].(string)
		if code, ok := line["tax_code"].(string); ok {
			key += " " + code
		}
		got[key] = got[key] + line["amount"].(map[string]any)["amount"].(string) + " "
	}
	ap, bank, cash, tax := e.Account(t, "2000", "211000").String(), e.Account(t, "2000", "100000").String(), e.Account(t, "2000", "400000").String(),
		e.Account(t, "2000", "140000").String()
	want := map[string]string{ap: "119.00 -11.90 ", bank: "-103.89 ", cash: "-2.70 ", tax + " V1": "-0.51 "}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("payment entry: got %v, want %v", got, want)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	if items := open["items"].([]any); len(items) != 0 {
		t.Errorf("supplier's open items after the payment: got %v, want none (both cleared)", items)
	}
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	listed := files["items"].([]any)
	if len(listed) != 1 {
		t.Fatalf("bank files: got %d, want 1", len(listed))
	}
	file := listed[0].(map[string]any)
	e.transmit(t, id(file), `"1"`)
	if xml := string(e.connector.files[0].Content); !strings.Contains(xml, `<InstdAmt Ccy="EUR">103.89</InstdAmt>`) {
		t.Errorf("bank file: got\n%s\nwant it to pay 103.89", xml)
	}
}
