package payables_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Withholding tax codes of the tests: W1 10% of net at invoice, W2 15% of
// gross at payment, W3 and W4 60% of gross each, at invoice and at payment.
var (
	w1 = config.WithholdingTaxCode{Code: "W1", Description: "10% of net at invoice", RatePercent: "10", Account: "236000", Posting: config.WithholdingAtInvoice}
	w2 = config.WithholdingTaxCode{Code: "W2", Description: "15% of gross at payment", RatePercent: "15", Base: config.WithholdingGross, Account: "236000",
		Posting: config.WithholdingAtPayment}
	w3 = config.WithholdingTaxCode{Code: "W3", Description: "60% at invoice", RatePercent: "60", Base: config.WithholdingGross, Account: "236000",
		Posting: config.WithholdingAtInvoice}
	w4 = config.WithholdingTaxCode{Code: "W4", Description: "60% at payment", RatePercent: "60", Base: config.WithholdingGross, Account: "236000",
		Posting: config.WithholdingAtPayment}
)

// kept is an entry's withholding taxes as the ledger keeps them with its
// supplier open item: code, posting, base and amount withheld at invoice.
func kept(t *testing.T, entry any) string {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT code, posting, base_amount, coalesce(amount::text, '-') FROM ledger.open_item_withholding
		WHERE journal_entry_id = $1 ORDER BY code`, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code, posting, amount string
		var base int64
		if err := rows.Scan(&code, &posting, &base, &amount); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(code, " ", posting, " ", base, " ", amount))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(out)
}

// TestWithholdingAtInvoice: an invoice of a supplier with withholding tax
// codes (T46f) credits what its codes deducted at invoice withhold, their
// rate of its net or gross, to their account and owes the supplier the
// rest; a credit memo the other way round. The open item keeps every code,
// with its base, so a run withholds those deducted at payment. A code the
// legal entity no longer has, and codes that would withhold the whole
// invoice, are refused.
func TestWithholdingAtInvoice(t *testing.T) {
	e := setup(t)
	e.Withholding(t, w1, w2, w3, w4)
	e.SupplierWithholding(t, e.Supplier, "W1", "W2")
	wht := e.Account(t, "2000", "236000").String()
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	posted := e.act(t, 200, id(invoice), "post", `{"posting_date": "2026-03-21"}`)
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "119.00", "19.00", "", accountItem(e.expense, "100.00")), "")
	credited := e.act(t, 200, id(memo), "post", `{"posting_date": "2026-03-21"}`)
	for _, c := range []struct {
		name  string
		entry any
		lines []line
		items string
	}{
		{"invoice", posted["journal_entry_id"], []line{{e.expense.String(), "100.00", nil}, {e.tax.String(), "19.00", nil},
			{e.ap.String(), "-109.00", nil}, {wht, "-10.00", nil}}, "[W1 invoice 10000 1000 W2 payment 11900 -]"},
		{"credit memo", credited["journal_entry_id"], []line{{e.expense.String(), "-100.00", nil}, {e.tax.String(), "-19.00", nil},
			{e.ap.String(), "109.00", nil}, {wht, "10.00", nil}}, "[W1 invoice -10000 -1000 W2 payment -11900 -]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			if got, want := fmt.Sprint(sorted(e.entryLines(t, c.entry))), fmt.Sprint(sorted(c.lines)); got != want {
				t.Errorf("entry lines: got %s, want %s", got, want)
			}
			if got := kept(t, c.entry); got != c.items {
				t.Errorf("withholding kept: got %s, want %s", got, c.items)
			}
		})
	}
	t.Run("database failures", func(t *testing.T) {
		e := e.in(t)
		v, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-F", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post with withholding", func(ctx context.Context) error {
			_, err := endpoints(nil).PostSupplierInvoice(ctx, gen.OptSupplierInvoicePost{}, gen.PostSupplierInvoiceParams{ID: uuid.MustParse(id(v)), IfMatch: "*"})
			return err
		})
	})
	t.Run("a code the legal entity no longer has", func(t *testing.T) {
		e := e.in(t)
		pgtest.Exec(t, `UPDATE ledger.withholding_tax_code SET is_active = false, version = version + 1 WHERE tenant_id = $1 AND code = 'W2'`, e.Tenant.ID)
		defer pgtest.Exec(t, `UPDATE ledger.withholding_tax_code SET is_active = true, version = version + 1 WHERE tenant_id = $1 AND code = 'W2'`, e.Tenant.ID)
		v, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
		e.Problem(422, "payables.withholding_tax_code_unknown", "POST", "/v1/supplier-invoices/"+id(v)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(v)))
	})
	t.Run("codes withholding the whole invoice", func(t *testing.T) {
		e := e.in(t)
		e.SupplierWithholding(t, e.Vendor, "W3", "W4")
		v, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "INV-3", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
		e.Problem(422, "payables.withholding_exceeds_invoice", "POST", "/v1/supplier-invoices/"+id(v)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(v)))
	})
}
