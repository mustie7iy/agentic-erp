package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const orderReleased = "com.example.erp.procurement.purchase_order_released.v1"

// receiptOrder inserts a released purchase order of a supplier of
// company 2000, valued through GR/IR, of 100 EA of the bracket into S1 of
// P1 (item 1, due 2026-12-01), 5 EA of paper on an expense account (item
// 2) and 10 EA of the bracket into S2 of P2 (item 3), and returns it.
func (e wmsEnv) receiptOrder(t *testing.T, number string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	supplier := e.NewSupplier(t, "SUP-"+number[len(number)-4:])
	expense := e.Account(t, "2000", "515000")
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-11-01', 'NT30', 'draft', $6, $6)`,
		e.Tenant.ID, id, number, e.DE, supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
		unit_price, price_unit, account_id, valuation_price, valuation_price_unit, plant_id, storage_location_id, delivery_date)
		VALUES ($1, $2, 1, $4, 'Bracket', 100, 'EA', 250, 1, $3, 250, 1, $5, $6, '2026-12-01'),
			($1, $2, 2, NULL, 'Paper', 5, 'EA', 200, 1, $3, 200, 1, NULL, NULL, NULL),
			($1, $2, 3, $4, 'Bracket', 10, 'EA', 250, 1, $3, 250, 1, $7, $8, NULL)`,
		e.Tenant.ID, id, expense, e.bracket, e.p1, e.s1, e.p2, e.s2)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, id)
	return id
}

// releasedEvent is the order's PurchaseOrderReleased as the relay
// publishes it, with what the queue reads of it.
func (e wmsEnv) releasedEvent(order uuid.UUID, number string) outbox.Message {
	id := uuid.Must(uuid.NewV7())
	data := fmt.Appendf(nil, `{"purchase_order_id": %q, "document_number": %q, "legal_entity_id": %q, "legal_entity_code": "2000",
		"supplier_number": "SUP-1", "status": "released"}`, order, number, e.DE)
	return outbox.Message{ID: id, Subject: "erp.test.procurement." + e.Tenant.ID.String() + ".purchase_order_released", Data: data,
		Header: map[string]string{"ce-specversion": "1.0", "ce-id": id.String(), "ce-type": orderReleased, "ce-subject": order.String(),
			"ce-tenantid": e.Tenant.ID.String(), "ce-actorid": e.Tenant.Human.ID.String(), "Content-Type": "application/json"}}
}

// queuedOrder reads the order's release into the queue.
func (e wmsEnv) queuedOrder(t *testing.T, order uuid.UUID, number string) {
	t.Helper()
	var log pgtest.SyncBuffer
	if err := e.queue(t, &log).Handle(context.Background(), e.releasedEvent(order, number)); err != nil {
		t.Fatalf("the queue: got %v, want nil", err)
	}
}

// orderMessages are the order's messages as listed, newest first.
func (e wmsEnv) orderMessages(t *testing.T, order uuid.UUID, query string) []any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/wms-messages?purchase_order_id="+order.String()+query, "", "")
	items, _ := page["items"].([]any)
	return items
}

// notStock makes the order's item 1 a non-stock item, as an order
// change could.
func (e wmsEnv) notStock(t *testing.T, po uuid.UUID) {
	t.Helper()
	pgtest.Unguarded(t, `UPDATE procurement.purchase_order_item SET material_id = NULL, plant_id = NULL, storage_location_id = NULL
		WHERE tenant_id = $1 AND purchase_order_id = $2 AND item_no = 1`, e.Tenant.ID, po)
}

// goodsReceipt is a goods_receipt confirmation of the order receiving
// "item quantity unit" each.
func goodsReceipt(id string, order uuid.UUID, items ...string) []byte {
	var list []string
	for _, it := range items {
		f := strings.Fields(it)
		list = append(list, fmt.Sprintf(`{"item_no": %s, "quantity": %q, "unit": %q}`, f[0], f[1], f[2]))
	}
	return fmt.Appendf(nil, `{"message_id": %q, "type": "goods_receipt", "purchase_order_id": %q, "items": [%s]}`, id, order, strings.Join(list, ", "))
}

// received is what the order's item has received, net of its movements.
func (e wmsEnv) received(t *testing.T, order uuid.UUID, item int) (qty string, movements int) {
	t.Helper()
	pgtest.Scan(t, `SELECT coalesce(trim_scale(sum(quantity))::text, '0'), count(*) FROM procurement.goods_movement_item
		WHERE tenant_id = $1 AND purchase_order_id = $2 AND po_item_no = $3`, []any{e.Tenant.ID, order, item}, &qty, &movements)
	return qty, movements
}

// TestWMSReceiptRoundTrip: a released purchase order with a stock item at
// a plant the wms connector manages is queued once, however often its
// event is read, and sent once, naming the order, the supplier's number,
// the plant and the items still to receive there (not the non-stock
// item, nor the stock item of another plant); the goods receipt the WMS
// reports is posted once as a 101 of the items it names, naming its
// material document, and acknowledged; reported again, it is
// acknowledged again, not posted twice. A message id longer than a
// goods receipt's reference posts with none.
func TestWMSReceiptRoundTrip(t *testing.T) {
	e := setupWMS(t)
	po := e.receiptOrder(t, "4500000001")
	for range 2 {
		e.queuedOrder(t, po, "4500000001")
	}
	out := e.orderMessages(t, po, "&direction=outbound")
	if len(out) != 1 {
		t.Fatalf("outbound messages of the order: got %d, want 1", len(out))
	}
	m := out[0].(map[string]any)
	for k, want := range map[string]any{"status": "pending", "kind": "inbound_receipt", "plant_code": "P1", "company_code": "2000",
		"purchase_order_id": po.String(), "purchase_order_number": "4500000001", "outbound_delivery_id": nil} {
		if m[k] != want {
			t.Errorf("the queued message's %s: got %v, want %v", k, m[k], want)
		}
	}
	x := e.exchange(t, time.Now)
	if n, err := x.SendOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("send: got %d, %v; want 1 sent", n, err)
	}
	body, sends := e.wms.Received(m["message_id"].(string))
	var sent integration.WMSReceipt
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("what the WMS was sent: %v", err)
	}
	if sends != 1 {
		t.Errorf("sends: got %d, want 1", sends)
	}
	if len(sent.Items) != 1 || sent.Items[0].DeliveryDate == nil {
		t.Fatalf("the items sent: got %+v, want item 1 with its delivery date", sent.Items)
	}
	got := fmt.Sprintf("%s %s v%d %s/%s for %s: %d %s %s %s %s %s", sent.Type, sent.OrderNumber, sent.SchemaVersion, sent.CompanyCode, sent.PlantCode,
		sent.SupplierNumber, sent.Items[0].ItemNo, sent.Items[0].StorageLocationCode, sent.Items[0].Quantity, sent.Items[0].Unit, *sent.Items[0].DeliveryDate,
		sent.Items[0].MaterialNumber)
	if want := "inbound_receipt 4500000001 v1 2000/P1 for SUP-1: 1 S1 100 EA 2026-12-01 " + sent.Items[0].MaterialNumber; got != want ||
		sent.Items[0].MaterialNumber == "" {
		t.Errorf("what the WMS was sent: got %s, want %s with the bracket's number", got, want)
	}
	e.wms.Report("GR-1", goodsReceipt("GR-1", po, "1 60 EA"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll: got %d, %v; want 1 recorded", n, err)
	}
	in := e.inbound(t, "GR-1")
	for k, want := range map[string]any{"status": "posted", "kind": "goods_receipt", "purchase_order_number": "4500000001", "company_code": "2000"} {
		if in[k] != want {
			t.Errorf("the goods receipt reported, %s: got %v, want %v (%v: %v)", k, in[k], want, in["problem_code"], in["problem_detail"])
		}
	}
	if in["material_document_number"] == nil || in["acknowledged_at"] == nil {
		t.Errorf("the goods receipt reported: document %v, acknowledged %v; want both", in["material_document_number"], in["acknowledged_at"])
	}
	if qty, n := e.received(t, po, 1); qty != "60" || n != 1 {
		t.Errorf("item 1 received: got %s in %d movements, want 60 in 1", qty, n)
	}
	var reference, movement string
	pgtest.Scan(t, `SELECT coalesce(m.reference, ''), m.movement_type FROM procurement.goods_movement m WHERE m.tenant_id = $1 AND m.material_document_id = $2`,
		[]any{e.Tenant.ID, in["material_document_id"]}, &reference, &movement)
	if reference != "GR-1" || movement != "101" {
		t.Errorf("the goods receipt: got %s with reference %q, want 101 with reference GR-1", movement, reference)
	}
	e.wms.Report("GR-1", goodsReceipt("GR-1", po, "1 60 EA"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("the confirmation listed again: got %d recorded, %v; want 0", n, err)
	}
	if qty, n := e.received(t, po, 1); qty != "60" || n != 1 || e.wms.Acknowledged("GR-1") != 1 {
		t.Errorf("after it was listed again: got %s in %d movements, %d acknowledgements; want 60 in 1, acknowledged", qty, n, e.wms.Acknowledged("GR-1"))
	}
	long := strings.Repeat("L", 41) // longer than a goods receipt's reference
	e.wms.Report(long, goodsReceipt(long, po, "1 10 EA"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll of a long id: got %d, %v; want 1 recorded", n, err)
	}
	if in := e.inbound(t, long); in["status"] != "posted" {
		t.Fatalf("the goods receipt of a long id: got %v (%v), want posted", in["status"], in["problem_code"])
	}
	var refs int
	pgtest.Scan(t, `SELECT count(DISTINCT m.id) FROM procurement.goods_movement m JOIN procurement.goods_movement_item i ON i.tenant_id = m.tenant_id
		AND i.goods_movement_id = m.id WHERE m.tenant_id = $1 AND i.purchase_order_id = $2 AND m.reference IS NULL`, []any{e.Tenant.ID, po}, &refs)
	if refs != 1 {
		t.Errorf("goods receipts of the order without a reference: got %d, want 1 (the long id's)", refs)
	}
	if n := len(e.orderMessages(t, po, "")); n != 3 {
		t.Errorf("the order's messages: got %d, want 3", n)
	}
}

// TestWMSReceiptsNotQueued: an order is not sent when no active WMS
// manages a plant of a stock item of it, when it is no longer released
// or not found when its event is read, when its stock items at the
// managed plant are all received, or when its event is malformed (logged
// and acknowledged).
func TestWMSReceiptsNotQueued(t *testing.T) {
	e := setupWMS(t)
	n := 0 // the orders' numbers
	for name, c := range map[string]struct {
		change func(t *testing.T, po uuid.UUID) outbox.Message
		logged bool
	}{
		"its items at P1 no stock items": {func(t *testing.T, po uuid.UUID) outbox.Message {
			t.Helper()
			e.notStock(t, po)
			return e.releasedEvent(po, "x")
		}, false},
		"another company's WMS alone": {func(t *testing.T, po uuid.UUID) outbox.Message {
			t.Helper()
			e.connectorSet(t, `settings = '{"adapter": "standin", "base_url": "`+e.wms.URL+`", "plants": {"3000": ["P1"]}}'`)
			return e.releasedEvent(po, "x")
		}, false},
		"cancelled since": {func(t *testing.T, po uuid.UUID) outbox.Message {
			t.Helper()
			pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'cancelled', version = version + 1 WHERE id = $1`, po)
			return e.releasedEvent(po, "x")
		}, false},
		"not found": {func(*testing.T, uuid.UUID) outbox.Message { return e.releasedEvent(uuid.Must(uuid.NewV7()), "x") }, false},
		"received in full": {func(t *testing.T, po uuid.UUID) outbox.Message {
			t.Helper()
			today := time.Now().Format(time.DateOnly)
			e.Must(201, "POST", "/v1/goods-receipts", wmsJSON(t, map[string]any{"legal_entity_id": e.DE, "document_date": today,
				"posting_date": today, "items": []any{map[string]any{"purchase_order_id": po, "po_item_no": 1, "quantity": map[string]any{"value": "100",
					"unit": "EA"}}}}), "")
			return e.releasedEvent(po, "x")
		}, false},
		"malformed": {func(_ *testing.T, po uuid.UUID) outbox.Message {
			m := e.releasedEvent(po, "x")
			m.Data = []byte(`{`)
			return m
		}, true},
		"no order": {func(_ *testing.T, po uuid.UUID) outbox.Message {
			m := e.releasedEvent(po, "x")
			m.Data = []byte(`{"legal_entity_code": "2000"}`)
			return m
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			e.connectorSet(t, `settings = '{"adapter": "standin", "base_url": "`+e.wms.URL+`", "plants": {"2000": ["P1"]}}'`)
			n++
			po := e.receiptOrder(t, fmt.Sprintf("45000001%02d", n))
			var log pgtest.SyncBuffer
			if err := e.queue(t, &log).Handle(context.Background(), c.change(t, po)); err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if got := strings.Contains(log.String(), "refused an event"); got != c.logged {
				t.Errorf("logged: got %v, want %v (%s)", got, c.logged, log.String())
			}
			if n := len(e.orderMessages(t, po, "")); n != 0 {
				t.Errorf("messages: got %d, want 0", n)
			}
		})
	}
}

// TestWMSReceiptOutcomes: a goods receipt of an order the connector was
// not sent, naming an item it was not sent, naming no order, or that
// procurement refuses, is recorded refused with its problem and
// acknowledged, nothing of it posted.
func TestWMSReceiptOutcomes(t *testing.T) {
	e := setupWMS(t)
	po := e.receiptOrder(t, "4500000010")
	e.queuedOrder(t, po, "4500000010")
	other := e.receiptOrder(t, "4500000011")
	changed := e.receiptOrder(t, "4500000012") // its item 1 made a non-stock item once sent
	e.queuedOrder(t, changed, "4500000012")
	x := e.exchange(t, time.Now)
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body []byte
		code string
	}{
		"an order not sent it":    {goodsReceipt("GR-other", other, "1 1 EA"), "integration.wms_unknown_order"},
		"an item not sent it":     {goodsReceipt("GR-item", po, "1 1 EA", "3 1 EA"), "integration.wms_item_not_sent"},
		"the non-stock item":      {goodsReceipt("GR-paper", po, "2 1 EA"), "integration.wms_item_not_sent"},
		"no order":                {[]byte(`{"message_id": "GR-none", "type": "goods_receipt"}`), "integration.wms_message_unreadable"},
		"items not a list":        {[]byte(`{"message_id": "GR-list", "type": "goods_receipt", "items": "all"}`), "integration.wms_message_unreadable"},
		"more than ordered":       {goodsReceipt("GR-more", po, "1 200 EA"), "procurement.over_delivery"},
		"another unit":            {goodsReceipt("GR-unit", po, "1 1 BOX"), "procurement.unit_mismatch"},
		"no longer a stock order": {goodsReceipt("GR-stock", changed, "1 1 EA"), "integration.wms_not_stock"},
	} {
		t.Run(name, func(t *testing.T) {
			mid := strings.Split(strings.Split(string(c.body), `"message_id": "`)[1], `"`)[0]
			if name == "no longer a stock order" {
				e.notStock(t, changed)
			}
			e.wms.Report(mid, c.body)
			if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
				t.Fatalf("poll: got %d, %v; want 1 recorded", n, err)
			}
			in := e.inbound(t, mid)
			if in["status"] != "refused" || in["problem_code"] != c.code || in["acknowledged_at"] == nil {
				t.Errorf("got %v %v, acknowledged %v; want refused %s, acknowledged", in["status"], in["problem_code"], in["acknowledged_at"], c.code)
			}
		})
	}
	if qty, n := e.received(t, po, 1); qty != "0" || n != 0 {
		t.Errorf("item 1 after the refused ones: got %s in %d movements, want nothing (nothing of them kept)", qty, n)
	}
}

// failingOrders is procurement's port, failing with err.
type failingOrders struct{ err error }

func (f failingOrders) Expected(context.Context, uuid.UUID) (integration.WMSExpectedOrder, bool, error) {
	return integration.WMSExpectedOrder{}, false, f.err
}

func (f failingOrders) ReceiptReported(context.Context, uuid.UUID, uuid.UUID, string, []integration.WMSReceived) (integration.WMSPosted, error) {
	return integration.WMSPosted{}, f.err
}

// TestWMSReceiptPortFailures: procurement's port failing, or asking to be
// retried, fails the queue's event, which is read again, and records
// nothing of a goods receipt, which the next poll takes again.
func TestWMSReceiptPortFailures(t *testing.T) {
	e := setupWMS(t)
	po := e.receiptOrder(t, "4500000030")
	var log pgtest.SyncBuffer
	q := e.queue(t, &log)
	q.Orders = failingOrders{errors.New("procurement down")}
	if err := q.Handle(context.Background(), e.releasedEvent(po, "4500000030")); err == nil || !strings.Contains(err.Error(), "procurement down") {
		t.Errorf("the queue: got %v, want the port's failure", err)
	}
	e.queuedOrder(t, po, "4500000030")
	e.wms.Report("GR-port", goodsReceipt("GR-port", po, "1 1 EA"))
	for name, err := range map[string]error{"failing": errors.New("procurement down"), "a retry asked": problem.Unavailable("busy", 1)} {
		t.Run(name, func(t *testing.T) {
			x := e.exchange(t, time.Now)
			x.Orders = failingOrders{err}
			if _, err := x.PollOnce(context.Background()); err == nil {
				t.Fatal("got nil, want the failure reported")
			}
			if n := len(e.orderMessages(t, po, "&direction=inbound")); n != 0 {
				t.Errorf("recorded: got %d, want 0", n)
			}
		})
	}
	x := e.exchange(t, time.Now)
	if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
		t.Errorf("the next poll: got %d, %v; want it recorded", n, err)
	}
}
