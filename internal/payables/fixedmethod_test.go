package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withMethods gives company 2000 the payment methods S, active, and X,
// inactive (ADR-0038).
func (e env) withMethods(t *testing.T) {
	t.Helper()
	for code, active := range map[string]bool{"S": true, "X": false} {
		pgtest.Exec(t, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
			VALUES ($1, $2, $3, $3, $4, $5, $5)`, e.Tenant.ID, e.DE, code, active, e.Tenant.Human.ID)
	}
}

// fixing is the invoice body with the payment method it fixes and, for a
// credit memo, its kind.
func fixing(body, method, kind string) string {
	fields := fmt.Sprintf(`"payment_method": %q, `, method)
	if kind != "" {
		fields += fmt.Sprintf(`"kind": %q, `, kind)
	}
	return strings.Replace(body, `"items"`, fields+`"items"`, 1)
}

// openItemMethod is the payment method the supplier's one open item of
// the journal entry fixes, as listOpenItems shows it; nil for none.
func (e env) openItemMethod(t *testing.T, supplier uuid.UUID, entry any) any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplier.String(), "", "")
	for _, it := range page["items"].([]any) {
		if m := it.(map[string]any); m["journal_entry_id"] == entry {
			return m["payment_method"]
		}
	}
	t.Fatalf("open items of %s: got none of entry %v, want one", supplier, entry)
	return nil
}

// TestFixedPaymentMethod: a supplier invoice or credit memo may fix the
// payment method its open item is paid by (ADR-0038, SAP BSEG-ZLSCH;
// product owner 2026-11-10): an active method of its company, set at
// entry and changed or cleared while the document is parked, each change
// under its version and audited; posting keeps it with the open item, and
// the posted document is no longer changed. One that fixes none posts an
// item without one, as before.
func TestFixedPaymentMethod(t *testing.T) {
	e := setup(t)
	e.withMethods(t)
	body := e.invoice(e.Supplier, "INV-S", "119.00", "19.00", accountItem(e.expense, "100.00"))
	for name, method := range map[string]string{"a method the company does not have": "Q", "an inactive method": "X"} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(422, "payables.unknown_payment_method", "POST", "/v1/supplier-invoices", fixing(body, method, ""), "")
		})
	}
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", fixing(body, "S", ""), "")
	if inv["payment_method"] != "S" {
		t.Fatalf("entered payment method: got %v, want S", inv["payment_method"])
	}
	path := "/v1/supplier-invoices/" + id(inv)
	e.Problem(422, "payables.unknown_payment_method", "PATCH", path, `{"payment_method": "X"}`, tag)
	e.Problem(412, "request.version_mismatch", "PATCH", path, `{"payment_method": null}`, `"7"`)
	kept, tag := e.Must(200, "PATCH", path, `{"supplier_reference": "INV-S1"}`, tag)
	if kept["payment_method"] != "S" {
		t.Errorf("payment method after a change of another field: got %v, want S", kept["payment_method"])
	}
	cleared, tag := e.Must(200, "PATCH", path, `{"payment_method": null}`, tag)
	if cleared["payment_method"] != nil {
		t.Errorf("cleared payment method: got %v, want none", cleared["payment_method"])
	}
	set, _ := e.Must(200, "PATCH", path, `{"payment_method": "S"}`, tag)
	if set["payment_method"] != "S" {
		t.Errorf("payment method set again: got %v, want S", set["payment_method"])
	}
	var changes string
	pgtest.Scan(t, `SELECT string_agg(coalesce(detail->'payment_method'->>'from', 'none') || ' to ' || coalesce(detail->'payment_method'->>'to', 'none'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'payables.supplier_invoice.update' AND detail ? 'payment_method'`,
		[]any{e.Tenant.ID, uuid.MustParse(id(inv))}, &changes)
	if want := "S to none; none to S"; changes != want {
		t.Errorf("audited changes of the payment method: got %q, want %q", changes, want)
	}

	posted := e.act(t, 200, id(inv), "post", "")
	if posted["payment_method"] != "S" {
		t.Errorf("posted invoice's payment method: got %v, want S", posted["payment_method"])
	}
	if got := e.openItemMethod(t, e.Supplier, posted["journal_entry_id"]); got != "S" {
		t.Errorf("open item's payment method: got %v, want S", got)
	}
	e.Problem(409, "payables.invalid_status", "PATCH", path, `{"payment_method": null}`, e.tag(t, id(inv)))

	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", fixing(e.invoice(e.Supplier, "CM-S", "11.90", "1.90", accountItem(e.expense, "10.00")), "S", "credit_memo"), "")
	credited := e.act(t, 200, id(memo), "post", "")
	if got := e.openItemMethod(t, e.Supplier, credited["journal_entry_id"]); got != "S" {
		t.Errorf("credit memo's open item, payment method: got %v, want S", got)
	}
	plain, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "INV-P", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	if plain["payment_method"] != nil {
		t.Errorf("payment method of an invoice that fixes none: got %v, want none", plain["payment_method"])
	}
	unfixed := e.act(t, 200, id(plain), "post", "")
	if got := e.openItemMethod(t, e.Vendor, unfixed["journal_entry_id"]); got != nil {
		t.Errorf("open item of an invoice that fixes none, payment method: got %v, want none", got)
	}
}

// TestFixedPaymentMethodForecastAndFailures: an invoice not posted yet is
// read for the liquidity forecast with the method it fixes, as the item
// it would post (Unposted), and entering an invoice that fixes a method
// fails with each of its database statements.
func TestFixedPaymentMethodForecastAndFailures(t *testing.T) {
	e := setup(t)
	e.withMethods(t)
	h := endpoints(nil)
	e.Must(201, "POST", "/v1/supplier-invoices", fixing(e.invoice(e.Supplier, "INV-S", "119.00", "19.00", accountItem(e.expense, "100.00")), "S", ""), "")
	e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "INV-P", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	var items []ledger.PayableItem
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		items, err = h.Unposted(ctx, e.DE)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, it := range items {
		got[it.PartnerID] = "none"
		if it.PaymentMethod != nil {
			got[it.PartnerID] = *it.PaymentMethod
		}
	}
	for supplier, want := range map[uuid.UUID]string{e.Supplier: "S", e.Vendor: "none"} {
		if got[supplier] != want {
			t.Errorf("unposted invoice of %s, fixed method: got %q, want %q", supplier, got[supplier], want)
		}
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "create with a fixed method", func(ctx context.Context) error {
		_, err := h.CreateSupplierInvoice(ctx, &gen.SupplierInvoiceCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, SupplierReference: "NEW",
			InvoiceDate: date("2026-03-10"), GrossAmount: gen.Money{Amount: "15.00", Currency: "EUR"}, TaxAmount: gen.Money{Amount: "0.00", Currency: "EUR"},
			PaymentMethod: gen.NewOptString("S"),
			Items: []gen.SupplierInvoiceItem{{AccountID: gen.NewOptUUID(e.expense), Quantity: gen.Quantity{Value: "1", Unit: "EA"},
				Amount: gen.Money{Amount: "15.00", Currency: "EUR"}}}}, gen.CreateSupplierInvoiceParams{})
		return err
	})
}
