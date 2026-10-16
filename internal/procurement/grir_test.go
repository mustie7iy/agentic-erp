package procurement_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestOrderValuation: an order is valued when it is released, through
// GR/IR when its legal entity has a GR/IR account then and directly
// otherwise, and keeps that valuation; an order a migration run loads is
// valued directly whatever the account determination says, as its
// accounting comes from the loaded journal (T33).
func TestOrderValuation(t *testing.T) {
	e := setup(t)
	before := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	if before["valuation"] != "direct" {
		t.Errorf("draft valuation: got %v, want direct", before["valuation"])
	}
	if got := e.act(t, "/v1/purchase-orders/"+id(before), "release", ""); got["valuation"] != "direct" {
		t.Errorf("released before GR/IR: got %v, want direct", got["valuation"])
	}
	e.GRIR(t)
	after := e.act(t, "/v1/purchase-orders/"+id(e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")), "release", "")
	if after["valuation"] != "gr_ir" {
		t.Errorf("released with GR/IR: got %v, want gr_ir", after["valuation"])
	}
	if got, _ := e.Must(200, "GET", "/v1/purchase-orders/"+id(before), "", ""); got["valuation"] != "direct" {
		t.Errorf("released before GR/IR, read again: got %v, want direct", got["valuation"])
	}
	released := events(t, e, "purchase_order_released")
	if got := released[len(released)-1]["valuation"]; got != "gr_ir" {
		t.Errorf("PurchaseOrderReleased valuation: got %v, want gr_ir", got)
	}
	loaded := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	var o procurement.PurchaseOrder
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		o, err = endpoints().ReleaseLoadedOrder(ctx, uuid.MustParse(id(loaded)), func(int32) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.Valuation != procurement.ValuationDirect {
		t.Errorf("loaded order: got %v, want direct", o.Valuation)
	}
}

// entryLine is a posted line as the receipt tests read it.
type entryLine struct {
	account  uuid.UUID
	tc, lc   int64
	quantity *string
	order    *uuid.UUID
}

// entryOf reads a journal entry's document type, source and lines.
func (e env) entryOf(t *testing.T, id any) (string, string, uuid.UUID, []entryLine) {
	t.Helper()
	var docType, source string
	var doc uuid.UUID
	admin := pgtest.Admin(t)
	if err := admin.QueryRow(context.Background(), `SELECT document_type, source_context, source_document_id FROM ledger.journal_entry WHERE id = $1`, id).
		Scan(&docType, &source, &doc); err != nil {
		t.Fatalf("read entry %v: %v", id, err)
	}
	rows, _ := admin.Query(context.Background(), `SELECT account_id, amount_tc, amount_lc, trim_scale(quantity)::text, purchase_order_id FROM ledger.journal_line
		WHERE journal_entry_id = $1 ORDER BY line_no`, id)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (entryLine, error) {
		var l entryLine
		return l, r.Scan(&l.account, &l.tc, &l.lc, &l.quantity, &l.order)
	})
	if err != nil {
		t.Fatalf("read lines of %v: %v", id, err)
	}
	return docType, source, doc, lines
}

// grirOrder is a released EUR order of 10 EA at 2.50 on 520000, valued
// through GR/IR.
func (e env) grirOrder(t *testing.T, currency string) string {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": %q, "order_date": "2026-03-10",
		"items": %s}`, e.DE, e.Supplier, currency, items(item(e.office, "10", "EA", "2.50", currency, ""))), "")
	if got := e.act(t, "/v1/purchase-orders/"+id(o), "release", ""); got["valuation"] != "gr_ir" {
		t.Fatalf("order valuation: got %v, want gr_ir", got["valuation"])
	}
	return id(o)
}

// TestReceiptsPostGRIR: a receipt on an order valued through GR/IR posts
// a WE entry, Dr the order item's account / Cr GR/IR at the order price,
// the GR/IR line naming the order item with the quantity received
// negated; its reversal posts its own WE entry the other way; a receipt on
// a directly valued order posts nothing.
func TestReceiptsPostGRIR(t *testing.T) {
	e := setup(t)
	direct := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	e.act(t, "/v1/purchase-orders/"+id(direct), "release", "")
	e.GRIR(t)
	grir, office := e.Account(t, "2000", "219000"), uuid.MustParse(e.office)
	order := e.grirOrder(t, "EUR")
	code, gr := e.receipt(t, order, "1:4:EA")
	if code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	if gr["journal_entry_year"] != float64(2026) {
		t.Errorf("receipt journal_entry_year: got %v, want 2026", gr["journal_entry_year"])
	}
	docType, source, doc, lines := e.entryOf(t, gr["journal_entry_id"])
	if got := [3]any{docType, source, doc.String()}; got != [3]any{"WE", "procurement", id(gr)} {
		t.Errorf("receipt entry: got %v, want WE by procurement for the receipt", got)
	}
	o := uuid.MustParse(order)
	minus4, plus4 := "-4", "4"
	want := []entryLine{{grir, -1000, -1000, &minus4, &o}, {office, 1000, 1000, nil, nil}}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("receipt lines: got %+v, want %+v", lines, want)
	}
	received := events(t, e, "goods_received")
	if got := received[len(received)-1]["journal_entry_id"]; got != gr["journal_entry_id"] {
		t.Errorf("GoodsReceived journal_entry_id: got %v, want %v", got, gr["journal_entry_id"])
	}
	rev := e.act(t, "/v1/goods-receipts/"+id(gr), "reverse", "")
	docType, _, doc, lines = e.entryOf(t, rev["journal_entry_id"])
	if got := [2]any{docType, doc.String()}; got != [2]any{"WE", id(rev)} {
		t.Errorf("reversal entry: got %v, want WE for the reversal", got)
	}
	want = []entryLine{{grir, 1000, 1000, &plus4, &o}, {office, -1000, -1000, nil, nil}}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("reversal lines: got %+v, want %+v", lines, want)
	}
	reversed := events(t, e, "goods_receipt_reversed")
	if got := reversed[len(reversed)-1]["journal_entry_id"]; got != rev["journal_entry_id"] {
		t.Errorf("GoodsReceiptReversed journal_entry_id: got %v, want %v", got, rev["journal_entry_id"])
	}
	if code, gr := e.receipt(t, id(direct), "1:1:EA"); code != 201 || gr["journal_entry_id"] != nil {
		t.Errorf("receipt on a direct order: got %d with entry %v, want 201 without one", code, gr["journal_entry_id"])
	}
}

// TestReceiptsRefusedByTheLedger: a receipt on an order valued through
// GR/IR is posted with its entry or not at all: in a closed period it is
// refused and leaves no movement; items of GR/IR orders in two currencies
// cannot share its one entry.
func TestReceiptsRefusedByTheLedger(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	eur, usd := e.grirOrder(t, "EUR"), e.grirOrder(t, "USD")
	count := func() int {
		var n int
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM procurement.goods_movement WHERE tenant_id = $1`, e.Tenant.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID, e.DE)
	if code, m := e.receipt(t, eur, "1:1:EA"); code != 409 || m["code"] != "ledger.period_not_open" {
		t.Errorf("closed period: got %d %v, want 409 ledger.period_not_open", code, m["code"])
	}
	if n := count(); n != 0 {
		t.Errorf("movements after the refusal: got %d, want 0", n)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID, e.DE)
	parts := fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}, {"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}`, eur, usd)
	e.Problem(422, "procurement.mixed_currencies", "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-15",
		"posting_date": "2026-03-15", "items": [%s]}`, e.DE, parts), "")
}

// TestGRIRReceiptFailures: every statement of a receipt and of a reversal
// that post WE entries reports its failure.
func TestGRIRReceiptFailures(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	order := uuid.MustParse(e.grirOrder(t, "EUR"))
	_, gr := e.receipt(t, order.String(), "1:4:EA")
	for name, call := range map[string]func(ctx context.Context) error{
		"receipt": func(ctx context.Context) error {
			_, err := endpoints().PostReceipt(ctx, procurement.ReceiptInput{LegalEntityID: e.DE, DocumentDate: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
				PostingDate: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), Items: []procurement.ReceiptItem{{No: 1, PurchaseOrderID: order, POItemNo: 1, Quantity: "1", Unit: "EA"}}})
			return err
		},
		"reversal": func(ctx context.Context) error {
			_, err := endpoints().ReverseGoodsReceipt(ctx, gen.OptGoodsReceiptReverse{}, gen.ReverseGoodsReceiptParams{ID: uuid.MustParse(id(gr)), IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call) })
	}
}

// TestGoodsReceiptBudget checks 01-requirements' budget for a posting (p95
// 200 ms) on a goods receipt that posts its WE entry: five items of an
// order valued through GR/IR whose items already moved 200 times each, so
// the ledger reads positions with history.
func TestGoodsReceiptBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setup(t)
	e.GRIR(t)
	lines := make([]string, 5)
	for i := range lines {
		lines[i] = item(e.office, "100000", "EA", "1.25", "EUR", "")
	}
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10",
		"items": %s}`, e.DE, e.Supplier, items(lines...)), "")
	order := id(e.act(t, "/v1/purchase-orders/"+id(o), "release", ""))
	receive := []string{"1:1:EA", "2:1:EA", "3:1:EA", "4:1:EA", "5:1:EA"}
	for range 200 {
		if code, m := e.receipt(t, order, receive...); code != 201 {
			t.Fatalf("history receipt: %d %v", code, m)
		}
	}
	pgtest.Exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, procurement.goods_movement, procurement.goods_movement_item`)
	var times []time.Duration
	for range 20 {
		start := time.Now()
		if code, m := e.receipt(t, order, receive...); code != 201 {
			t.Fatalf("receipt: %d %v", code, m)
		}
		times = append(times, time.Since(start))
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("goods receipt of 5 GR/IR items: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(200 * time.Millisecond); p95 > budget {
		t.Fatalf("goods receipt p95 %v exceeds the %v budget (200 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// TestReceiptRepeatingAnItem: a receipt may list one order item twice; the
// item's position moves once per line from where the last left it, so it
// is the order price of what is received (0.025 at 1.00 per 3: -0.01), and
// reversing every receipt leaves it zero.
func TestReceiptRepeatingAnItem(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10",
		"items": %s}`, e.DE, e.Supplier, items(item(e.office, "10", "EA", "1.00", "EUR", `, "price_unit": 3`))), "")
	order := id(e.act(t, "/v1/purchase-orders/"+id(o), "release", ""))
	position := func() [3]int64 {
		var p [3]int64
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT coalesce(sum(amount_tc), 0)::bigint, coalesce(sum(amount_lc), 0)::bigint,
			coalesce(sum(amount_gc), 0)::bigint FROM ledger.journal_line WHERE purchase_order_id = $1 AND account_id = $2`, order, e.Account(t, "2000", "219000")).
			Scan(&p[0], &p[1], &p[2]); err != nil {
			t.Fatal(err)
		}
		return p
	}
	_, first := e.receipt(t, order, "1:0.015:EA")
	code, second := e.receipt(t, order, "1:0.005:EA", "1:0.005:EA")
	if code != 201 {
		t.Fatalf("receipt repeating the item: got %d %v, want 201", code, second)
	}
	if got := position(); got != [3]int64{-1, -1, -1} {
		t.Errorf("position after 0.025 received: got %v, want -0.01 in every currency", got)
	}
	e.act(t, "/v1/goods-receipts/"+id(second), "reverse", "")
	e.act(t, "/v1/goods-receipts/"+id(first), "reverse", "")
	if got := position(); got != [3]int64{} {
		t.Errorf("position after reversing both: got %v, want zero", got)
	}
}

// TestReceiptsForeignAndMixed: a receipt on a USD order of a EUR company
// posts its WE entry in USD with local amounts at the receipt date's rate;
// a movement with items of a direct and of a GR/IR order posts only the
// GR/IR order's.
func TestReceiptsForeignAndMixed(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	direct := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	e.act(t, "/v1/purchase-orders/"+id(direct), "release", "")
	e.GRIR(t)
	grir, office := e.Account(t, "2000", "219000"), uuid.MustParse(e.office)
	usd := e.grirOrder(t, "USD")
	_, gr := e.receipt(t, usd, "1:4:EA")
	_, _, _, lines := e.entryOf(t, gr["journal_entry_id"])
	o, minus4 := uuid.MustParse(usd), "-4"
	if want := []entryLine{{grir, -1000, -900, &minus4, &o}, {office, 1000, 900, nil, nil}}; !reflect.DeepEqual(lines, want) {
		t.Errorf("USD receipt lines: got %+v, want %+v", lines, want)
	}
	eur := e.grirOrder(t, "EUR")
	parts := fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}, {"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "2", "unit": "EA"}}`, id(direct), eur)
	mixed, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-15", "posting_date": "2026-03-15",
		"items": [%s]}`, e.DE, parts), "")
	_, _, _, lines = e.entryOf(t, mixed["journal_entry_id"])
	o, minus2 := uuid.MustParse(eur), "-2"
	if want := []entryLine{{grir, -500, -500, &minus2, &o}, {office, 500, 500, nil, nil}}; !reflect.DeepEqual(lines, want) {
		t.Errorf("mixed receipt lines: got %+v, want only the GR/IR order's %+v", lines, want)
	}
}

// TestReceiptsAtTheValuationPrice: an order item is valued at the price it
// was released at, which its goods movements post GR/IR at whatever its
// price says later (T47a): a price change of a released order never
// revalues its open GR/IR position.
func TestReceiptsAtTheValuationPrice(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	grir, office := e.Account(t, "2000", "219000"), uuid.MustParse(e.office)
	order := e.grirOrder(t, "EUR")
	got, _ := e.Must(200, "GET", "/v1/purchase-orders/"+order, "", "")
	it := got["items"].([]any)[0].(map[string]any)
	if v := it["valuation_price"]; fmt.Sprint(v) != "map[amount:2.50 currency:EUR]" {
		t.Errorf("valuation price: got %v, want 2.50 EUR", v)
	}
	if v := it["valuation_price_unit"]; v != float64(1) {
		t.Errorf("valuation price unit: got %v, want 1", v)
	}
	// The price changes after the release, as an order change will (T47f).
	pgtest.Unguarded(t, `UPDATE procurement.purchase_order_item SET unit_price = 400 WHERE purchase_order_id = $1`, order)
	code, gr := e.receipt(t, order, "1:4:EA")
	if code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	_, _, _, lines := e.entryOf(t, gr["journal_entry_id"])
	o, minus4 := uuid.MustParse(order), "-4"
	want := []entryLine{{grir, -1000, -1000, &minus4, &o}, {office, 1000, 1000, nil, nil}}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("receipt after a price change: got %+v, want %+v (4 at 2.50)", lines, want)
	}
}

// movement posts a goods movement of type mt of qty EA of item 1 of order
// with the body's extra fields, as the API answers it.
func (e env) movement(t *testing.T, mt, order, qty, extra string) (int, map[string]any) {
	t.Helper()
	code, m, _ := e.Do("POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-20", "posting_date": "2026-03-20",
		"movement_type": %q%s, "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"}}]}`, e.DE, mt, extra, order, qty), "")
	return code, m
}

// ret posts a return (122) of qty EA of item 1 of order.
func (e env) ret(t *testing.T, order, qty, extra string) (int, map[string]any) {
	t.Helper()
	return e.movement(t, "122", order, qty, extra)
}

// received is the quantity received on item 1 of an order.
func (e env) received(t *testing.T, order string) any {
	t.Helper()
	o, _ := e.Must(200, "GET", "/v1/purchase-orders/"+order, "", "")
	return o["items"].([]any)[0].(map[string]any)["received_quantity"].(map[string]any)["value"]
}

// TestReturnsToTheSupplier: a 122 sends received goods back with its
// reason, posting Dr GR/IR / Cr the item's account at the valuation price,
// on released and closed orders, never more than was received; a 123
// reverses it, never beyond the tolerance (T47b).
func TestReturnsToTheSupplier(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	grir, office := e.Account(t, "2000", "219000"), uuid.MustParse(e.office)
	order := e.grirOrder(t, "EUR")
	if code, gr := e.receipt(t, order, "1:6:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	code, ret := e.ret(t, order, "2", `, "reason": "Damaged in transit"`)
	if code != 201 {
		t.Fatalf("return: got %d %v, want 201", code, ret)
	}
	if ret["movement_type"] != "122" {
		t.Errorf("return movement type: got %v, want 122", ret["movement_type"])
	}
	if ret["reason"] != "Damaged in transit" {
		t.Errorf("return reason: got %v, want Damaged in transit", ret["reason"])
	}
	if got := e.received(t, order); got != "4" {
		t.Errorf("received after the return: got %v, want 4", got)
	}
	_, _, _, lines := e.entryOf(t, ret["journal_entry_id"])
	o, plus2, minus2 := uuid.MustParse(order), "2", "-2"
	if want := []entryLine{{grir, 500, 500, &plus2, &o}, {office, -500, -500, nil, nil}}; !reflect.DeepEqual(lines, want) {
		t.Errorf("return lines: got %+v, want %+v", lines, want)
	}
	returned := events(t, e, "goods_returned")
	if len(returned) != 1 {
		t.Fatalf("GoodsReturned events: got %d, want 1", len(returned))
	}
	if returned[0]["reason"] != "Damaged in transit" {
		t.Errorf("GoodsReturned reason: got %v, want Damaged in transit", returned[0]["reason"])
	}
	for name, c := range map[string]struct {
		mt, qty, extra string
		code           int
		problem        string
	}{
		"a return without a reason": {"122", "1", "", 422, "procurement.reason_required"},
		"more than was received":    {"122", "5", `, "reason": "r"`, 409, "procurement.return_exceeds_receipts"},
		"a reversal asked for":      {"102", "1", "", 422, "procurement.invalid_movement_type"},
		"a receipt with a reason":   {"101", "1", `, "reason": "r"`, 422, "procurement.reason_not_allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			code, m := e.movement(t, c.mt, order, c.qty, c.extra)
			if code != c.code {
				t.Errorf("status: got %d, want %d", code, c.code)
			}
			if m["code"] != c.problem {
				t.Errorf("problem: got %v, want %s", m["code"], c.problem)
			}
		})
	}
	// Its reversal, a 123, takes the goods back in.
	rev := e.act(t, "/v1/goods-receipts/"+id(ret), "reverse", "")
	if rev["movement_type"] != "123" {
		t.Errorf("reversal of a return: got type %v, want 123", rev["movement_type"])
	}
	_, _, _, lines = e.entryOf(t, rev["journal_entry_id"])
	if want := []entryLine{{grir, -500, -500, &minus2, &o}, {office, 500, 500, nil, nil}}; !reflect.DeepEqual(lines, want) {
		t.Errorf("123 lines: got %+v, want %+v", lines, want)
	}
	if got := e.received(t, order); got != "6" {
		t.Errorf("received after the 123: got %v, want 6", got)
	}
	_, tag := e.Must(200, "GET", "/v1/goods-receipts/"+id(rev), "", "")
	e.Problem(409, "procurement.cannot_reverse_reversal", "POST", "/v1/goods-receipts/"+id(rev)+":reverse", "", tag)
	_, tag = e.Must(200, "GET", "/v1/goods-receipts/"+id(ret), "", "")
	e.Problem(409, "procurement.already_reversed", "POST", "/v1/goods-receipts/"+id(ret)+":reverse", "", tag)
	// Closed orders take returns, as goods found faulty later go back.
	e.act(t, "/v1/purchase-orders/"+order, "close", "")
	if code, m := e.ret(t, order, "1", `, "reason": "Faulty"`); code != 201 {
		t.Errorf("return on a closed order: got %d %v, want 201", code, m)
	}
	if code, m := e.receipt(t, order, "1:1:EA"); code != 409 {
		t.Errorf("receipt on a closed order: got %d %v, want 409", code, m)
	}
	// A draft received nothing to return.
	draft := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", "")), "")
	if code, m := e.ret(t, id(draft), "1", `, "reason": "r"`); code != 409 || m["code"] != "procurement.order_not_released" {
		t.Errorf("return on a draft: got %d %v, want 409 procurement.order_not_released", code, m["code"])
	}
}

// TestReturnReversalWithinTheTolerance: a 123 takes goods back in only
// within the order's over-delivery tolerance.
func TestReturnReversalWithinTheTolerance(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	order := e.grirOrder(t, "EUR")
	e.receipt(t, order, "1:10:EA")
	_, ret := e.ret(t, order, "2", `, "reason": "Damaged"`)
	if code, m := e.receipt(t, order, "1:2:EA"); code != 201 {
		t.Fatalf("replacement receipt: got %d %v, want 201", code, m)
	}
	_, tag := e.Must(200, "GET", "/v1/goods-receipts/"+id(ret), "", "")
	e.Problem(409, "procurement.over_delivery", "POST", "/v1/goods-receipts/"+id(ret)+":reverse", "", tag)
}
