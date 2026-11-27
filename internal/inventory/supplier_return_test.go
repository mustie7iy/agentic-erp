package inventory_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Returns to the supplier of a purchase order's stock items (S1e2;
// ADR-0082 As built S1e2), through the API: procurement's return (122)
// takes the goods out of stock through inventory's published API, valued
// as the receipt's reversal is, against GR/IR at the order price; its
// reversal (123) puts back what it took.

// received is the order item's received quantity.
func (e orderEnv) received(t *testing.T, po uuid.UUID, no int) string {
	t.Helper()
	o, _ := e.Must(200, "GET", orders+"/"+po.String(), "", "")
	return rows(t, o, "items")[no-1]["received_quantity"].(map[string]any)["value"].(string)
}

// returned posts a return to the supplier of the order's items (item
// number and quantity in EA, in turn) and returns it.
func (e orderEnv) returned(t *testing.T, po uuid.UUID, items ...any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/goods-receipts", e.receipt(t, po, "122", items...), "")
	return got
}

// balances checks company 2000's balances of the accounts.
func (e orderEnv) balances(t *testing.T, want map[string]int64) {
	t.Helper()
	for account, w := range want {
		if got := e.balance(t, account); got != w {
			t.Errorf("balance of %s: got %d, want %d", account, got, w)
		}
	}
}

// TestSupplierReturn (S1e2): a return of stock items takes them out of
// stock as a material document of movement type 122, the moving-priced
// bracket at its value in proportion and the standard-priced bolt at its
// price, against GR/IR at the order price, the rest a price difference;
// the order items' received quantities fall and are received again; the
// return's reversal (123) puts back exactly what it took, once; the stock
// account stays the valuations' value throughout.
func TestSupplierReturn(t *testing.T) {
	e := setupOrders(t)
	po := e.stockOrder(t, "PO-1", "EUR")
	e.receive(t, po, 1, "10", 2, "10")
	// Another 10 brackets worth 30.00 without an order: the moving price
	// is 2.75, above the order's 2.50.
	e.post(t, "501", e.item("10", "EA", map[string]any{"amount": amount("30.00")}))
	ret := e.returned(t, po, 1, "4", 2, "4")
	d := e.document(t, ret)
	if d["movement_type"] != "122" || d["reference"] != ret["document_number"] || d["journal_entry_id"] != ret["journal_entry_id"] {
		t.Errorf("the return's document: got %v %v %v, want 122 %v %v", d["movement_type"], d["reference"], d["journal_entry_id"], ret["document_number"],
			ret["journal_entry_id"])
	}
	bracket, bolt := e.materialNumber(t, e.bracket), e.materialNumber(t, e.bolt)
	// GR/IR is debited 10.00 and 4.80 at the order price: the bracket's
	// 11.00 out of stock leaves 1.00 on the price differences, the bolt's
	// 4.00 at the standard price 0.80 off them.
	if got, want := fmt.Sprint(values(t, d)), fmt.Sprintf("[%s -4 -11.00 1.00 %s -4 -4.00 -0.80]", bracket, bolt); got != want {
		t.Errorf("the return's lines: got %s, want %s", got, want)
	}
	if got, want := e.valuation(t, e.bracket, e.p1)+" | "+e.valuation(t, e.bolt, e.p1), "16 EA 44.00 2.75/1 | 6 EA 6.00 1.00/1"; got != want {
		t.Errorf("the valuations after the return: got %s, want %s", got, want)
	}
	if got := e.received(t, po, 1) + " " + e.received(t, po, 2); got != "6 6" {
		t.Errorf("received after the return: got %s, want 6 6", got)
	}
	e.balances(t, map[string]int64{"150000": 5000, "191000": -2220, "517000": 220})
	if ev := e.events(t, d["id"].(string)); len(ev) != 1 || ev[0]["movement_type"] != "122" || ev[0]["reference"] != ret["document_number"] {
		t.Errorf("MaterialDocumentPosted: got %v, want one of the 122 naming the return", ev)
	}
	rev, _ := e.Must(200, "POST", "/v1/goods-receipts/"+ret["id"].(string)+":reverse", "", e.receiptTag(t, ret))
	if got, want := fmt.Sprint(values(t, e.document(t, rev))), fmt.Sprintf("[%s 4 4.00 0.80 %s 4 11.00 -1.00]", bolt, bracket); rev["movement_type"] != "123" ||
		got != want {
		t.Errorf("the return's reversal: got %v %s, want 123 %s", rev["movement_type"], got, want)
	}
	e.Problem(409, "procurement.already_reversed", "POST", "/v1/goods-receipts/"+ret["id"].(string)+":reverse", "", e.receiptTag(t, ret))
	if got, want := e.valuation(t, e.bracket, e.p1)+" | "+e.valuation(t, e.bolt, e.p1), "20 EA 55.00 2.75/1 | 10 EA 10.00 1.00/1"; got != want {
		t.Errorf("the valuations after the reversal: got %s, want %s", got, want)
	}
	e.balances(t, map[string]int64{"150000": 6500, "191000": -3700, "517000": 200})
	// Four brackets go back, and the supplier sends four more.
	e.returned(t, po, 1, "4")
	if got := e.received(t, po, 1); got != "6" {
		t.Errorf("received after the second return: got %s, want 6", got)
	}
	e.receive(t, po, 1, "4")
	if got := e.received(t, po, 1); got != "10" {
		t.Errorf("received again: got %s, want 10", got)
	}
	if got, want := e.valuation(t, e.bracket, e.p1), "20 EA 54.00 2.70/1"; got != want {
		t.Errorf("the bracket after its return and receipt: got %s, want %s", got, want)
	}
	e.balances(t, map[string]int64{"150000": 6400, "191000": -3700, "517000": 300})
}

// TestSupplierReturnRefused (S1e2): a stock item is returned within what
// was received and is still in stock, and, being a stock item, not below
// what was invoiced until the credit memo; an item that is no stock item
// is returned after its invoice as before. A refusal returns nothing.
func TestSupplierReturnRefused(t *testing.T) {
	e := setupOrders(t)
	po := e.stockOrder(t, "PO-1", "EUR")
	e.receive(t, po, 1, "10", 3, "5")
	e.Problem(409, "procurement.return_exceeds_receipts", "POST", "/v1/goods-receipts", e.receipt(t, po, "122", 1, "11"), "")
	e.post(t, "201", e.item("8", "EA", map[string]any{"cost_center_id": e.c1}))
	e.Problem(409, "inventory.insufficient_stock", "POST", "/v1/goods-receipts", e.receipt(t, po, "122", 1, "4"), "")
	if got := e.received(t, po, 1) + " " + e.stock(t, e.s1, ""); got != "10 2 EA" {
		t.Errorf("after the refusals: got %s, want 10 received and 2 EA in stock", got)
	}
	code, inv := e.invoice(t, "", nil, po, "35.00", invoiced{1, "10", "25.00"}, invoiced{3, "5", "10.00"})
	if code != 200 {
		t.Fatalf("invoice: got %d %v, want 200", code, inv)
	}
	got := e.Problem(409, "procurement.return_below_invoiced", "POST", "/v1/goods-receipts", e.receipt(t, po, "122", 1, "1"), "")
	if detail, _ := got["detail"].(string); !strings.Contains(detail, "9 received of 10 invoiced") {
		t.Errorf("the refusal's detail: got %q, want it to name 9 received of 10 invoiced", detail)
	}
	e.returned(t, po, 3, "1")
	if code, got := e.invoice(t, "credit_memo", inv["id"], po, "5.00", invoiced{1, "2", "5.00"}); code != 200 {
		t.Fatalf("credit memo: got %d %v, want 200", code, got)
	}
	e.returned(t, po, 1, "2")
	e.Problem(409, "procurement.return_below_invoiced", "POST", "/v1/goods-receipts", e.receipt(t, po, "122", 1, "1"), "")
	if got := e.received(t, po, 1) + " " + e.received(t, po, 3) + " in stock " + e.stock(t, e.s1, ""); got != "8 4 in stock " {
		t.Errorf("after the returns: got %s, want 8 and 4 received and no brackets in stock", got)
	}
	// The paper's return waits on GR/IR for its credit memo, as before.
	e.balances(t, map[string]int64{"150000": e.valuationValue(t), "191000": 200})
}

// trackedReturn is a return request of the tracked order's items.
func (e trackedEnv) trackedReturn(t *testing.T, items ...map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"legal_entity_id": e.DE, "document_date": e.day, "posting_date": e.day, "movement_type": "122", "reason": "Damaged",
		"items": items})
}

// TestSupplierReturnTracked (S1e2): a return of a batch-managed stock
// item names the batch it gives back by number, and one of a
// serial-managed item each unit's serial number, in stock at the order
// item's storage location; the return names them, the batch's stock and
// the serial numbers leave the stock, and its reversal puts them back.
func TestSupplierReturnTracked(t *testing.T) {
	e := setupTracked(t)
	serials := func(numbers ...string) map[string]any { return map[string]any{"serial_numbers": numbers} }
	named := func(number string) map[string]any { return map[string]any{"batch": map[string]any{"number": number}} }
	e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(1, "10", map[string]any{"batch": batchOf("B1", "2026-11-01", "")}),
		e.tracked(1, "5", map[string]any{"batch": batchOf("B2", "2026-11-02", "")}), e.tracked(2, "3", serials("G1", "G2", "G3")), e.tracked(3, "1", nil)), "")
	for name, c := range map[string]struct {
		status      int
		items       []map[string]any
		code, field string
	}{
		"no batch":            {422, []map[string]any{e.tracked(1, "1", nil)}, "inventory.batch_required", "items[0].batch"},
		"a batch of none":     {422, []map[string]any{e.tracked(1, "1", named("B9"))}, "inventory.batch_invalid", "items[0].batch"},
		"a batch with dates":  {422, []map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B1", "2026-11-01", "")})}, "procurement.return_batch_dates", "items[0].batch"},
		"more than the batch": {409, []map[string]any{e.tracked(1, "6", named("B2"))}, "inventory.insufficient_stock", ""},
		"no serial numbers":   {422, []map[string]any{e.tracked(2, "1", nil)}, "inventory.serial_required", "items[0].serial_numbers"},
		"a unit never received, after no stock item": {409, []map[string]any{e.tracked(3, "1", nil), e.tracked(2, "1", serials("G9"))},
			"inventory.serial_not_in_stock", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.Problem(c.status, c.code, "POST", "/v1/goods-receipts", e.trackedReturn(t, c.items...), "")
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	gauge := serialEnv{env: e.env, gauge: e.gauge}
	ret, _ := e.Must(201, "POST", "/v1/goods-receipts", e.trackedReturn(t, e.tracked(1, "4", named("B1")), e.tracked(2, "2", serials("G3", "G1"))), "")
	if got, want := receiptItems(t, ret), `1 B1 null; 2 <nil> ["G1","G3"]`; got != want {
		t.Errorf("the return's items: got %s, want %s", got, want)
	}
	if batched, located := e.sealantStock(t); batched != "S1 B1 6, S1 B2 5" || located != "S1 11" {
		t.Errorf("after the return: got %q and %q, want B1 down to 6", batched, located)
	}
	if got := gauge.places(t, ""); got != "G1:none G2:S1 G3:none" {
		t.Errorf("the serial numbers after the return: got %s, want G1 and G3 in no stock", got)
	}
	e.Problem(409, "inventory.serial_not_in_stock", "POST", "/v1/goods-receipts", e.trackedReturn(t, e.tracked(2, "1", serials("G1"))), "")
	rev, _ := e.Must(200, "POST", "/v1/goods-receipts/"+ret["id"].(string)+":reverse", "", e.receiptTag(t, ret))
	if got, want := receiptItems(t, rev), `1 B1 null; 2 <nil> ["G1","G3"]`; got != want {
		t.Errorf("the reversal's items: got %s, want %s", got, want)
	}
	if batched, _ := e.sealantStock(t); batched != "S1 B1 10, S1 B2 5" {
		t.Errorf("after the reversal: got %q, want B1 back to 10", batched)
	}
	if got := gauge.places(t, ""); got != "G1:S1 G2:S1 G3:S1" {
		t.Errorf("the serial numbers after the reversal: got %s, want all at S1", got)
	}
}

// TestSupplierReturnsAtOnce (S1e2; ADR-0082 As built S1e2, locks): returns
// and receipts of the bracket on one order, issues of it to a cost center
// and invoices of another order's brackets post at once, none waiting on
// another for good: a return takes the order items, then the valuation,
// the stock, the ranges and the ledger's locks, as a receipt does.
func TestSupplierReturnsAtOnce(t *testing.T) {
	e := setupOrders(t)
	po, billed := e.stockOrder(t, "PO-1", "EUR"), e.stockOrder(t, "PO-2", "EUR")
	e.receive(t, po, 1, "40")
	e.receive(t, billed, 1, "40")
	invoices := make([]string, 4)
	for i := range invoices {
		invoices[i] = e.parked(t, billed, "25.00", invoiced{1, "10", "25.00"})
	}
	// The requests are built here: a test helper fails the test on the
	// test's goroutine alone.
	receipt, ret := e.receipt(t, po, "", 1, "1"), e.receipt(t, po, "122", 1, "2")
	issue, posting := body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1})), mustJSON(t, map[string]any{"posting_date": e.day})
	var wg sync.WaitGroup
	statuses := make([]int, 16)
	for i := range statuses {
		path, req, ifMatch := "/v1/goods-receipts", receipt, ""
		switch i % 4 {
		case 1:
			req = ret
		case 2:
			path, req = docs, issue
		case 3:
			path, req, ifMatch = "/v1/supplier-invoices/"+invoices[i/4]+":post", posting, "*"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", path, req, ifMatch)
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if want := map[bool]int{true: 200, false: 201}[i%4 == 3]; s != want {
			t.Errorf("posting %d: got %d, want %d", i, s, want)
		}
	}
	if got, want := e.received(t, po, 1)+" "+e.stock(t, e.s1, ""), "36 72 EA"; got != want {
		t.Errorf("received and in stock: got %s, want %s", got, want)
	}
	if got, want := e.balance(t, "150000"), e.valuationValue(t); got != want {
		t.Errorf("stock account: got %d, want the valuation's %d", got, want)
	}
}
