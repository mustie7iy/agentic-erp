package procurement_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// orgUnit inserts an active org unit of company 2000.
func (e env) orgUnit(t *testing.T, kind, code string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.org_unit (tenant_id, id, legal_entity_id, kind, code, name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5, 'active', $6, $6)`, e.Tenant.ID, id, e.DE, kind, code, e.Tenant.Human.ID)
	return id.String()
}

func (e env) order(t *testing.T, its, extra string) map[string]any {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10",
		"items": %s%s}`, e.DE, e.Supplier, its, extra), "")
	return o
}

func (e env) receipt(t *testing.T, order string, lines ...string) (int, map[string]any) {
	t.Helper()
	parts := make([]string, len(lines))
	for i, l := range lines {
		f := strings.Split(l, ":") // item:quantity:unit
		no, qty, unit := f[0], f[1], f[2]
		parts[i] = fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": %s, "quantity": {"value": %q, "unit": %q}}`, order, no, qty, unit)
	}
	code, m, _ := e.Do("POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-15", "posting_date": "2026-03-15",
		"reference": "DN-1", "items": [%s]}`, e.DE, strings.Join(parts, ", ")), "")
	return code, m
}

func TestPurchaseOrders(t *testing.T) {
	e := setup(t)
	org := e.orgUnit(t, "purchasing_org", "P1")
	r := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	rpath := "/v1/requisitions/" + id(r)
	e.act(t, rpath, "submit", "")
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR",
		"order_date": "2026-03-10", "requisition_id": %q, "items": %s}`, e.DE, e.Supplier, id(r), items(item(e.office, "1", "EA", "1.00", "EUR", ""))), "")
	e.Approve(t, id(r))
	o := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", `, "tax_code": "V1"`), item(e.expense, "4", "KG", "2.50", "EUR", `, "over_delivery_pct": "10"`)),
		fmt.Sprintf(`, "purchasing_org_id": %q, "requisition_id": %q, "extensions": {"cost_center": "MKT"}`, org, id(r)))
	path := "/v1/purchase-orders/" + id(o)
	it := o["items"].([]any)
	if o["document_number"] != "PO100000" {
		t.Errorf("created document number: got %v, want PO100000", o["document_number"])
	}
	if o["payment_term_code"] != "NT30" {
		t.Errorf("created payment terms: got %v, want NT30", o["payment_term_code"])
	}
	if got := o["total_amount"].(map[string]any)["amount"]; got != "60.00" {
		t.Errorf("created total: got %v, want 60.00", got)
	}
	if got := it[0].(map[string]any)["over_delivery_pct"]; got != "0" {
		t.Errorf("item 1 over-delivery tolerance: got %v, want 0", got)
	}
	if got := it[1].(map[string]any)["over_delivery_pct"]; got != "10" {
		t.Errorf("item 2 over-delivery tolerance: got %v, want 10", got)
	}
	if got := it[0].(map[string]any)["received_quantity"].(map[string]any)["value"]; got != "0" {
		t.Errorf("item 1 received: got %v, want 0", got)
	}
	if got, _ := e.Must(200, "GET", rpath, "", ""); got["status"] != "ordered" {
		t.Fatalf("requisition: %v", got)
	}
	if ev := events(t, e, "requisition_ordered"); len(ev) != 1 || ev[0]["purchase_order_id"] != id(o) {
		t.Fatalf("ordered event: %v", ev)
	}
	got, tag := e.Must(200, "GET", path+"?expand=supplier", "", "")
	if got["expanded"].(map[string]any)["supplier"].(map[string]any)["id"] != e.Supplier.String() {
		t.Fatalf("expanded: %v", got)
	}
	// Edits re-check the header against the items.
	e.Problem(422, "procurement.currency_mismatch", "PATCH", path, `{"currency": "USD"}`, tag)
	e.Problem(422, "procurement.unknown_payment_term", "PATCH", path, `{"payment_term_code": "NOPE"}`, tag)
	e.Problem(422, "procurement.invalid_org_unit", "PATCH", path, fmt.Sprintf(`{"purchasing_org_id": %q}`, e.orgUnit(t, "plant", "PL1")), tag)
	e.Problem(422, "request.invalid_extension", "PATCH", path, `{"extensions": {"Bad": 1}}`, tag)
	upd, tag := e.Must(200, "PATCH", path, fmt.Sprintf(`{"payment_term_code": "NT60", "supplier_id": %q, "order_date": "2026-03-11", "purchasing_org_id": null,
		"extensions": {"project": "X"}}`, e.Vendor), tag)
	if upd["payment_term_code"] != "NT60" {
		t.Errorf("updated payment terms: got %v, want NT60", upd["payment_term_code"])
	}
	if upd["supplier_id"] != e.Vendor.String() {
		t.Errorf("updated supplier: got %v, want %v", upd["supplier_id"], e.Vendor)
	}
	if upd["purchasing_org_id"] != nil {
		t.Errorf("updated purchasing org: got %v, want none", upd["purchasing_org_id"])
	}
	if n := len(upd["items"].([]any)); n != 2 {
		t.Errorf("updated items: got %d, want 2", n)
	}
	upd, tag = e.Must(200, "PATCH", path, fmt.Sprintf(`{"supplier_id": %q, "items": %s}`, e.Supplier, items(item(e.office, "10", "EA", "5.00", "EUR", ""))), tag)
	if upd["payment_term_code"] != "NT30" {
		t.Errorf("supplier terms: got %v, want NT30", upd["payment_term_code"])
	}
	if n := len(upd["items"].([]any)); n != 1 {
		t.Errorf("items after the supplier change: got %d, want 1", n)
	}
	rel, _ := e.Must(200, "POST", path+":release", "", tag)
	if rel["status"] != "released" {
		t.Fatalf("released: %v", rel)
	}
	if ev := events(t, e, "purchase_order_released"); len(ev) != 1 || len(ev[0]["items"].([]any)) != 1 {
		t.Fatalf("released event: %v", ev)
	}
	e.Problem(409, "procurement.invalid_status", "PATCH", path, `{}`, "*")
	e.Problem(409, "procurement.invalid_status", "POST", path+":release", "", "*")
	// A released order is closed when nothing more is expected: receipts and
	// a second close are refused, the event names the transition.
	other := e.order(t, items(item(e.office, "3", "EA", "5.00", "EUR", "")), "")
	otherPath := "/v1/purchase-orders/" + id(other)
	e.Problem(409, "procurement.invalid_status", "POST", otherPath+":close", "", "*")
	e.act(t, otherPath, "release", "")
	if code, m := e.receipt(t, id(other), "1:1:EA"); code != 201 {
		t.Fatalf("receipt before close: %d %v", code, m)
	}
	closed := e.act(t, otherPath, "close", "")
	if closed["status"] != "closed" {
		t.Fatalf("closed: %v", closed)
	}
	if code, m := e.receipt(t, id(other), "1:1:EA"); code != 409 || m["code"] != "procurement.order_not_released" {
		t.Fatalf("receipt after close: %d %v", code, m)
	}
	e.Problem(409, "procurement.invalid_status", "POST", otherPath+":close", "", "*")
	e.Problem(409, "procurement.invalid_status", "POST", otherPath+":cancel", "", "*")
	ev := events(t, e, "purchase_order_closed")
	if len(ev) != 1 {
		t.Fatalf("closed events: got %d, want 1: %v", len(ev), ev)
	}
	if ev[0]["previous_status"] != "released" {
		t.Errorf("closed event previous status: got %v, want released", ev[0]["previous_status"])
	}
	if ev[0]["status"] != "closed" {
		t.Errorf("closed event status: got %v, want closed", ev[0]["status"])
	}
	if ev[0]["purchase_order_id"] != id(other) {
		t.Errorf("closed event order: got %v, want %s", ev[0]["purchase_order_id"], id(other))
	}
	// Cancelling gives the requisition back.
	can := e.act(t, path, "cancel", "")
	if can["status"] != "cancelled" {
		t.Fatalf("cancelled: %v", can)
	}
	if got, _ := e.Must(200, "GET", rpath, "", ""); got["status"] != "approved" {
		t.Fatalf("requisition after cancel: %v", got)
	}
	e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), fmt.Sprintf(`, "requisition_id": %q`, id(r)))
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR",
		"order_date": "2026-03-10", "requisition_id": %q, "items": %s}`, e.DE, e.Supplier, id(r), items(item(e.office, "1", "EA", "1.00", "EUR", ""))), "")
}

func TestOrderRefusals(t *testing.T) {
	e := setup(t)
	ok := items(item(e.office, "1", "EA", "1.00", "EUR", ""))
	usExpense := e.Account(t, "1000", "520000").String()
	pr := e.requisition(t, ok, "")
	usReq, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "USD", "items": %s}`, e.US,
		items(item(usExpense, "1", "EA", "1.00", "USD", ""))), "")
	e.act(t, "/v1/requisitions/"+id(usReq), "submit", "")
	e.Approve(t, id(usReq))
	body := func(extra, its string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10", "items": %s%s}`, e.DE, e.Supplier, its, extra)
	}
	contract := e.contract(t, e.Supplier.String(), "")
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"requisition":       {body(fmt.Sprintf(`, "requisition_id": %q`, uuid.New()), ok), 422, "request.reference_not_found"},
		"draft requisition": {body(fmt.Sprintf(`, "requisition_id": %q`, id(pr)), ok), 409, "procurement.invalid_status"},
		"other company":     {body(fmt.Sprintf(`, "requisition_id": %q`, id(usReq)), ok), 422, "procurement.requisition_mismatch"},
		"payment terms":     {body(`, "payment_term_code": "NOPE"`, ok), 422, "procurement.unknown_payment_term"},
		"tax code":          {body("", items(item(e.office, "1", "EA", "1.00", "EUR", `, "tax_code": "ZZ"`))), 422, "procurement.unknown_tax_code"},
		"tolerance":         {body("", items(item(e.office, "1", "EA", "1.00", "EUR", `, "over_delivery_pct": "101"`))), 422, "procurement.invalid_tolerance"},
		"contract item":     {body("", items(item(e.office, "1", "EA", "1.00", "EUR", `, "contract_item_no": 1`))), 422, "procurement.invalid_contract_item"},
		"draft contract":    {body(fmt.Sprintf(`, "contract_id": %q`, id(contract)), ok), 422, "procurement.invalid_contract"},
		"unknown contract":  {body(fmt.Sprintf(`, "contract_id": %q`, uuid.New()), ok), 422, "procurement.invalid_contract"},
		"quantity":          {body("", items(item(e.office, "0", "EA", "1.00", "EUR", ""))), 422, "procurement.invalid_quantity"},
		"price":             {body("", items(item(e.office, "1", "EA", "1.00", "USD", ""))), 422, "procurement.currency_mismatch"},
		"item number":       {body("", items(item(e.office, "1", "EA", "1.00", "EUR", `, "item_no": 3`))), 422, "procurement.invalid_item_no"},
		"account":           {body("", items(item(usExpense, "1", "EA", "1.00", "EUR", ""))), 422, "procurement.account_not_postable"},
		"currency":          {strings.Replace(body("", ok), `"currency": "EUR", "order`, `"currency": "XXQ", "order`, 1), 422, "procurement.unknown_currency"},
		"supplier":          {strings.Replace(body("", ok), e.Supplier.String(), uuid.NewString(), 1), 422, "procurement.supplier_not_usable"},
		"legal entity":      {strings.Replace(body("", ok), e.DE.String(), uuid.NewString(), 1), 422, "request.reference_not_found"},
		"extensions":        {body(`, "extensions": {"Bad": 1}`, ok), 422, "request.invalid_extension"},
	} {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			code, m, _ := api.Do("POST", "/v1/purchase-orders", c.body, "")
			if code != c.status {
				t.Errorf("status: got %d, want %d: %v", code, c.status, m)
			}
			if m["code"] != c.code {
				t.Errorf("problem code: got %v, want %s", m["code"], c.code)
			}
		})
	}
	// An unknown requisition is the shared reference refusal: not_found on its field.
	if _, m, _ := e.Do("POST", "/v1/purchase-orders", body(fmt.Sprintf(`, "requisition_id": %q`, uuid.New()), ok), ""); fmt.Sprint(m["errors"]) != "[map[code:not_found field:requisition_id message:no requisition with this id that you can access]]" {
		t.Errorf("unknown requisition field errors: got %v, want not_found on requisition_id", m["errors"])
	}
	// Contract orders take the contract price and consume the contract.
	cpath := "/v1/contracts/" + id(contract)
	e.act(t, cpath, "release", "")
	for name, c := range map[string]string{
		"wrong unit":   items(item(e.office, "1", "KG", "9.00", "EUR", `, "contract_item_no": 1`)),
		"unknown item": items(item(e.office, "1", "EA", "9.00", "EUR", `, "contract_item_no": 7`)),
	} {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			api.Problem(422, "procurement.invalid_contract_item", "POST", "/v1/purchase-orders", body(fmt.Sprintf(`, "contract_id": %q`, id(contract)), c), "")
		})
	}
	if code, m, _ := e.Do("POST", "/v1/purchase-orders", strings.Replace(body(fmt.Sprintf(`, "contract_id": %q`, id(contract)), ok), "2026-03-10", "2027-03-10", 1), ""); code != 422 || m["code"] != "procurement.invalid_contract" {
		t.Errorf("outside validity: %d %v", code, m)
	}
	if code, m, _ := e.Do("POST", "/v1/purchase-orders", strings.Replace(body(fmt.Sprintf(`, "contract_id": %q`, id(contract)), ok), e.Supplier.String(), e.Vendor.String(), 1), ""); code != 422 || m["code"] != "procurement.invalid_contract" {
		t.Errorf("other supplier: %d %v", code, m)
	}
	big := e.order(t, items(item(e.office, "100", "BOX", "1.00", "EUR", `, "contract_item_no": 2`)), fmt.Sprintf(`, "contract_id": %q`, id(contract)))
	if big["total_amount"].(map[string]any)["amount"] != "1500.00" {
		t.Fatalf("contract price: %v", big)
	}
	e.Problem(409, "procurement.contract_exceeded", "POST", "/v1/purchase-orders/"+id(big)+":release", "", "*")
	small := e.order(t, items(item(e.office, "20", "BOX", "1.00", "EUR", `, "contract_item_no": 2`)), fmt.Sprintf(`, "contract_id": %q`, id(contract)))
	e.act(t, "/v1/purchase-orders/"+id(small), "release", "")
	if c, _ := e.Must(200, "GET", cpath, "", ""); c["consumed_value"].(map[string]any)["amount"] != "300.00" {
		t.Fatalf("consumed: %v", c)
	}
	e.act(t, "/v1/purchase-orders/"+id(small), "cancel", "")
	if c, _ := e.Must(200, "GET", cpath, "", ""); c["consumed_value"].(map[string]any)["amount"] != "0.00" {
		t.Fatalf("returned: %v", c)
	}
	if ev := events(t, e, "purchase_order_cancelled"); len(ev) != 1 || ev[0]["released_value"].(map[string]any)["amount"] != "300.00" {
		t.Fatalf("cancelled event: %v", ev)
	}
	// A blocked supplier cannot be released to; a terminated contract cannot be used.
	free := e.order(t, ok, "")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	e.Problem(422, "procurement.supplier_not_usable", "POST", "/v1/purchase-orders/"+id(free)+":release", "", "*")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = false, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	e.act(t, cpath, "terminate", `{"reason": "End"}`)
	e.Problem(422, "procurement.invalid_contract", "POST", "/v1/purchase-orders/"+id(big)+":release", "", "*")
	e.Problem(422, "procurement.invalid_contract", "PATCH", "/v1/purchase-orders/"+id(big), `{}`, "*")
	cleared, _ := e.Must(200, "PATCH", "/v1/purchase-orders/"+id(big), fmt.Sprintf(`{"contract_id": null, "items": %s}`, ok), "*")
	if cleared["contract_id"] != nil {
		t.Fatalf("cleared contract: %v", cleared)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/purchase-orders/"+uuid.NewString(), "", "")
	e.As(e.Tenant.Agent).Problem(404, "request.not_found", "GET", "/v1/purchase-orders/"+id(free), "", "")

	// Orders invoiced in payables cannot be cancelled.
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Invoices = invoiced{yes: true}
		_, err := h.CancelPurchaseOrder(ctx, genCancel(id(free)))
		return err
	})
	pgtest.WantErr(t, err, "procurement.order_invoiced")
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Invoices = invoiced{err: fmt.Errorf("payables down")}
		_, err := h.CancelPurchaseOrder(ctx, genCancel(id(free)))
		return err
	})
	pgtest.WantErr(t, err, "payables down")
}

type invoiced struct {
	yes bool
	err error
}

func (i invoiced) HasInvoices(context.Context, uuid.UUID) (bool, error) { return i.yes, i.err }

var _ procurement.Invoices = invoiced{}

func TestGoodsReceipts(t *testing.T) {
	e := setup(t)
	plant := e.orgUnit(t, "plant", "PL1")
	o := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", ""), item(e.expense, "4", "KG", "2.50", "EUR", `, "over_delivery_pct": "10"`)), "")
	if code, m := e.receipt(t, id(o), "1:1:EA"); code != 409 || m["code"] != "procurement.order_not_released" {
		t.Fatalf("draft order: %d %v", code, m)
	}
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	for name, c := range map[string][]string{
		"over delivery": {"1:6:EA", "1:5:EA"},
		"tolerance":     {"2:4.5:KG"},
		"unit":          {"1:1:KG"},
		"item":          {"9:1:EA"},
		"quantity":      {"1:0:EA"},
	} {
		t.Run(name, func(t *testing.T) {
			code, m := e.receipt(t, id(o), c...)
			want := map[string]string{"over delivery": "procurement.over_delivery", "tolerance": "procurement.over_delivery", "unit": "procurement.unit_mismatch",
				"item": "procurement.unknown_order_item", "quantity": "procurement.invalid_quantity"}[name]
			if m["code"] != want {
				t.Errorf("problem code: got %v, want %s", m["code"], want)
			}
			if code/100 != 4 {
				t.Errorf("status: got %d, want 4xx", code)
			}
		})
	}
	code, gr := e.receipt(t, id(o), "1:6:EA", "2:4.4:KG")
	if code != 201 {
		t.Fatalf("receipt status: got %d, want 201: %v", code, gr)
	}
	if gr["movement_type"] != "101" {
		t.Errorf("receipt movement type: got %v, want 101", gr["movement_type"])
	}
	if gr["document_number"] != "GR100000" {
		t.Errorf("receipt document number: got %v, want GR100000", gr["document_number"])
	}
	if n := len(gr["items"].([]any)); n != 2 {
		t.Errorf("receipt items: got %d, want 2", n)
	}
	// Plants are org units of the legal entity.
	if code, m, _ := e.Do("POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "plant_id": %q, "document_date": "2026-03-15",
		"posting_date": "2026-03-15", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`,
		e.DE, e.orgUnit(t, "purchasing_org", "PO1"), id(o)), ""); code != 422 || m["code"] != "procurement.invalid_org_unit" {
		t.Fatalf("plant: %d %v", code, m)
	}
	second, _, _ := e.Do("POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "plant_id": %q, "document_date": "2026-03-16",
		"posting_date": "2026-03-16", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "4", "unit": "EA"}}]}`, e.DE, plant, id(o)), "")
	if second != 201 {
		t.Fatalf("second receipt: %d", second)
	}
	po, _ := e.Must(200, "GET", "/v1/purchase-orders/"+id(o), "", "")
	if po["items"].([]any)[0].(map[string]any)["received_quantity"].(map[string]any)["value"] != "10" {
		t.Fatalf("received: %v", po)
	}
	e.Problem(409, "procurement.order_received", "POST", "/v1/purchase-orders/"+id(o)+":cancel", "", "*")
	path := "/v1/goods-receipts/" + id(gr)
	got, tag := e.Must(200, "GET", path, "", "")
	if got["reference"] != "DN-1" {
		t.Errorf("get reference: got %v, want DN-1", got["reference"])
	}
	if len(tag) < 10 {
		t.Fatalf("get etag: got %q, want at least 10 characters", tag)
	}
	e.Problem(412, "request.version_mismatch", "POST", path+":reverse", "", `"stale"`)
	rev, _ := e.Must(200, "POST", path+":reverse", `{"posting_date": "2026-03-20"}`, tag)
	if rev["movement_type"] != "102" {
		t.Errorf("reversal movement type: got %v, want 102", rev["movement_type"])
	}
	if rev["reversal_of_id"] != id(gr) {
		t.Errorf("reversal of: got %v, want %s", rev["reversal_of_id"], id(gr))
	}
	if got := rev["items"].([]any)[0].(map[string]any)["quantity"].(map[string]any)["value"]; got != "-6" {
		t.Errorf("reversal quantity: got %v, want -6", got)
	}
	e.Problem(409, "procurement.already_reversed", "POST", path+":reverse", "", "*")
	e.Problem(409, "procurement.cannot_reverse_reversal", "POST", "/v1/goods-receipts/"+id(rev)+":reverse", "", "*")
	e.Problem(404, "request.not_found", "POST", "/v1/goods-receipts/"+uuid.NewString()+":reverse", "", "*")
	if ev := events(t, e, "goods_receipt_reversed"); len(ev) != 1 || ev[0]["goods_receipt_id"] != id(gr) {
		t.Fatalf("reversed event: %v", ev)
	}
	if ev := events(t, e, "goods_received"); len(ev) != 2 {
		t.Fatalf("received events: %v", ev)
	}
	// A reversal never makes the received quantity negative.
	page, _ := e.Must(200, "GET", "/v1/goods-receipts?purchase_order_id="+id(o)+"&order_by=document_number%20desc&page_size=1", "", "")
	latest := page["items"].([]any)[0].(map[string]any)
	if page["next_page_token"] == nil {
		t.Errorf("latest page token: got none, want one")
	}
	if latest["movement_type"] != "102" {
		t.Errorf("latest movement type: got %v, want 102", latest["movement_type"])
	}
	var second2 string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id::text FROM procurement.goods_movement WHERE tenant_id = $1 AND movement_type = '101'
		AND posting_date = '2026-03-16'`, e.Tenant.ID).Scan(&second2); err != nil {
		t.Fatalf("read the second goods receipt: %v", err)
	}
	extra := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date,
		reversal_of_id, posted_by) VALUES ($1, $2, 'X1', $3, '102', '2026-03-17', '2026-03-17', $4, $5)`, e.Tenant.ID, extra, e.DE, uuid.MustParse(id(gr)), e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		VALUES ($1, $2, 1, $3, 1, -2, 'EA')`, e.Tenant.ID, extra, uuid.MustParse(id(o)))
	e.Problem(409, "procurement.reversal_exceeds_receipts", "POST", "/v1/goods-receipts/"+second2+":reverse", "", "*")

	// Lists.
	list := func(q string) (int, string) {
		page, _ := e.Must(200, "GET", "/v1/goods-receipts?"+q, "", "")
		tok, _ := page["next_page_token"].(string)
		return len(page["items"].([]any)), tok
	}
	if n, _ := list(fmt.Sprintf("legal_entity_id=%s&posting_date_from=2026-03-16&posting_date_to=2026-03-20", e.DE)); n != 3 {
		t.Fatalf("by date: %d", n)
	}
	if n, _ := list("document_number=GR100000"); n != 1 {
		t.Fatalf("by number: %d", n)
	}
	n, tok := list("page_size=2")
	if n != 2 {
		t.Errorf("page 1: got %d receipts, want 2", n)
	}
	if tok == "" {
		t.Fatal("page 1: got no next page token, want one")
	}
	if n, _ := list("page_size=2&page_token=" + tok); n != 2 {
		t.Fatalf("page 2: %d", n)
	}
	e.As(e.Tenant.Agent).Problem(404, "request.not_found", "GET", path, "", "")
}

func TestOrderByNumber(t *testing.T) {
	e := setup(t)
	o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		got, ok, err := procurement.Orders{}.OrderByNumber(ctx, e.DE, o["document_number"].(string))
		if err != nil {
			t.Errorf("by number: %v", err)
		}
		if !ok {
			t.Errorf("by number: got not found, want the order")
		}
		if got.String() != id(o) {
			t.Errorf("by number: got %v, want %s", got, id(o))
		}
		_, ok, err = (procurement.Orders{}).OrderByNumber(ctx, e.US, o["document_number"].(string))
		if err != nil {
			t.Errorf("other legal entity: %v", err)
		}
		if ok {
			t.Errorf("other legal entity: got the order, want not found")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := (procurement.Orders{}).OrderByNumber(context.Background(), e.DE, "x"); err == nil {
		t.Error("outside a unit of work")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "order by number", func(ctx context.Context) error {
		_, _, err := procurement.Orders{}.OrderByNumber(ctx, e.DE, "x")
		return err
	})
}

// TestOrderingARequisitionNeedsLevelTwo: an agent at autonomy level 1
// drafts orders, which only propose, but not from a requisition, which
// reserves the requisition for its order (ADR-0036).
func TestOrderingARequisitionNeedsLevelTwo(t *testing.T) {
	e := setup(t)
	r := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.act(t, "/v1/requisitions/"+id(r), "submit", "")
	e.Approve(t, id(r))
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	pgtest.Exec(t, `UPDATE identity.agent_profile SET autonomy_level = 1, version = version + 1 WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, e.Tenant.Agent.ID)
	agent := e.As(e.Tenant.Agent)
	body := func(extra string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10", "items": %s%s}`,
			e.DE, e.Supplier, items(item(e.office, "1", "EA", "1.00", "EUR", "")), extra)
	}
	agent.Problem(403, "identity.autonomy_insufficient", "POST", "/v1/purchase-orders", body(fmt.Sprintf(`, "requisition_id": %q`, id(r))), "")
	agent.Must(201, "POST", "/v1/purchase-orders", body(""), "")
	if got, _ := e.Must(200, "GET", "/v1/requisitions/"+id(r), "", ""); got["status"] != "approved" {
		t.Errorf("requisition: got %v, want approved, not reserved", got["status"])
	}
}
