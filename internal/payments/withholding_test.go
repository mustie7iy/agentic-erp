package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payments"
)

// withheld is a run's payment of the supplier as "amount discount
// withholding", and its items' "amount-discount-withholding".
func (e env) withheld(t *testing.T, run map[string]any) string {
	t.Helper()
	amount := func(m map[string]any, key string) any { return m[key].(map[string]any)["amount"] }
	for _, p := range e.payments(t, run) {
		pay := p.(map[string]any)
		if pay["supplier_id"] != e.Supplier.String() {
			continue
		}
		var items []string
		for _, it := range pay["items"].([]any) {
			item := it.(map[string]any)
			items = append(items, fmt.Sprint(amount(item, "amount"), "-", amount(item, "discount_amount"), "-", amount(item, "withholding_amount")))
		}
		return fmt.Sprint(amount(pay, "amount"), " ", amount(pay, "discount_amount"), " ", amount(pay, "withholding_amount"), " ", items)
	}
	return "none"
}

// TestWithholdingAtPayment: a run deducts the withholding tax of the codes
// an item posted with that are deducted at payment (T46f, ADR-0037), on
// the base less the share of the cash discount it takes, and pays the
// rest; a credit memo's withholding nets against the invoice's. Codes
// deducted at invoice were withheld already. The release posts the
// withholding to the codes' account, and the bank file pays what is left;
// the proposal's event carries the run's withholding;
// posting the payment again posts nothing.
func TestWithholdingAtPayment(t *testing.T) {
	e := at(t, "2026-03-12")
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.Withholding(t, config.WithholdingTaxCode{Code: "W1", Description: "10% at invoice", RatePercent: "10", Account: "236000", Posting: config.WithholdingAtInvoice},
		config.WithholdingTaxCode{Code: "W2", Description: "15% of gross at payment", RatePercent: "15", Base: config.WithholdingGross, Account: "236000",
			Posting: config.WithholdingAtPayment})
	e.SupplierWithholding(t, e.Supplier, "W1", "W2")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01") // W1 withholds 10.00 at invoice: 90.00 owed
	run := e.run(t, "2026-03-10", "2026-03-20")
	// 3% discount: 2.70; W2: 15% of 100.00 less 3%, 14.55; paid 90.00 - 2.70 - 14.55.
	if got, want := e.withheld(t, run), "72.75 2.70 14.55 [90.00-2.70-14.55]"; got != want {
		t.Errorf("payment: got %s, want %s", got, want)
	}
	e.decide(t, 200, run, "reject", `{"reason": "test"}`)
	e.memo(t, e.Supplier, "CM-1", "20.00", "2026-03-05", "") // W1 2.00 at invoice: 18.00 credited; W2 on -20.00 at payment, no discount
	run = e.release(t, e.run(t, "2026-03-10", "2026-03-20"), "")
	if got, want := e.withheld(t, run), "57.75 2.70 11.55 [90.00-2.70-14.55 -18.00-0.00--3.00]"; got != want {
		t.Errorf("payment with a credit memo: got %s, want %s", got, want)
	}
	proposed := events(t, e.Tenant, "payment_run_proposed")
	if got, ok := proposed[len(proposed)-1]["withholding_amount"].(map[string]any); !ok || got["amount"] != "11.55" {
		t.Errorf("PaymentRunProposed withholding: got %v, want 11.55", got)
	}
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", pay["journal_entry_id"]), "", "")
	got := map[string]string{}
	for _, l := range je["lines"].([]any) {
		line := l.(map[string]any)
		got[line["account_id"].(string)] += line["amount"].(map[string]any)["amount"].(string) + " "
	}
	want := map[string]string{e.Account(t, "2000", "211000").String(): "90.00 -18.00 ", e.Account(t, "2000", "100000").String(): "-57.75 ",
		e.Account(t, "2000", "400000").String(): "-2.70 ", e.Account(t, "2000", "236000").String(): "-11.55 "}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("payment entry: got %v, want %v", got, want)
	}
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	e.transmit(t, id(files["items"].([]any)[0].(map[string]any)), `"1"`)
	if xml := string(e.connector.files[0].Content); !strings.Contains(xml, `<InstdAmt Ccy="EUR">57.75</InstdAmt>`) {
		t.Errorf("bank file: got\n%s\nwant it to pay 57.75", xml)
	}
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		posted, err := endpoints(e.connector).PostPayment(ctx, r, uuidOf(id(pay)))
		if posted {
			t.Error("payment posted again: got posted, want nothing posted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWithholdingAndDiscountTax: what an invoice withheld at invoice still
// counts as its gross when a run splits the cash discount's tax share: the
// discount on what is left corrects the input tax by the invoice's tax over
// its gross (T36d), not over what the withholding left.
func TestWithholdingAndDiscountTax(t *testing.T) {
	e := at(t, "2026-03-12")
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.Withholding(t, config.WithholdingTaxCode{Code: "W1", Description: "10% at invoice", RatePercent: "10", Account: "236000", Posting: config.WithholdingAtInvoice})
	e.SupplierWithholding(t, e.Supplier, "W1")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "INV-1",
		"invoice_date": "2026-03-01", "gross_amount": {"amount": "119.00", "currency": "EUR"}, "tax_amount": {"amount": "19.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "100.00", "currency": "EUR"},
		"tax_code": "V1"}]}`, e.DE, e.Supplier, e.expense), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
	run := e.release(t, e.run(t, "2026-03-10", "2026-03-20"), "")
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", pay["journal_entry_id"]), "", "")
	got := map[string]string{}
	for _, l := range je["lines"].([]any) {
		line := l.(map[string]any)
		got[line["account_id"].(string)] = line["amount"].(map[string]any)["amount"].(string)
	}
	// 3% of 109.00 is 3.27; its tax share 3.27 x 19/119 = 0.52.
	want := map[string]string{e.Account(t, "2000", "211000").String(): "109.00", e.Account(t, "2000", "100000").String(): "-105.73",
		e.Account(t, "2000", "400000").String(): "-2.75", e.Account(t, "2000", "140000").String(): "-0.52"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("payment entry: got %v, want %v", got, want)
	}
}

// TestWithholdingBeyondTheItem: a withholding at payment that, rounded
// after the cash discount, reaches what the item still pays fails the
// proposal with a problem (payments.withholding_too_large), not with the
// payment item's check: 99.495% of 1.00 leaves the invoice 0.01, but of the
// 0.97 left after the 3% discount it rounds to 0.97 and leaves nothing,
// while another invoice keeps the supplier owed.
func TestWithholdingBeyondTheItem(t *testing.T) {
	e := at(t, "2026-03-12")
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.Withholding(t, config.WithholdingTaxCode{Code: "W2", Description: "99.495% of gross at payment", RatePercent: "99.495", Base: config.WithholdingGross,
		Account: "236000", Posting: config.WithholdingAtPayment})
	e.SupplierWithholding(t, e.Supplier, "W2")
	e.invoice(t, e.Supplier, "INV-1", "1.00", "2026-03-01")
	e.invoice(t, e.Supplier, "INV-2", "100.00", "2026-03-01") // the supplier is still owed something
	op := e.propose(t, "2026-03-10", "2026-03-20")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["status"] != "failed" {
		t.Fatalf("run status: got %v, want failed: %s %v", run["status"], e.withheld(t, run), e.exclusions(t, run))
	}
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if got, ok := done["error"].(map[string]any); !ok || got["code"] != "payments.withholding_too_large" {
		t.Errorf("operation error: got %v, want payments.withholding_too_large", got)
	}
}
