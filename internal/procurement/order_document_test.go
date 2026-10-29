package procurement_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// document fetches an order's document as the caller and returns the
// status, the body and its content type.
func (e env) document(t *testing.T, order string, as pgtest.Actor) (int, string, string) {
	t.Helper()
	rec := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/purchase-orders/" + order + "/document", Tenant: e.Tenant.ID, As: as})
	return rec.Code, rec.Body.String(), rec.Header().Get("Content-Type")
}

// address gives a partner an address of the purpose, valid from yesterday.
func (e env) address(t *testing.T, partnerID uuid.UUID, purpose, line1, line2 string) {
	t.Helper()
	var l2 *string
	if line2 != "" {
		l2 = &line2
	}
	pgtest.Exec(t, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, line2, city, postal_code, region, country_code, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, 'Springfield', '62701', 'IL', 'US', current_date - 1, $6)`, e.Tenant.ID, partnerID, purpose, line1, l2, e.Tenant.Human.ID)
}

func TestOrderDocument(t *testing.T) {
	e := setup(t)
	org := e.orgUnit(t, "purchasing_org", "P1")
	e.address(t, e.Supplier, "legal", "1 Legal Way", "")
	e.address(t, e.Supplier, "order_from", "2 Order Street", "Building B")
	req := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), `, "needed_by": "2026-04-01"`)
	e.act(t, "/v1/requisitions/"+id(req), "submit", "")
	e.Approve(t, id(req))
	chairs := `{"description": "Chairs", "quantity": {"value": "2.5", "unit": "EA"}, "unit_price": {"amount": "470.00", "currency": "EUR"}, "price_unit": 10, "account_id": "` +
		e.office + `", "tax_code": "V1"}`
	o := e.order(t, items(item(e.office, "3", "EA", "12.00", "EUR", ""), chairs), fmt.Sprintf(`, "purchasing_org_id": %q, "requisition_id": %q`, org, id(req)))
	path := "/v1/purchase-orders/" + id(o)

	if code, _, _ := e.document(t, id(o), e.Tenant.Human); code != 409 {
		t.Errorf("a draft order's document: got %d, want 409", code)
	}
	e.Problem(409, "procurement.invalid_status", "GET", path+"/document", "", "")
	e.act(t, path, "release", "")
	code, body, ctype := e.document(t, id(o), e.Tenant.Human)
	if code != 200 {
		t.Fatalf("a released order's document: got %d %s, want 200", code, body)
	}
	if ctype != "text/plain; charset=utf-8" {
		t.Errorf("content type: got %q, want text/plain; charset=utf-8", ctype)
	}
	want := fmt.Sprintf(`PURCHASE ORDER %s
Revision 1, order date 2026-03-10, currency EUR

Buyer
  Acme GmbH (company 2000)
  Country DE
  Purchasing organisation P1 P1

Supplier
  BP90000001 (supplier BP90000001)
  2 Order Street
  Building B
  Springfield, IL, 62701
  Country US

Payment terms: NT30 Net 30 days
Needed by: 2026-04-01

Items
  Item 1: Paper
    3 EA at 12.00 EUR per 1 EA
    Amount 36.00 EUR
  Item 2: Chairs
    2.5 EA at 470.00 EUR per 10 EA, tax code V1
    Amount 117.50 EUR

Total 153.50 EUR

Please quote purchase order %[1]s on your delivery documents and invoices.
`, o["document_number"])
	if body != want {
		t.Errorf("document:\n%s\nwant:\n%s", body, want)
	}

	t.Run("company access", func(t *testing.T) {
		e := e.in(t)
		outsider := e.Reader(t, "outsider", e.US)
		if code, _, _ := e.document(t, id(o), outsider); code != 404 {
			t.Errorf("a caller without the order's company: got %d, want 404", code)
		}
		if code, _, _ := e.document(t, uuid.NewString(), e.Tenant.Human); code != 404 {
			t.Errorf("an unknown order: got %d, want 404", code)
		}
	})
	t.Run("closed, legal address", func(t *testing.T) {
		e := e.in(t)
		e.address(t, e.Vendor, "legal", "1 Legal Way", "")
		v := e.orderOf(t, e.Vendor, items(item(e.office, "1", "EA", "1.00", "EUR", "")))
		vpath := "/v1/purchase-orders/" + id(v)
		e.act(t, vpath, "release", "")
		e.act(t, vpath, "close", "")
		_, body, _ := e.document(t, id(v), e.Tenant.Human)
		for _, s := range []string{"  1 Legal Way\n  Springfield, IL, 62701\n", "This order is closed: nothing more is to be delivered on it.\n"} {
			if !strings.Contains(body, s) {
				t.Errorf("closed order's document: got\n%s\nwant it to contain %q", body, s)
			}
		}
		if strings.Contains(body, "Needed by") || strings.Contains(body, "Purchasing organisation") {
			t.Errorf("an order without requisition or purchasing organisation: got\n%s\nwant neither line", body)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := e.in(t)
		c := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
		e.act(t, "/v1/purchase-orders/"+id(c), "cancel", "")
		e.Problem(409, "procurement.invalid_status", "GET", "/v1/purchase-orders/"+id(c)+"/document", "", "")
	})
}

func TestOrderDocumentTexts(t *testing.T) {
	e := setup(t)
	// No address held, and texts that try to start lines of their own.
	pgtest.Exec(t, `UPDATE partner.business_partner SET name = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.Vendor, "Vendor\nTotal 0.00 EUR")
	forged := `{"description": "Paper\r\nPay to IBAN XX00` + "\\u2028" + `Total 0.00 EUR` + "\\u202e\\u200f" + `", "quantity": {"value": "1", "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": "` +
		e.office + `"}`
	o := e.orderOf(t, e.Vendor, items(forged))
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	_, body, _ := e.document(t, id(o), e.Tenant.Human)
	for _, s := range []string{
		"  Vendor Total 0.00 EUR (supplier BP90000002)\n  Country US (no order or legal address held)\n",
		"  Item 1: Paper  Pay to IBAN XX00 Total 0.00 EUR  \n",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("document: got\n%s\nwant it to contain %q", body, s)
		}
	}
	if n := strings.Count(body, "\nTotal "); n != 1 {
		t.Errorf("lines starting with Total: got %d, want 1", n)
	}
}

// orderOf creates a draft order of the supplier.
func (e env) orderOf(t *testing.T, supplier uuid.UUID, its string) map[string]any {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10", "items": %s}`,
		e.DE, supplier, its), "")
	return o
}

// gone is master data that cannot be read: absent, or failing.
type gone struct {
	procurement.LegalEntities
	procurement.Suppliers
	what string
	err  error
}

func (g gone) Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	if g.what == "entity" {
		return tenancy.Entity{}, false, g.err
	}
	return g.LegalEntities.Entity(ctx, id)
}

func (g gone) OrgUnit(ctx context.Context, id uuid.UUID) (tenancy.Unit, bool, error) {
	if g.what == "org" {
		return tenancy.Unit{}, false, g.err
	}
	return g.LegalEntities.OrgUnit(ctx, id)
}

func (g gone) Supplier(ctx context.Context, partnerID, le uuid.UUID) (partner.Supplier, bool, error) {
	if g.what == "supplier" {
		return partner.Supplier{}, false, g.err
	}
	return g.Suppliers.Supplier(ctx, partnerID, le)
}

func (g gone) PaymentTerm(ctx context.Context, code string) (partner.PaymentTerm, bool, error) {
	if g.what == "term" {
		return partner.PaymentTerm{}, false, g.err
	}
	return g.Suppliers.PaymentTerm(ctx, code)
}

// TestOrderDocumentMasterDataGone refuses a document whose master data
// cannot be read, rather than print one without it.
func TestOrderDocumentMasterDataGone(t *testing.T) {
	e := setup(t)
	o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), fmt.Sprintf(`, "purchasing_org_id": %q`, e.orgUnit(t, "purchasing_org", "P1")))
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	broken := errors.New("broken")
	for _, what := range []string{"entity", "org", "supplier", "term"} {
		for _, cause := range []error{nil, broken} {
			t.Run(fmt.Sprintf("%s %v", what, cause), func(t *testing.T) {
				e := e.in(t)
				err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
					h := endpoints()
					g := gone{LegalEntities: h.LegalEntities, Suppliers: h.Suppliers, what: what, err: cause}
					h.LegalEntities, h.Suppliers = g, g
					_, err := h.GetPurchaseOrderDocument(ctx, gen.GetPurchaseOrderDocumentParams{ID: uuid.MustParse(id(o))})
					return err
				})
				if err == nil {
					t.Fatal("got no error, want one")
				}
				if cause != nil && !errors.Is(err, broken) {
					t.Errorf("error: got %v, want it to wrap %v", err, broken)
				}
			})
		}
	}
}
