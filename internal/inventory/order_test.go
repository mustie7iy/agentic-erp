package inventory_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The goods receipt of purchase order items into stock, GR/IR and the
// invoice against valued receipts (S1e; ADR-0082 As built S1e), through
// the API: procurement's goods receipts and payables' invoices move and
// value the stock through inventory's published API.

// receiving gives company 2000 the GR/IR (191000) and price difference
// (590000) accounts beside valued's, and FX accounts, so orders are
// valued through GR/IR.
func receiving(c *config.Config) {
	valued(c)
	de := c.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "191000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense},
		config.Account{Number: "480000", Name: "FX gains", Type: config.Revenue}, config.Account{Number: "690000", Name: "FX losses", Type: config.Expense})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "191000", "590000"
	de.AccountDetermination.FXGain, de.AccountDetermination.FXLoss = "480000", "690000"
}

// orderEnv is env with GR/IR, a supplier, the bolt (standard-priced at
// 1.00 in P1) and the nut (valued nowhere), both kept where the bracket
// is.
type orderEnv struct {
	env
	supplier   uuid.UUID
	bolt, nut  string
	expense    uuid.UUID
	day        string
	receiptSeq *int
}

func setupOrders(t *testing.T) orderEnv {
	t.Helper()
	return setupOrdersWith(t, receiving)
}

// setupOrdersWith is setupOrders under the configuration cfg sets, with
// the permissions more besides.
func setupOrdersWith(t *testing.T, cfg func(*config.Config), more ...string) orderEnv {
	t.Helper()
	ps := slices.Compact(slices.Sorted(slices.Values(append(append(append([]string{"procurement.receipt.post", "procurement.order.create",
		"partner.supplier.read"}, perms...), fixture.InvoicePermissions...), more...))))
	e := orderEnv{env: env{Env: fixture.NewWith(t, nil, cfg, ps...)}, day: "2026-11-02", receiptSeq: new(int)}
	e.p1 = e.orgUnit(t, e.DE, "plant", "P1", nil)
	e.s1 = e.orgUnit(t, e.DE, "storage_location", "S1", &e.p1)
	e.s2 = e.orgUnit(t, e.DE, "storage_location", "S2", &e.p1)
	e.p2 = e.orgUnit(t, e.DE, "plant", "P2", nil)
	e.s3 = e.orgUnit(t, e.DE, "storage_location", "S3", &e.p2)
	e.p3 = e.orgUnit(t, e.US, "plant", "P3", nil)
	e.s4 = e.orgUnit(t, e.US, "storage_location", "S4", &e.p3)
	e.bracket = e.material(t, "Machined bracket M10", "trading_good", true)
	e.value(t, e.bracket, e.p1, "moving_average", "2.00")
	e.bolt = e.material(t, "Hex bolt M10", "trading_good", true)
	e.value(t, e.bolt, e.p1, "standard", "1.00")
	e.nut = e.material(t, "Hex nut M10", "trading_good", true)
	e.c1 = e.CostCenterFrom(t, e.DE, "C1", "2026-01-01", nil)
	e.supplier = e.NewSupplier(t, "S-1")
	e.expense = e.Account(t, "2000", "520000")
	return e
}

// stockOrder inserts a released order of the supplier valued through
// GR/IR in currency: item 1 is 100 EA of the bracket at 2.50 into S1 of
// P1, item 2 10 EA of the bolt at 1.20 there, item 3 5 EA of paper on the
// expense account, item 4 10 EA of the nut at 0.10 into S1.
func (e orderEnv) stockOrder(t *testing.T, number, currency string) uuid.UUID {
	t.Helper()
	return e.stockOrderAt(t, number, currency, 250, 120)
}

// stockOrderAt is stockOrder with the bracket's and the bolt's prices in
// minor units.
func (e orderEnv) stockOrderAt(t *testing.T, number, currency string, bracket, bolt int) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-11-01', 'NT30', 'draft', $7, $7)`,
		e.Tenant.ID, id, number, e.DE, e.supplier, currency, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
		unit_price, price_unit, account_id, valuation_price, valuation_price_unit, plant_id, storage_location_id)
		VALUES ($1, $2, 1, $4, 'Bracket', 100, 'EA', $9, 1, $3, $9, 1, $6, $7), ($1, $2, 2, $5, 'Bolt', 10, 'EA', $10, 1, $3, $10, 1, $6, $7),
			($1, $2, 3, NULL, 'Paper', 5, 'EA', 200, 1, $3, 200, 1, NULL, NULL), ($1, $2, 4, $8, 'Nut', 10, 'EA', 10, 1, $3, 10, 1, $6, $7)`,
		e.Tenant.ID, id, e.expense, e.bracket, e.bolt, e.p1, e.s1, e.nut, bracket, bolt)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, id)
	return id
}

// receipt is a goods receipt request of the order's items (item number
// and quantity in EA, in turn), a return when movementType is 122.
func (e orderEnv) receipt(t *testing.T, po uuid.UUID, movementType string, items ...any) string {
	t.Helper()
	var list []map[string]any
	for i := 0; i < len(items); i += 2 {
		list = append(list, map[string]any{"purchase_order_id": po, "po_item_no": items[i], "quantity": map[string]any{"value": items[i+1], "unit": "EA"}})
	}
	b := map[string]any{"legal_entity_id": e.DE, "document_date": e.day, "posting_date": e.day, "items": list}
	if movementType != "" {
		b["movement_type"], b["reason"] = movementType, "Damaged"
	}
	return mustJSON(t, b)
}

// receive posts a goods receipt and returns it.
func (e orderEnv) receive(t *testing.T, po uuid.UUID, items ...any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/goods-receipts", e.receipt(t, po, "", items...), "")
	return got
}

// invoiced is an invoice item: an order item, the quantity in EA and the
// net amount in EUR.
type invoiced struct {
	no          int
	qty, amount string
}

// invoiceItems are the request's items of the invoiced ones on the order.
func invoiceItems(po uuid.UUID, items []invoiced) []map[string]any {
	list := make([]map[string]any, len(items))
	for i, it := range items {
		list[i] = map[string]any{"purchase_order_id": po, "po_item_no": it.no, "quantity": map[string]any{"value": it.qty, "unit": "EA"},
			"amount": amount(it.amount)}
	}
	return list
}

// invoice enters an invoice of the supplier of kind ("" an invoice),
// naming the invoice it corrects, if any, on the order's items, gross
// without tax, matches it clean and posts it on the day; it answers the
// post's status and body.
func (e orderEnv) invoice(t *testing.T, kind string, reference any, po uuid.UUID, gross string, items ...invoiced) (int, map[string]any) {
	t.Helper()
	*e.receiptSeq++
	b := map[string]any{"legal_entity_id": e.DE, "supplier_id": e.supplier, "supplier_reference": fmt.Sprintf("INV-%d", *e.receiptSeq), "invoice_date": e.day,
		"gross_amount": amount(gross), "tax_amount": amount("0.00"), "items": invoiceItems(po, items)}
	if kind != "" {
		b["kind"] = kind
	}
	if reference != nil {
		b["reference_invoice_id"] = reference
	}
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", mustJSON(t, b), "")
	id := inv["id"].(string)
	if m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id+":match", "", tag); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	_, tag = e.Must(200, "GET", "/v1/supplier-invoices/"+id, "", "")
	code, got, _ := e.Do("POST", "/v1/supplier-invoices/"+id+":post", mustJSON(t, map[string]any{"posting_date": e.day}), tag)
	return code, got
}

// document is the material document a goods receipt names.
func (e orderEnv) document(t *testing.T, gr map[string]any) map[string]any {
	t.Helper()
	id, ok := gr["material_document_id"].(string)
	if !ok {
		t.Fatalf("goods receipt %v names no material document", gr["document_number"])
	}
	d, _ := e.Must(200, "GET", docs+"/"+id, "", "")
	if d["number"] != gr["material_document_number"] {
		t.Errorf("material document number: got %v, want %v", gr["material_document_number"], d["number"])
	}
	return d
}

// values are a document's lines as "material amount difference" each.
func values(t *testing.T, d map[string]any) []string {
	t.Helper()
	var out []string
	for _, l := range rows(t, d, "lines") {
		out = append(out, fmt.Sprintf("%v %v %v %v", l["material_number"], l["quantity"].(map[string]any)["value"], l["amount"].(map[string]any)["amount"],
			l["price_difference"].(map[string]any)["amount"]))
	}
	return out
}

// TestStockReceipt (S1e): an order's stock items are received into stock
// in parts, valued at the order price at a moving price and at the
// standard price with the rest a price difference, against GR/IR; a
// receipt's reversal takes the stock and GR/IR back once; the invoice
// clears GR/IR and puts its price difference on the stock as far as the
// stock still holds the invoiced quantity, the rest and a standard price's
// on the class's price difference account; the stock account's balance is
// the valuations' value throughout, and the invoice is not reversed.
func TestStockReceipt(t *testing.T) {
	e := setupOrders(t)
	po := e.stockOrder(t, "PO-1", "EUR")
	gr1 := e.receive(t, po, 1, "60", 2, "10", 3, "5")
	d1 := e.document(t, gr1)
	if d1["movement_type"] != "101" || d1["reference"] != gr1["document_number"] || d1["journal_entry_id"] != gr1["journal_entry_id"] {
		t.Errorf("goods receipt's document: got %v %v %v, want 101 %v %v", d1["movement_type"], d1["reference"], d1["journal_entry_id"],
			gr1["document_number"], gr1["journal_entry_id"])
	}
	bracket, bolt := e.materialNumber(t, e.bracket), e.materialNumber(t, e.bolt)
	if got, want := fmt.Sprint(values(t, d1)), fmt.Sprintf("[%s 60 150.00 0.00 %s 10 10.00 2.00]", bracket, bolt); got != want {
		t.Errorf("receipt's lines: got %s, want %s", got, want)
	}
	gr2 := e.receive(t, po, 1, "40")
	if got := e.valuation(t, e.bracket, e.p1); got != "100 EA 250.00 2.50/1" {
		t.Errorf("after two receipts: got %s, want 100 EA 250.00 2.50/1", got)
	}
	rev, _ := e.Must(200, "POST", "/v1/goods-receipts/"+gr2["id"].(string)+":reverse", "", e.receiptTag(t, gr2))
	if got, want := fmt.Sprint(values(t, e.document(t, rev))), fmt.Sprintf("[%s -40 -100.00 0.00]", bracket); got != want {
		t.Errorf("reversal's lines: got %s, want %s", got, want)
	}
	e.Problem(409, "procurement.already_reversed", "POST", "/v1/goods-receipts/"+gr2["id"].(string)+":reverse", "", e.receiptTag(t, gr2))
	if got := e.stock(t, e.s1, ""); got != "60 EA" {
		t.Errorf("stock after the reversal: got %s, want 60 EA", got)
	}
	e.receive(t, po, 1, "40")
	e.post(t, "201", e.item("30", "EA", map[string]any{"cost_center_id": e.c1}))
	if got := e.valuation(t, e.bracket, e.p1); got != "70 EA 175.00 2.50/1" {
		t.Errorf("after the issue: got %s, want 70 EA 175.00 2.50/1", got)
	}
	code, inv := e.invoice(t, "", nil, po, "276.50", invoiced{1, "100", "254.00"}, invoiced{2, "10", "12.50"}, invoiced{3, "5", "10.00"})
	if code != 200 {
		t.Fatalf("invoice: got %d %v, want 200", code, inv)
	}
	if got, want := e.valuation(t, e.bracket, e.p1), "70 EA 177.80 2.54/1"; got != want {
		t.Errorf("after the invoice: got %s, want %s", got, want)
	}
	for account, want := range map[string]int64{"150000": 18780, "191000": 0, "517000": 370, "520000": 1000, "590000": 0} {
		if got := e.balance(t, account); got != want {
			t.Errorf("balance of %s: got %d, want %d", account, got, want)
		}
	}
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+inv["id"].(string), "", "")
	e.Problem(409, "payables.invoice_revalued_stock", "POST", "/v1/supplier-invoices/"+inv["id"].(string)+":reverse", `{"reason": "wrong"}`, tag)
	t.Run("corrections", func(t *testing.T) { e.corrections(t, po, inv["id"].(string)) })
}

// corrections: a credit memo takes its price difference off the stock as
// far as the stock holds its quantity, a subsequent debit spreads its
// amount over the quantity received.
func (e orderEnv) corrections(t *testing.T, po uuid.UUID, invoice string) {
	t.Helper()
	if code, got := e.invoice(t, "credit_memo", invoice, po, "24.00", invoiced{1, "10", "24.00"}); code != 200 {
		t.Fatalf("credit memo: got %d %v, want 200", code, got)
	}
	// GR/IR credited 25.00 for 10 EA, the supplier debited 24.00: 1.00 on
	// the stock, which holds all 10.
	if got, want := e.valuation(t, e.bracket, e.p1), "70 EA 178.80 2.55/1"; got != want {
		t.Errorf("after the credit memo: got %s, want %s", got, want)
	}
	if code, got := e.invoice(t, "subsequent_debit", invoice, po, "3.00", invoiced{1, "0", "3.00"}); code != 200 {
		t.Fatalf("subsequent debit: got %d %v, want 200", code, got)
	}
	// 70 of the 100 received are in stock: 2.10 on it, 0.90 a price difference.
	if got, want := e.valuation(t, e.bracket, e.p1), "70 EA 180.90 2.58/1"; got != want {
		t.Errorf("after the subsequent debit: got %s, want %s", got, want)
	}
	if got := e.balance(t, "150000"); got != 19090 {
		t.Errorf("stock account: got %d, want 19090", got)
	}
}

// receiptTag is the goods receipt's ETag now.
func (e orderEnv) receiptTag(t *testing.T, gr map[string]any) string {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/goods-receipts/"+gr["id"].(string), "", "")
	return tag
}

// materialNumber is the material's number.
func (e orderEnv) materialNumber(t *testing.T, id string) string {
	t.Helper()
	m, _ := e.Must(200, "GET", "/v1/materials/"+id, "", "")
	return m["number"].(string)
}

// TestStockReceiptRefused (S1e): a stock item without a valuation is not
// received, nothing of the receipt is; a stock item is not received on
// an order in another currency than the company's (its return, S1e2:
// TestSupplierReturn); a goods
// receipt's document is reversed through its goods receipt alone, and
// only procurement posts a 101.
func TestStockReceiptRefused(t *testing.T) {
	e := setupOrders(t)
	po := e.stockOrder(t, "PO-1", "EUR")
	e.Problem(422, "inventory.no_valuation", "POST", "/v1/goods-receipts", e.receipt(t, po, "", 1, "5", 4, "1"), "")
	order, _ := e.Must(200, "GET", "/v1/purchase-orders/"+po.String(), "", "")
	for _, it := range rows(t, order, "items") {
		if q := it["received_quantity"].(map[string]any)["value"]; q != "0" {
			t.Errorf("item %v received: got %v, want 0", it["item_no"], q)
		}
	}
	gr := e.receive(t, po, 1, "5")
	d := e.document(t, gr)
	_, tag := e.Must(200, "GET", docs+"/"+d["id"].(string), "", "")
	e.Problem(409, "inventory.reverse_goods_receipt", "POST", docs+"/"+d["id"].(string)+":reverse", "", tag)
	e.Problem(422, "inventory.unknown_movement_type", "POST", docs, body(t, "101", e.item("1", "EA", nil)), "")
	usd := e.stockOrder(t, "PO-2", "USD")
	e.Problem(422, "procurement.stock_item_not_valued", "POST", "/v1/goods-receipts", e.receipt(t, usd, "", 1, "1"), "")
}
