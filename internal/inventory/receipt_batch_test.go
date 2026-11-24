package inventory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Batches and serial numbers in a purchase order's goods receipt (S2f1,
// S2g1; ADR-0082 As built S2f1), through the API.

// trackedEnv is orderEnv with the sealant, batch-managed, and the gauge,
// serial-managed, both kept where the bracket is and valued in P1 at a
// moving price of EUR 1.00, and a released order of the supplier.
type trackedEnv struct {
	orderEnv
	sealant, gauge string
	po             uuid.UUID
}

func setupTracked(t *testing.T) trackedEnv {
	t.Helper()
	e := trackedEnv{orderEnv: setupOrdersWith(t, receiving, "inventory.batch.write", "procurement.order.release")}
	e.sealant, e.gauge = e.batchMaterial(t, "Thread sealant"), e.serialMaterial(t, "Pressure gauge")
	e.po = e.trackedOrder(t)
	return e
}

// trackedOrder orders, through the API, and releases: item 1, 30 EA of the
// sealant into S1 of P1; item 2, 5 EA of the gauge there; item 3, 5 EA of
// paper on the expense account; item 4, 10 EA of the bracket into S1.
func (e trackedEnv) trackedOrder(t *testing.T) uuid.UUID {
	t.Helper()
	sealant := e.orderItem(map[string]any{"material_id": e.sealant, "description": "Sealant", "quantity": map[string]any{"value": "30", "unit": "EA"},
		"unit_price": amount("1.00")})
	gauge := e.orderItem(map[string]any{"material_id": e.gauge, "description": "Gauge", "quantity": map[string]any{"value": "5", "unit": "EA"},
		"unit_price": amount("1.00")})
	paper := e.orderItem(map[string]any{"material_id": nil, "description": "Paper", "quantity": map[string]any{"value": "5", "unit": "EA"},
		"unit_price": amount("2.00"), "plant_id": nil, "storage_location_id": nil})
	o, tag := e.Must(201, "POST", orders, mustJSON(t, map[string]any{"legal_entity_id": e.DE, "supplier_id": e.supplier, "currency": "EUR",
		"order_date": "2026-11-01", "items": []any{sealant, gauge, paper, e.orderItem(nil)}}), "")
	got, _ := e.Must(200, "POST", orders+"/"+o["id"].(string)+":release", "", tag)
	if got["status"] != "released" {
		t.Fatalf("the order: got %v, want released", got["status"])
	}
	return uuid.MustParse(o["id"].(string))
}

// tracked is a goods receipt item of the order's item no, of qty EA, with
// fields added by more.
func (e trackedEnv) tracked(no int, qty string, more map[string]any) map[string]any {
	it := map[string]any{"purchase_order_id": e.po, "po_item_no": no, "quantity": map[string]any{"value": qty, "unit": "EA"}}
	for k, v := range more {
		it[k] = v
	}
	return it
}

// trackedReceipt is a goods receipt request of the items.
func (e trackedEnv) trackedReceipt(t *testing.T, items ...map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"legal_entity_id": e.DE, "document_date": e.day, "posting_date": e.day, "items": items})
}

// batchOf is a goods receipt item's batch: its number, and the dates
// when made is not "" (no expiry when expires is "").
func batchOf(number, made, expires string) map[string]any {
	b := map[string]any{"number": number}
	if made != "" {
		b["manufacture_date"] = made
	}
	if expires != "" {
		b["expiry_date"] = expires
	}
	return b
}

// sealantStock is the sealant's stock by batch and by location (byBatch).
func (e trackedEnv) sealantStock(t *testing.T) (string, string) {
	t.Helper()
	return batchEnv{env: e.env, sealant: e.sealant}.byBatch(t)
}

// receiptItems are a goods receipt's items as "item batch serials" each.
func receiptItems(t *testing.T, gr map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, gr, "items") {
		serials, _ := json.Marshal(it["serial_numbers"])
		out = append(out, fmt.Sprintf("%v %v %s", it["po_item_no"], it["batch_number"], serials))
	}
	return strings.Join(out, "; ")
}

// TestReceiptBatches (S2f1): a batch-managed stock item's receipt names
// its batch; a new one is created by the receipt with its dates and
// audited, an existing one is named by its number with its own dates or
// none; the goods receipt, its document and the event name it, stock by
// batch adds up to the location's, and the receipt's reversal takes the
// batch's stock back.
func TestReceiptBatches(t *testing.T) {
	e := setupTracked(t)
	gr, _ := e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(3, "1", nil),
		e.tracked(1, "10", map[string]any{"batch": batchOf("B1", "2026-11-01", "2027-05-01")})), "")
	if got, want := receiptItems(t, gr), "3 <nil> null; 1 B1 null"; got != want {
		t.Errorf("the receipt's items: got %s, want %s", got, want)
	}
	page, _ := e.Must(200, "GET", batches+"?material_id="+e.sealant+"&number=B1", "", "")
	listed := rows(t, page, "items")
	if len(listed) != 1 {
		t.Fatalf("the batch created: got %d, want 1", len(listed))
	}
	b := listed[0]
	if got := fmt.Sprintf("%v %v %v", b["manufacture_date"], b["expiry_date"], b["id"] == rows(t, gr, "items")[1]["batch_id"]); got != "2026-11-01 2027-05-01 true" {
		t.Errorf("the batch: got %s, want made 2026-11-01, expiring 2027-05-01, named by the receipt", got)
	}
	if a := e.audits(t, b["id"].(string), "inventory.batch.create"); len(a) != 1 || a[0]["number"] != "B1" || a[0]["manufacture_date"] != "2026-11-01" {
		t.Errorf("the batch's audit: got %v, want B1's creation", a)
	}
	if got := batchLines(e.document(t, gr)); got != "1:B1:10" {
		t.Errorf("the material document's lines: got %s, want 1:B1:10", got)
	}
	again, _ := e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(1, "5", map[string]any{"batch": batchOf("B1", "", "")}),
		e.tracked(1, "2", map[string]any{"batch": batchOf("B1", "2026-11-01", "2027-05-01")}),
		e.tracked(1, "3", map[string]any{"batch": batchOf("B2", "2026-11-15", "")})), "")
	if got, want := receiptItems(t, again), "1 B1 null; 1 B1 null; 1 B2 null"; got != want {
		t.Errorf("the second receipt's items: got %s, want %s", got, want)
	}
	if batched, located := e.sealantStock(t); batched != "S1 B1 17, S1 B2 3" || located != "S1 20" {
		t.Errorf("stock by batch and by location: got %q and %q, want B1 17 and B2 3 adding up to S1's 20", batched, located)
	}
	var payload string
	pgtest.Scan(t, `SELECT payload::text FROM procurement.outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE '%goods_received%'`,
		[]any{e.Tenant.ID, uuid.MustParse(again["id"].(string))}, &payload)
	if !strings.Contains(payload, `"batch_number": "B2"`) || !strings.Contains(payload, `"batch_id": "`+rows(t, again, "items")[2]["batch_id"].(string)) {
		t.Errorf("GoodsReceived: got %s, want its items naming their batches", payload)
	}
	rev, _ := e.Must(200, "POST", "/v1/goods-receipts/"+again["id"].(string)+":reverse", "", e.receiptTag(t, again))
	if got, want := receiptItems(t, rev), "1 B1 null; 1 B1 null; 1 B2 null"; got != want {
		t.Errorf("the reversal's items: got %s, want %s", got, want)
	}
	if batched, located := e.sealantStock(t); batched != "S1 B1 10" || located != "S1 10" {
		t.Errorf("after the reversal: got %q and %q, want B1 back to 10 and B2 at none", batched, located)
	}
}

// TestReceiptBatchRefusals (S2f1): a batch-managed stock item's receipt
// without a batch, a new batch without its manufacture date or expiring
// before it, dates not an existing batch's, and a batch on an item of
// another material or on no stock item are refused, each naming its
// field, and nothing is received.
func TestReceiptBatchRefusals(t *testing.T) {
	e := setupTracked(t)
	e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(1, "1", map[string]any{"batch": batchOf("B1", "2026-11-01", "")})), "")
	for name, c := range map[string]struct {
		items       []map[string]any
		code, field string
	}{
		"no batch": {[]map[string]any{e.tracked(1, "1", nil)}, "inventory.batch_required", "items[0].batch"},
		"a new batch without its manufacture date": {[]map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B9", "", "")})},
			"inventory.batch_manufacture_date_required", "items[0].batch.manufacture_date"},
		"a new batch expiring before it was made": {[]map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B9", "2026-11-02", "2026-11-01")})},
			"inventory.batch_dates", "items[0].batch.expiry_date"},
		"another manufacture date": {[]map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B1", "2026-11-02", "")})},
			"inventory.batch_dates_differ", "items[0].batch.manufacture_date"},
		"an expiry the batch has not": {[]map[string]any{e.tracked(1, "1", map[string]any{"batch": map[string]any{"number": "B1", "expiry_date": "2027-01-01"}})},
			"inventory.batch_dates_differ", "items[0].batch.expiry_date"},
		"one new batch with two manufacture dates": {[]map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B8", "2026-11-03", "")}),
			e.tracked(1, "1", map[string]any{"batch": batchOf("B8", "2026-11-04", "")})}, "inventory.batch_dates_differ", "items[1].batch.manufacture_date"},
		"a batch of the bracket, after no stock item": {[]map[string]any{e.tracked(3, "1", nil), e.tracked(4, "1", map[string]any{"batch": batchOf("B1", "", "")})},
			"inventory.batch_not_allowed", "items[1].batch"},
		"a batch on no stock item": {[]map[string]any{e.tracked(3, "1", map[string]any{"batch": batchOf("B1", "", "")})}, "procurement.not_stock_item",
			"items[0].batch"},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.Problem(422, c.code, "POST", "/v1/goods-receipts", e.trackedReceipt(t, c.items...), "")
			if field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	page, _ := e.Must(200, "GET", batches+"?material_id="+e.sealant, "", "")
	if got := len(rows(t, page, "items")); got != 1 {
		t.Errorf("batches after the refusals: got %d, want B1 alone", got)
	}
	if batched, located := e.sealantStock(t); batched != "S1 B1 1" || located != "S1 1" {
		t.Errorf("stock after the refusals: got %q and %q, want B1's 1 alone", batched, located)
	}
}

// TestReceiptSerialNumbers (S2g1): a serial-managed stock item's receipt
// names the serial number of each unit, each created by it; the goods
// receipt names them, one in stock is not received again, a count not
// matching the quantity and serial numbers on no stock item are refused,
// and the receipt's reversal takes them out of stock again.
func TestReceiptSerialNumbers(t *testing.T) {
	e := setupTracked(t)
	serials := func(numbers ...string) map[string]any { return map[string]any{"serial_numbers": numbers} }
	gr, _ := e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(3, "1", nil), e.tracked(2, "2", serials("G2", "G1"))), "")
	if got, want := receiptItems(t, gr), `3 <nil> null; 2 <nil> ["G1","G2"]`; got != want {
		t.Errorf("the receipt's items: got %s, want %s", got, want)
	}
	gauge := serialEnv{env: e.env, gauge: e.gauge}
	if got := gauge.places(t, ""); got != "G1:S1 G2:S1" {
		t.Errorf("the serial numbers: got %s, want G1 and G2 at S1", got)
	}
	for name, c := range map[string]struct {
		status      int
		items       []map[string]any
		code, field string
	}{
		"one in stock": {409, []map[string]any{e.tracked(2, "1", serials("G1"))}, "inventory.serial_in_stock", ""},
		"fewer than the units, after no stock item": {422, []map[string]any{e.tracked(3, "1", nil), e.tracked(2, "2", serials("G3"))},
			"inventory.serial_required", "items[1].serial_numbers"},
		"none": {422, []map[string]any{e.tracked(2, "1", nil)}, "inventory.serial_required", "items[0].serial_numbers"},
		"serial numbers of the sealant": {422, []map[string]any{e.tracked(1, "1", map[string]any{"batch": batchOf("B1", "2026-11-01", ""),
			"serial_numbers": []string{"G3"}})}, "inventory.serial_not_allowed", "items[0].serial_numbers"},
		"serial numbers on no stock item": {422, []map[string]any{e.tracked(3, "1", serials("G3"))}, "procurement.not_stock_item", "items[0].serial_numbers"},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.Problem(c.status, c.code, "POST", "/v1/goods-receipts", e.trackedReceipt(t, c.items...), "")
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	rev, _ := e.Must(200, "POST", "/v1/goods-receipts/"+gr["id"].(string)+":reverse", "", e.receiptTag(t, gr))
	if got, want := receiptItems(t, rev), `3 <nil> null; 2 <nil> ["G1","G2"]`; got != want {
		t.Errorf("the reversal's items: got %s, want %s", got, want)
	}
	if got := gauge.places(t, ""); got != "G1:none G2:none" {
		t.Errorf("after the reversal: got %s, want G1 and G2 in no stock", got)
	}
	e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(2, "1", serials("G1"))), "")
	if got := gauge.places(t, ""); got != "G1:S1 G2:none" {
		t.Errorf("G1 received again: got %s, want G1 at S1", got)
	}
}

// TestReceiptBatchesAtOnce (S2f1; ADR-0082 As built S2f1, locks): receipts
// creating the same two new batches, named in opposite order, post at
// once without waiting on each other for good, each batch created once;
// a receipt naming one with other dates is refused once the other has
// created it.
func TestReceiptBatchesAtOnce(t *testing.T) {
	e := setupTracked(t)
	x, y := e.tracked(1, "1", map[string]any{"batch": batchOf("BX", "2026-11-01", "")}), e.tracked(1, "1", map[string]any{"batch": batchOf("BY", "2026-11-01", "")})
	// The requests are built here: a test helper fails the test on the
	// test's goroutine alone.
	requests := []string{e.trackedReceipt(t, x, y), e.trackedReceipt(t, y, x)}
	statuses := make([]int, 8)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Go(func() { statuses[i], _, _ = e.Do("POST", "/v1/goods-receipts", requests[i%2], "") })
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 201 {
			t.Errorf("receipt %d: got %d, want 201", i, s)
		}
	}
	if batched, _ := e.sealantStock(t); batched != "S1 BX 8, S1 BY 8" {
		t.Errorf("stock by batch: got %q, want BX and BY 8 each", batched)
	}
	other := []string{e.trackedReceipt(t, e.tracked(1, "1", map[string]any{"batch": batchOf("BZ", "2026-11-01", "")})),
		e.trackedReceipt(t, e.tracked(1, "1", map[string]any{"batch": batchOf("BZ", "2026-11-02", "")}))}
	results := make([]string, 2)
	for i := range results {
		wg.Go(func() {
			s, p, _ := e.Do("POST", "/v1/goods-receipts", other[i], "")
			results[i] = fmt.Sprint(s, " ", p["code"])
		})
	}
	wg.Wait()
	slices.Sort(results)
	if got := strings.Join(results, ", "); got != "201 <nil>, 422 inventory.batch_dates_differ" {
		t.Errorf("BZ with two manufacture dates at once: got %s, want one created and one refused", got)
	}
}

// TestIntercompanyTrackedRefused (S2f1, S2g1): an intercompany stock
// transport order still refuses a batch-managed and a serial-managed
// material's stock item, as its goods issue carries neither yet.
func TestIntercompanyTrackedRefused(t *testing.T) {
	e := setupIntercompanyWith(t, intercompany, "inventory.batch.write")
	for name, m := range map[string]string{"batch-managed": e.batchMaterial(t, "Sealant"), "serial-managed": e.serialMaterial(t, "Gauge")} {
		t.Run(name, func(t *testing.T) {
			got := e.Problem(422, "procurement.invalid_stock_item", "POST", orders, e.icOrder(t, e.sister, e.p3, e.icItem(map[string]any{"material_id": m})), "")
			if field(got) != "items[0].material_id" {
				t.Errorf("field: got %v, want items[0].material_id", field(got))
			}
		})
	}
}

// TestReceiptBatchDatabaseFailures: every database error of a receipt
// creating a batch and naming serial numbers, and of its reversal, is
// answered as an error.
func TestReceiptBatchDatabaseFailures(t *testing.T) {
	e := setupTracked(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	proc := app.Procurement(app.Config{Pages: pages, Now: time.Now})
	gr, _ := e.Must(201, "POST", "/v1/goods-receipts", e.trackedReceipt(t, e.tracked(1, "2", map[string]any{"batch": batchOf("B1", "2026-11-01", "")}),
		e.tracked(2, "1", map[string]any{"serial_numbers": []string{"G1"}})), "")
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	made := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, call := range map[string]func(ctx context.Context) error{
		"receive": func(ctx context.Context) error {
			_, err := proc.PostGoodsReceipt(ctx, &gen.GoodsReceiptCreate{LegalEntityID: e.DE, DocumentDate: day, PostingDate: day,
				Items: []gen.GoodsReceiptItemInput{
					{PurchaseOrderID: e.po, PoItemNo: 1, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, Batch: gen.NewOptGoodsReceiptBatch(gen.GoodsReceiptBatch{
						Number: "B2", ManufactureDate: gen.NewOptDate(made), ExpiryDate: gen.NewOptDate(made.AddDate(1, 0, 0))})},
					{PurchaseOrderID: e.po, PoItemNo: 1, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, Batch: gen.NewOptGoodsReceiptBatch(gen.GoodsReceiptBatch{
						Number: "B1"})},
					{PurchaseOrderID: e.po, PoItemNo: 2, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, SerialNumbers: []string{"G2"}}}},
				gen.PostGoodsReceiptParams{})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := proc.ReverseGoodsReceipt(ctx, gen.OptGoodsReceiptReverse{}, gen.ReverseGoodsReceiptParams{ID: uuid.MustParse(gr["id"].(string)), IfMatch: "*"})
			return err
		},
		"read": func(ctx context.Context) error {
			_, err := proc.GetGoodsReceipt(ctx, gen.GetGoodsReceiptParams{ID: uuid.MustParse(gr["id"].(string))})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call) })
	}
}
