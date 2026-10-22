package payables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// rates converts invoice amounts into the company's currency, as the API
// wires it.
var rates = ledger.Rates{LegalEntities: tenancy.Directory{}}

var perms = []string{"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "payables.invoice.hold",
	"payables.invoice.review", "payables.down_payment.request", "ledger.entry.read", "ledger.entry.post", "ledger.clearing.post"}

type env struct {
	fixture.Env
	expense, tax, ap uuid.UUID // accounts 520000, 140000 and 211000 of company 2000
}

// twoParts is a code of GST and a non-deductible PST: the GST posts on
// its tax account, the PST on the item's line (T61d).
var twoParts = config.TaxCode{Code: "V2", Description: "GST + PST", Components: []config.TaxComponent{
	{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
	{Key: "pst", AccountKey: config.KeyNonDeductible, Rates: []config.TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}}}}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	return env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
}

// in returns e reporting to t, so a subtest's failure stops that subtest
// rather than its parent.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.T = t
	return e
}

// order inserts a released purchase order of the supplier in legal entity
// 2000 (EUR): item 1 is 10 EA at 12.00 with tax code V1, item 2 is 5 KG at
// 3.00 per 1 KG.
func (e env) order(t *testing.T, supplier uuid.UUID, number string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-03-01', 'NT30', 'draft', $6, $6)`,
		e.Tenant.ID, id, number, e.DE, supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 'V1', 1200, 1), ($1, $2, 2, 'Toner', 5, 'KG', 300, 1, $3, NULL, 300, 1)`,
		e.Tenant.ID, id, e.expense)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, id)
	return id
}

// receive posts a goods receipt of the quantity on an order item.
func (e env) receive(t *testing.T, po uuid.UUID, item int, qty string) {
	t.Helper()
	gm := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date, posted_by)
		VALUES ($1, $2, $3, $4, '101', '2026-03-05', '2026-03-05', $5)`, e.Tenant.ID, gm, gm.String(), e.DE, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, $2, 1, $3, $4, $5::numeric, uom_code FROM procurement.purchase_order_item WHERE purchase_order_id = $3 AND item_no = $4`,
		e.Tenant.ID, gm, po, item, qty)
}

// invoice is an invoice body in EUR for legal entity 2000.
func (e env) invoice(supplier uuid.UUID, ref, gross, tax string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q, "invoice_date": "2026-03-10",
		"gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"}, "items": [%s]}`,
		e.DE, supplier, ref, gross, tax, strings.Join(items, ", "))
}

func poItem(po uuid.UUID, item int, qty, unit, amount string) string {
	return fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": %d, "quantity": {"value": %q, "unit": %q}, "amount": {"amount": %q, "currency": "EUR"}}`,
		po, item, qty, unit, amount)
}

func accountItem(account uuid.UUID, amount string) string {
	return fmt.Sprintf(`{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}, "tax_code": "V1"}`,
		account, amount)
}

// untaxedItem is accountItem without a tax code.
func untaxedItem(account uuid.UUID, amount string) string {
	return strings.Replace(accountItem(account, amount), `, "tax_code": "V1"`, "", 1)
}

func id(m map[string]any) string { return m["id"].(string) }

func events(t *testing.T, ten pgtest.Tenant, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, ten, outbox.Event{Context: "payables", Name: name})
}

// tag is the current ETag of an invoice.
func (e env) tag(t *testing.T, invoice string) string {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+invoice, "", "")
	return tag
}

func (e env) act(t *testing.T, code int, invoice, action, body string) map[string]any {
	t.Helper()
	m, _ := e.Must(code, "POST", "/v1/supplier-invoices/"+invoice+":"+action, body, e.tag(t, invoice))
	return m
}

func TestEntryAndEdits(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "160.65", "22.80",
		poItem(po, 1, "10", "EA", "120.00"), poItem(po, 2, "5", "KG", "17.85")), "")
	items := inv["items"].([]any)
	first := items[0].(map[string]any)
	if inv["status"] != "parked" {
		t.Errorf("invoice status: got %v, want parked", inv["status"])
	}
	if inv["source"] != "manual" {
		t.Errorf("invoice source: got %v, want manual", inv["source"])
	}
	if inv["purchase_order_id"] != po.String() {
		t.Errorf("invoice order: got %v, want %v", inv["purchase_order_id"], po)
	}
	if tag != `"1"` {
		t.Errorf("invoice etag: got %s, want \"1\"", tag)
	}
	if got := inv["document_number"].(string); !strings.HasPrefix(got, "SI") {
		t.Errorf("invoice document number: got %q, want prefix SI", got)
	}
	if first["tax_code"] != "V1" {
		t.Errorf("first item tax code: got %v, want V1", first["tax_code"])
	}
	if first["account_id"] != e.expense.String() {
		t.Errorf("first item account: got %v, want %v", first["account_id"], e.expense)
	}
	captured := events(t, e.Tenant, "supplier_invoice_captured")
	if len(captured) != 1 {
		t.Fatalf("captured events: got %d, want 1: %v", len(captured), captured)
	}
	if captured[0]["status"] != "parked" {
		t.Errorf("captured event status: got %v, want parked", captured[0]["status"])
	}
	if captured[0]["capture_sha256"] != nil {
		t.Errorf("captured event hash: got %v, want none", captured[0]["capture_sha256"])
	}
	got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv)+"?expand=supplier", "", "")
	if got["expanded"].(map[string]any)["supplier"].(map[string]any)["number"] != "BP90000001" {
		t.Fatalf("expanded: %v", got)
	}
	e.Problem(409, "payables.duplicate_invoice", "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "inv 1", "1.00", "0.00", accountItem(e.expense, "1.00")), "")

	// Edits replace the items as a whole; removed items come back as new ones.
	upd, _ := e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), fmt.Sprintf(`{"supplier_reference": "INV-1A", "posting_date": "2026-03-12",
		"gross_amount": {"amount": "142.80", "currency": "EUR"}, "items": [%s], "extensions": {"cost_center": "CC1"}}`, poItem(po, 1, "10", "EA", "120.00")), tag)
	if got := len(upd["items"].([]any)); got != 1 {
		t.Errorf("updated items: got %d, want 1", got)
	}
	if upd["supplier_reference"] != "INV-1A" {
		t.Errorf("updated reference: got %v, want INV-1A", upd["supplier_reference"])
	}
	if upd["posting_date"] != "2026-03-12" {
		t.Errorf("updated posting date: got %v, want 2026-03-12", upd["posting_date"])
	}
	if got := upd["extensions"].(map[string]any)["cost_center"]; got != "CC1" {
		t.Errorf("updated cost center: got %v, want CC1", got)
	}
	back, _ := e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), fmt.Sprintf(`{"posting_date": null, "purchase_order_id": null, "invoice_date": "2026-03-11",
		"tax_amount": {"amount": "22.80", "currency": "EUR"}, "items": [%s, %s], "extensions": {"cost_center": null}}`,
		poItem(po, 1, "10", "EA", "120.00"), poItem(po, 2, "5", "KG", "17.85")), `"2"`)
	if got := len(back["items"].([]any)); got != 2 {
		t.Errorf("items back: got %d, want 2", got)
	}
	if back["posting_date"] != nil {
		t.Errorf("cleared posting date: got %v, want none", back["posting_date"])
	}
	if back["purchase_order_id"] != po.String() {
		t.Errorf("order after clearing it: got %v, want %v from the items", back["purchase_order_id"], po)
	}
	if got := back["extensions"].(map[string]any); len(got) != 0 {
		t.Errorf("cleared extensions: got %v, want none", got)
	}
	e.Problem(412, "request.version_mismatch", "PATCH", "/v1/supplier-invoices/"+id(inv), `{"supplier_reference": "X"}`, `"1"`)
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+uuid.NewString(), "", "")

	// An account-based invoice of the other supplier, entered as EDI.
	other, _ := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(e.invoice(e.Vendor, "V-9", "119.00", "19.00", accountItem(e.expense, "100.00")),
		`"items"`, `"source": "edi", "extensions": {"channel": "x400"}, "items"`, 1), "")
	if other["source"] != "edi" {
		t.Errorf("account-based source: got %v, want edi", other["source"])
	}
	if other["purchase_order_id"] != nil {
		t.Errorf("account-based order: got %v, want none", other["purchase_order_id"])
	}

	list := func(q string) []string {
		page, _ := e.Must(200, "GET", "/v1/supplier-invoices?"+q, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["supplier_reference"].(string))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return out
	}
	if got := list("supplier_id=" + e.Vendor.String() + "&status=parked&legal_entity_id=" + e.DE.String() + "&expand=supplier"); len(got) != 1 || got[0] != "V-9" {
		t.Fatalf("by supplier: %v", got)
	}
	if got := list("purchase_order_id=" + po.String() + "&invoice_date_from=2026-03-01&invoice_date_to=2026-03-31"); len(got) != 1 {
		t.Fatalf("by order: %v", got)
	}
	if got := list("document_number=" + other["document_number"].(string)); len(got) != 1 {
		t.Fatalf("by number: %v", got)
	}
	page1 := list("order_by=document_number&page_size=1")
	if len(page1) != 2 {
		t.Fatalf("page 1: %v", page1)
	}
	if rest := list("order_by=document_number&page_size=1&page_token=" + strings.TrimPrefix(page1[1], "next:")); len(rest) != 1 || rest[0] == page1[0] {
		t.Fatalf("page 2: %v", rest)
	}
	if got := list("page_size=1"); len(got) != 2 {
		t.Fatalf("by date: %v", got)
	} else if rest := list("page_size=1&page_token=" + strings.TrimPrefix(got[1], "next:")); len(rest) != 1 {
		t.Fatalf("by date, page 2: %v", rest)
	}

	// Another principal without company access sees nothing.
	svc := e.As(e.Tenant.Service)
	if page, _ := svc.Must(200, "GET", "/v1/supplier-invoices", "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("service sees: %v", page)
	}
	svc.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+id(inv), "", "")
}

func TestEntryRefusals(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	otherPO := e.order(t, e.Vendor, "PO-2")
	draft := e.order(t, e.Supplier, "PO-3")
	// A released order back to draft, which no order goes (T47d), for an
	// order payables meets as a draft.
	pgtest.Unguarded(t, `UPDATE procurement.purchase_order SET status = 'draft', version = version + 1 WHERE id = $1`, draft)
	blocked := e.NewSupplier(t, "BP90000003")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)
	usBank := e.Account(t, "1000", "100000")
	ap := accountItem(e.expense, "1.00")
	e.TaxCodes(t, config.TaxCode{Code: "A1", Description: "Output tax", Direction: config.TaxOutput, RatePercent: "19", Account: "140000"}, twoParts)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown legal entity": {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), e.DE.String(), uuid.NewString(), 1), 422, "request.reference_not_found"},
		"unknown supplier":     {e.invoice(uuid.New(), "R", "1.00", "0.00", ap), 422, "request.reference_not_found"},
		"blocked supplier":     {e.invoice(blocked, "R", "1.00", "0.00", ap), 422, "payables.supplier_not_usable"},
		"bad extension":        {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), `"items"`, `"extensions": {"Bad": 1}, "items"`, 1), 422, "request.invalid_extension"},
		"unknown currency":     {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), `"amount": "1.00", "currency": "EUR"}, "tax`, `"amount": "1.00", "currency": "XXQ"}, "tax`, 1), 422, "payables.unknown_currency"},
		"currency mismatch":    {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), `"amount": "0.00", "currency": "EUR"`, `"amount": "0.00", "currency": "USD"`, 1), 422, "payables.currency_mismatch"},
		"gross decimals":       {e.invoice(e.Supplier, "R", "1.0", "0.00", ap), 400, "request.invalid_amount"},
		"tax decimals":         {e.invoice(e.Supplier, "R", "1.00", "0", ap), 400, "request.invalid_amount"},
		"item decimals":        {e.invoice(e.Supplier, "R", "1.00", "0.00", accountItem(e.expense, "1")), 400, "request.invalid_amount"},
		"quantity":             {e.invoice(e.Supplier, "R", "1.00", "0.00", poItem(po, 1, "0", "EA", "1.00")), 422, "payables.invalid_quantity"},
		"source":               {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), `"items"`, `"source": "capture", "items"`, 1), 422, "payables.unknown_source"},
		"header order":         {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), `"items"`, `"purchase_order_id": "`+otherPO.String()+`", "items"`, 1), 422, "payables.invalid_purchase_order"},
		"draft order":          {e.invoice(e.Supplier, "R", "1.00", "0.00", poItem(draft, 1, "1", "EA", "1.00")), 422, "payables.invalid_purchase_order"},
		"no item number":       {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(poItem(po, 1, "1", "EA", "1.00"), `"po_item_no": 1, `, "", 1)), 422, "payables.po_item_required"},
		"no order":             {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(poItem(po, 1, "1", "EA", "1.00"), `"purchase_order_id": "`+po.String()+`", `, "", 1)), 422, "payables.purchase_order_required"},
		"two orders":           {e.invoice(e.Supplier, "R", "2.00", "0.00", poItem(po, 1, "1", "EA", "1.00"), poItem(draft, 1, "1", "EA", "1.00")), 422, "payables.mixed_purchase_orders"},
		"unknown order item":   {e.invoice(e.Supplier, "R", "1.00", "0.00", poItem(po, 9, "1", "EA", "1.00")), 422, "payables.unknown_po_item"},
		"unit":                 {e.invoice(e.Supplier, "R", "1.00", "0.00", poItem(po, 1, "1", "KG", "1.00")), 422, "payables.unit_mismatch"},
		"no account":           {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(ap, `"account_id": "`+e.expense.String()+`", `, "", 1)), 422, "payables.account_required"},
		"account":              {e.invoice(e.Supplier, "R", "1.00", "0.00", accountItem(usBank, "1.00")), 422, "payables.account_not_postable"},
		"reconciliation":       {e.invoice(e.Supplier, "R", "1.00", "0.00", accountItem(e.ap, "1.00")), 422, "payables.account_not_postable"},
		"tax code":             {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(ap, `"V1"`, `"V7"`, 1)), 422, "payables.unknown_tax_code"},
		"output tax code":      {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(ap, `"V1"`, `"A1"`, 1)), 422, "payables.unknown_tax_code"},
		"unit unknown":         {e.invoice(e.Supplier, "R", "1.00", "0.00", strings.Replace(ap, `"unit": "EA"`, `"unit": "XX"`, 1)), 422, "payables.unknown_unit"},
		"no fiscal year":       {strings.Replace(e.invoice(e.Supplier, "R", "1.00", "0.00", ap), "2026-03-10", "2031-03-10", 1), 422, "ledger.no_fiscal_period"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			code, m, _ := api.Do("POST", "/v1/supplier-invoices", c.body, "")
			if code != c.status {
				t.Errorf("status: got %d, want %d: %v", code, c.status, m)
			}
			if m["code"] != c.code {
				t.Errorf("problem code: got %v, want %s", m["code"], c.code)
			}
		})
	}
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "R", "1.00", "0.00", ap), "")
	e.Problem(422, "payables.account_not_postable", "PATCH", "/v1/supplier-invoices/"+id(inv), `{"items": [`+accountItem(usBank, "1.00")+`]}`, `"1"`)
	e.act(t, 200, id(inv), "hold", `{"reason": "Check"}`)
	e.Problem(409, "payables.invalid_status", "PATCH", "/v1/supplier-invoices/"+id(inv), `{"supplier_reference": "Q"}`, `"2"`)
}
