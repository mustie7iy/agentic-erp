package procurement_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
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

// berlin is company 2000's postal address in the configuration.
func berlin(doc *config.Config) {
	doc.Company("2000").PostalAddress = &config.PostalAddress{Line1: "Friedrichstrasse 100", Line2: "Wareneingang Tor 2", City: "Berlin",
		PostalCode: "10117", CountryCode: "DE"}
}

func TestOrderDocument(t *testing.T) {
	e := setupWith(t, berlin)
	org := e.orgUnit(t, "purchasing_org", "P1")
	e.address(t, e.Supplier, "legal", "1 Legal Way", "")
	e.address(t, e.Supplier, "order_from", "2 Order Street", "Building B")
	req := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), `, "needed_by": "2026-04-01"`)
	e.act(t, "/v1/requisitions/"+id(req), "submit", "")
	e.Approve(t, id(req))
	chairs := `{"description": "Chairs", "quantity": {"value": "2.5", "unit": "EA"}, "unit_price": {"amount": "470.00", "currency": "EUR"}, "price_unit": 10, "account_id": "` +
		e.office + `", "tax_code": "V1", "delivery_date": "2026-04-15"}`
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
  Friedrichstrasse 100
  Wareneingang Tor 2
  Berlin, 10117
  Country DE
  Purchasing organisation P1 P1

Supplier
  BP90000001 (supplier BP90000001)
  2 Order Street
  Building B
  Springfield, IL, 62701
  Country US

Deliver to
  Acme GmbH
  Friedrichstrasse 100
  Wareneingang Tor 2
  Berlin, 10117
  Country DE

Payment terms: NT30 Net 30 days

Items
  Item 1: Paper
    3 EA at 12.00 EUR per 1 EA
    Amount 36.00 EUR
    Delivery date 2026-04-01
  Item 2: Chairs
    2.5 EA at 470.00 EUR per 10 EA, tax code V1
    Amount 117.50 EUR
    Delivery date 2026-04-15

Total 153.50 EUR

Please quote purchase order %[1]s on your delivery documents and invoices.
`, o["document_number"])
	if body != want {
		t.Errorf("document:\n%s\nwant:\n%s", body, want)
	}

	t.Run("an item without a delivery date", func(t *testing.T) {
		// A mixed order: item 1's date cleared, standing in for an order
		// written before items carried dates (000205) that gained a dated
		// item through a change; item 1 is delivered by the requisition's
		// date.
		e := e.in(t)
		r := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), `, "needed_by": "2026-04-01"`)
		e.act(t, "/v1/requisitions/"+id(r), "submit", "")
		e.Approve(t, id(r))
		m := e.order(t, items(item(e.office, "3", "EA", "12.00", "EUR", ""), chairs), fmt.Sprintf(`, "requisition_id": %q`, id(r)))
		pgtest.Exec(t, `UPDATE procurement.purchase_order_item SET delivery_date = NULL WHERE tenant_id = $1 AND purchase_order_id = $2 AND item_no = 1`, e.Tenant.ID, id(m))
		e.act(t, "/v1/purchase-orders/"+id(m), "release", "")
		_, body, _ := e.document(t, id(m), e.Tenant.Human)
		for _, s := range []string{"Needed by: 2026-04-01, for the items without a delivery date of their own\n",
			"    Amount 36.00 EUR\n  Item 2: Chairs\n", "    Amount 117.50 EUR\n    Delivery date 2026-04-15\n"} {
			if !strings.Contains(body, s) {
				t.Errorf("document: got\n%s\nwant it to contain %q", body, s)
			}
		}
	})
	t.Run("a changed delivery date", func(t *testing.T) {
		// Nothing caches the document: a released order's delivery date
		// changes only through an order change, which the next revision
		// prints.
		e := e.in(t)
		paper := item(e.office, "3", "EA", "12.00", "EUR", "")
		if ch := e.submitted(t, path, items(paper, strings.Replace(chairs, "2026-04-15", "2026-04-22", 1))); ch["status"] != "applied" {
			t.Fatalf("the change: got %v, want applied", ch["status"])
		}
		_, body, _ := e.document(t, id(o), e.Tenant.Human)
		for _, s := range []string{"Revision 2 (replaces revision 1)", "    Amount 117.50 EUR\n    Delivery date 2026-04-22\n"} {
			if !strings.Contains(body, s) {
				t.Errorf("document: got\n%s\nwant it to contain %q", body, s)
			}
		}
	})
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
		// No postal address configured for the buyer: none is invented.
		"Buyer\n  Acme GmbH (company 2000)\n  Country DE (no postal address configured)\n\nSupplier\n",
		"\nDeliver to\n  No delivery address held: ask the buyer where to deliver.\n\nPayment terms",
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
	procurement.Settings
	what string
	err  error
}

// Document fails as settings, and as company holds no company at all.
func (g gone) Document(ctx context.Context) (*config.Config, int32, error) {
	switch g.what {
	case "settings":
		return nil, 0, g.err
	case "company":
		return &config.Config{}, 1, nil
	}
	return g.Settings.Document(ctx)
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
	render := func(t *testing.T, what string, cause error) (string, error) {
		t.Helper()
		var body string
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			h := endpoints()
			g := gone{LegalEntities: h.LegalEntities, Suppliers: h.Suppliers, Settings: h.Settings, what: what, err: cause}
			h.LegalEntities, h.Suppliers, h.Settings = g, g, g
			var err error
			body, err = h.OrderDocument(ctx, uuid.MustParse(id(o)))
			return err
		})
		return body, err
	}
	for _, what := range []string{"entity", "org", "supplier", "term", "settings"} {
		for _, cause := range []error{nil, broken} {
			if what == "settings" && cause == nil {
				continue // Settings.Document answers config.not_bootstrapped, never nothing
			}
			t.Run(fmt.Sprintf("%s %v", what, cause), func(t *testing.T) {
				_, err := render(t, what, cause)
				if err == nil {
					t.Fatal("got no error, want one")
				}
				if cause != nil && !errors.Is(err, broken) {
					t.Errorf("error: got %v, want it to wrap %v", err, broken)
				}
			})
		}
	}
	t.Run("a company the configuration does not hold", func(t *testing.T) {
		// Its address is not configured, which the document says.
		body, err := render(t, "company", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := "  Country DE (no postal address configured)\n"; !strings.Contains(body, want) {
			t.Errorf("document: got\n%s\nwant it to contain %q", body, want)
		}
	})
}
