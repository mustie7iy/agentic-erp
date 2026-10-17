package payables_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// term inserts a payment term of the tenant: net days and up to two tiers
// ("NULL" for none).
func (e env) term(t *testing.T, code string, net int, tiers string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, discount2_days, discount2_pct,
		created_by, updated_by) VALUES ($1, $2, $2, $3, `+tiers+`, $4, $4)`, e.Tenant.ID, code, net, e.Tenant.Human.ID)
}

// terms is an invoice's or open item's payment_terms as the API shows
// them, or "none".
func terms(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%v %v %v %v", m["code"], m["baseline_date"], m["net_due_date"], m["discounts"])
}

// TestInvoicesPostWithTheirTerms: an invoice posts with the payment terms
// of its order, or else of its supplier in the legal entity, counted from
// its invoice date, and keeps them, as its open item does (T36b, SAP RBKP
// and BSEG); a correction naming an invoice posts with that invoice's
// terms, one naming none with none (due at once, no discount). A later
// change to the term changes neither, and a term that is not configured
// is refused.
func TestInvoicesPostWithTheirTerms(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	e.term(t, "ZB01", 45, "14, 3, 30, 2")
	e.term(t, "ZB02", 60, "NULL, NULL, NULL, NULL")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_term_code = 'ZB02', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, e.Supplier)
	po := e.grirOrder(t, "PO-1")
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET payment_term_code = 'ZB01', version = version + 1 WHERE id = $1`, po)
	e.receive(t, po, 1, "10")
	onOrder, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	onOrder = e.posted(t, onOrder)
	direct, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "11.90", "1.90", accountItem(e.expense, "10.00")), "")
	e.act(t, 200, id(direct), "post", `{"posting_date": "2026-03-21"}`)
	ref := fmt.Sprintf(`, "reference_invoice_id": %q`, id(onOrder))
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "28.56", "4.56", ref, poItem(po, 1, "2", "EA", "24.00")), "")
	e.posted(t, memo)
	loose, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-2", "11.90", "1.90", "", accountItem(e.expense, "10.00")), "")
	e.act(t, 200, id(loose), "post", `{"posting_date": "2026-03-21"}`)
	pgtest.Exec(t, `UPDATE partner.payment_term SET net_days = 90, version = version + 1 WHERE tenant_id = $1 AND code = 'ZB01'`, e.Tenant.ID)

	orderTerms := "ZB01 2026-03-10 2026-04-24 [map[percent:3 until:2026-03-24] map[percent:2 until:2026-04-09]]"
	for name, c := range map[string]struct{ invoice, want string }{
		"on an order, by the order's term":  {id(onOrder), orderTerms},
		"without an order, by the supplier": {id(direct), "ZB02 2026-03-10 2026-05-09 []"},
		"credit memo naming the invoice":    {id(memo), orderTerms},
		"credit memo naming none":           {id(loose), "none"},
	} {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			v, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+c.invoice, "", "")
			if got := terms(v["payment_terms"]); got != c.want {
				t.Errorf("invoice terms: got %s, want %s", got, c.want)
			}
			page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/open-items?partner_id=%s", e.Supplier), "", "")
			for _, it := range page["items"].([]any) {
				if item := it.(map[string]any); item["journal_entry_id"] == v["journal_entry_id"] {
					if got := terms(item["payment_terms"]); got != c.want {
						t.Errorf("open item terms: got %s, want %s", got, c.want)
					}
				}
			}
		})
	}
	posted := events(t, e.Tenant, "supplier_invoice_posted")
	if got := terms(posted[0]["payment_terms"]); got != orderTerms {
		t.Errorf("SupplierInvoicePosted terms: got %s, want %s", got, orderTerms)
	}
	if got := posted[len(posted)-1]["payment_terms"]; got != nil {
		t.Errorf("SupplierInvoicePosted terms of a memo naming no invoice: got %v, want null", got)
	}

	other := e.order(t, e.Supplier, "PO-2")
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET payment_term_code = 'ZZ99', version = version + 1 WHERE id = $1`, other)
	e.receive(t, other, 1, "10")
	unknown, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-3", "142.80", "22.80", poItem(other, 1, "10", "EA", "120.00")), "")
	e.act(t, 200, id(unknown), "match", "")
	e.Problem(422, "payables.unknown_payment_term", "POST", "/v1/supplier-invoices/"+id(unknown)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(unknown)))

	third := e.grirOrder(t, "PO-3")
	e.receive(t, third, 1, "10")
	failing, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-4", "142.80", "22.80", poItem(third, 1, "10", "EA", "120.00")), "")
	e.act(t, 200, id(failing), "match", "")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post with terms", func(ctx context.Context) error {
		_, err := endpoints(nil).PostInvoice(ctx, uuid.MustParse(id(failing)), func(int32) error { return nil }, nil)
		return err
	})
}
