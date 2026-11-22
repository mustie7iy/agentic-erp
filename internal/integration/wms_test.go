package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/integration/wmstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

var wmsPerms = []string{"materials.material.read", "materials.material.write", "tenancy.org.manage", "inventory.movement.post", "inventory.stock.read",
	"inventory.valuation.read", "inventory.valuation.write", "sales.order.read", "sales.order.write", "shipping.delivery.read", "shipping.delivery.write",
	"integration.connector.read", "integration.connector.manage"}

const deliveryChanged = "com.example.erp.shipping.outbound_delivery_changed.v1"

// wmsEnv is a configured tenant whose company 2000 has plants P1
// (storage location S1) and P2 (S2); the bracket, a trading good kept at
// both, by the box of 20, valued at a moving price of 2.00 EUR, with 200
// EA at S1; Northwind, a customer; and the warehouse system's stand-in,
// connected for P1 by the wms connector conn.
type wmsEnv struct {
	fixture.Env
	p1, s1, p2, s2 uuid.UUID
	bracket        string
	customer       uuid.UUID
	revenue        uuid.UUID
	wms            *wmstest.Server
	conn           string
	ref            string
}

const wmsToken = "wms-token"

func setupWMS(t *testing.T) wmsEnv {
	t.Helper()
	e := wmsEnv{Env: fixture.NewWith(t, nil, wmsValued, wmsPerms...)}
	e.p1 = e.unit(t, "plant", "P1", nil)
	e.s1 = e.unit(t, "storage_location", "S1", &e.p1)
	e.p2 = e.unit(t, "plant", "P2", nil)
	e.s2 = e.unit(t, "storage_location", "S2", &e.p2)
	m, _ := e.Must(201, "POST", "/v1/materials", wmsJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Machined bracket", "base_unit": "EA",
		"material_type": "trading_good", "material_group": "BRACKETS", "confirm_duplicate": true,
		"plants":            []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}, map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s2}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	e.bracket = m["id"].(string)
	for _, p := range []uuid.UUID{e.p1, e.p2} {
		e.Must(201, "POST", "/v1/material-valuations", wmsJSON(t, map[string]any{"material_id": e.bracket, "plant_id": p, "valuation_class": "3000",
			"price_control": "moving_average", "price": map[string]any{"amount": "2.00", "currency": "EUR"}}), "")
	}
	e.Must(201, "POST", "/v1/material-documents", wmsJSON(t, map[string]any{"movement_type": "561", "items": []any{map[string]any{"material_id": e.bracket,
		"plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": "200", "unit": "EA"}}}}), "")
	e.revenue = e.Account(t, "2000", "400000")
	e.customer = e.Customer(t, "BP70000001", "Northwind Traders")
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by,
		updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.customer, e.DE, e.Account(t, "2000", "120000"), e.Tenant.Human.ID)
	e.wms = wmstest.NewServer(t, wmsToken)
	e.ref = vaulttest.TenantRef(e.Tenant.ID, "wms")
	c, _ := e.Must(201, "POST", "/v1/connectors", e.connector("Warehouse P1", `{"2000": ["P1"]}`), "")
	e.conn = c["id"].(string)
	return e
}

// wmsValued gives company 2000 a customer reconciliation account and the
// inventory accounts of valuation class 3000.
func wmsValued(c *config.Config) {
	de := c.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true},
		config.Account{Number: "150000", Name: "Inventory", Type: config.Asset}, config.Account{Number: "391000", Name: "Initial entries", Type: config.Equity},
		config.Account{Number: "515000", Name: "Consumption", Type: config.Expense}, config.Account{Number: "519000", Name: "Cost of goods sold", Type: config.Expense})
	de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", InitialEntry: "391000", Consumption: "515000",
		CostOfGoodsSold: "519000"}}
}

func (e wmsEnv) unit(t *testing.T, kind, code string, parent *uuid.UUID) uuid.UUID {
	t.Helper()
	body := map[string]any{"legal_entity_id": e.DE, "kind": kind, "code": code, "name": "Unit " + code}
	if parent != nil {
		body["parent_id"] = *parent
	}
	got, _ := e.Must(201, "POST", "/v1/org-units", wmsJSON(t, body), "")
	id := got["id"].(string)
	e.Must(200, "POST", "/v1/org-units/"+id+":activate", "", `"1"`)
	return uuid.MustParse(id)
}

// connector is a request creating a wms connector of the stand-in for the
// plants.
func (e wmsEnv) connector(name, plants string) string {
	return fmt.Sprintf(`{"kind": "wms", "name": %q, "secret_ref": %q, "settings": {"adapter": "standin", "base_url": %q, "plants": %s}}`,
		name, e.ref, e.wms.URL, plants)
}

func wmsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// delivery creates a delivery of a confirmed order of Northwind's for
// boxes of the bracket from the plant's storage location and returns its
// id and number.
func (e wmsEnv) delivery(t *testing.T, plant, location uuid.UUID, boxes string) (string, string) {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "items": [
		{"material_id": %q, "quantity": {"value": %q, "unit": "BOX"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`,
		e.DE, e.customer, e.bracket, boxes, e.revenue), "")
	order := o["id"].(string)
	// Confirmed as an order without a plant was (shipping's tests): the
	// delivery names its plant.
	pgtest.Exec(t, `UPDATE sales.sales_order SET status = 'confirmed', plant_id = NULL, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		e.Tenant.ID, order)
	d, _ := e.Must(201, "POST", "/v1/outbound-deliveries", wmsJSON(t, map[string]any{"sales_order_id": order, "plant_id": plant,
		"storage_location_id": location}), "")
	return d["id"].(string), d["number"].(string)
}

// busEvent is the delivery's first OutboundDeliveryChanged as the relay
// publishes it.
func (e wmsEnv) busEvent(t *testing.T, delivery string) outbox.Message {
	t.Helper()
	var id, actor uuid.UUID
	var payload []byte
	pgtest.Scan(t, `SELECT id, actor_id, payload FROM shipping.outbox WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3
		ORDER BY id LIMIT 1`, []any{e.Tenant.ID, deliveryChanged, delivery}, &id, &actor, &payload)
	return outbox.Message{ID: id, Subject: "erp.test.shipping." + e.Tenant.ID.String() + ".outbound_delivery_changed", Data: payload,
		Header: map[string]string{"ce-specversion": "1.0", "ce-id": id.String(), "ce-type": deliveryChanged, "ce-subject": delivery,
			"ce-tenantid": e.Tenant.ID.String(), "ce-actorid": actor.String(), "Content-Type": "application/json"}}
}

func (e wmsEnv) queue(t *testing.T, log *pgtest.SyncBuffer) *integration.WMSQueue {
	t.Helper()
	return &integration.WMSQueue{Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.NewTextHandler(log, nil))}
}

// exchange is the exchange as the server wires it, calling the stand-in
// with the tenant's secret, at now.
func (e wmsEnv) exchange(t *testing.T, now func() time.Time) *integration.WMSExchange {
	t.Helper()
	client, err := integration.NewWMSClient(egress.LoopbackForTests(e.wms.Roots))
	if err != nil {
		t.Fatal(err)
	}
	// The exchange works across tenants: other tests' (and earlier runs')
	// connectors and messages are put out of its way.
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE kind = 'wms' AND status = 'active' AND tenant_id <> $1`, e.Tenant.ID)
	pgtest.Exec(t, `UPDATE integration.wms_message SET status = 'failed', attempts = greatest(attempts, 1), next_attempt_at = NULL, version = version + 1
		WHERE status = 'pending' AND tenant_id <> $1`, e.Tenant.ID)
	x := app.WMS(app.Config{Secrets: vaulttest.Refs{e.ref: wmsToken}, Now: now}, client)
	x.Pool, x.AppRole, x.WorkerRole = pgtest.Pool(t), "erp_app", "erp_worker"
	return x
}

// queued reads the delivery's event into the queue.
func (e wmsEnv) queued(t *testing.T, delivery string) {
	t.Helper()
	var log pgtest.SyncBuffer
	if err := e.queue(t, &log).Handle(context.Background(), e.busEvent(t, delivery)); err != nil {
		t.Fatalf("the queue: got %v, want nil", err)
	}
}

// message is the delivery's outbound message as listed.
func (e wmsEnv) message(t *testing.T, delivery string) map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/wms-messages?direction=outbound&outbound_delivery_id="+delivery, "", "")
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("outbound messages of delivery %s: got %d, want 1", delivery, len(items))
	}
	return items[0].(map[string]any)
}

// inbound is the inbound message of the WMS's id, as listed.
func (e wmsEnv) inbound(t *testing.T, id string) map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/wms-messages?direction=inbound&connector_id="+e.conn, "", "")
	items, _ := page["items"].([]any)
	for _, it := range items {
		if m := it.(map[string]any); m["message_id"] == id {
			return m
		}
	}
	t.Fatalf("no inbound message %s", id)
	return nil
}

// goodsIssue is a goods_issue confirmation of the delivery picking
// "item quantity unit" each.
func goodsIssue(id, delivery string, items ...string) []byte {
	var list []string
	for _, it := range items {
		f := strings.Fields(it)
		list = append(list, fmt.Sprintf(`{"item_no": %s, "quantity": %q, "unit": %q}`, f[0], f[1], f[2]))
	}
	return fmt.Appendf(nil, `{"message_id": %q, "type": "goods_issue", "outbound_delivery_id": %q, "items": [%s]}`, id, delivery, strings.Join(list, ", "))
}

// TestWMSDeliveryRoundTrip: a delivery created at a plant the wms
// connector manages is queued once, however often its event is read, and
// sent once to the warehouse system, naming the delivery, its plant and
// items; the goods issue the WMS reports is posted once, its picked
// quantity issued as a 601, and acknowledged; reported again (its
// acknowledgement lost) it is acknowledged again, not posted twice. A
// delivery at a plant no WMS manages is not queued; nor is a later event
// of a queued one.
func TestWMSDeliveryRoundTrip(t *testing.T) {
	e := setupWMS(t)
	id, number := e.delivery(t, e.p1, e.s1, "3")
	for range 2 {
		e.queued(t, id)
	}
	m := e.message(t, id)
	for k, want := range map[string]any{"status": "pending", "kind": "outbound_delivery", "connector_name": "Warehouse P1", "plant_code": "P1",
		"company_code": "2000", "outbound_delivery_number": number, "attempts": float64(0)} {
		if m[k] != want {
			t.Errorf("the queued message's %s: got %v, want %v", k, m[k], want)
		}
	}
	x := e.exchange(t, time.Now)
	if n, err := x.SendOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("send: got %d, %v; want 1 sent", n, err)
	}
	m = e.message(t, id)
	if m["status"] != "sent" || m["attempts"] != float64(1) || m["sent_at"] == nil {
		t.Errorf("the sent message: got %v after %v attempts at %v, want sent after 1", m["status"], m["attempts"], m["sent_at"])
	}
	body, sends := e.wms.Received(m["message_id"].(string))
	var sent integration.WMSDelivery
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("what the WMS was sent: %v", err)
	}
	if sends != 1 || sent.DeliveryNumber != number || sent.PlantCode != "P1" || sent.StorageLocation != "S1" || len(sent.Items) != 1 ||
		sent.Items[0].Quantity != "3" || sent.Items[0].Unit != "BOX" || sent.CustomerNumber != "BP70000001" {
		t.Errorf("what the WMS was sent: got %d sends of %+v, want one of the delivery's 3 BOX from P1/S1", sends, sent)
	}
	if n, err := x.SendOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("a second round: got %d, %v; want nothing due", n, err)
	}
	e.wms.Report("GI-1", goodsIssue("GI-1", id, "1 2 BOX"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll: got %d, %v; want 1 recorded", n, err)
	}
	in := e.inbound(t, "GI-1")
	if in["status"] != "posted" || in["outbound_delivery_number"] != number || in["material_document_number"] == nil || in["acknowledged_at"] == nil {
		t.Errorf("the goods issue reported: got %v of %v, document %v, acknowledged %v; want posted, acknowledged", in["status"],
			in["outbound_delivery_number"], in["material_document_number"], in["acknowledged_at"])
	}
	d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+id, "", "")
	if d["status"] != "goods_issued" || d["material_document_id"] != in["material_document_id"] {
		t.Errorf("the delivery: got %v, document %v; want goods_issued by the reported movement's", d["status"], d["material_document_id"])
	}
	if p := d["items"].([]any)[0].(map[string]any)["picked_quantity"].(map[string]any)["value"]; p != "2" {
		t.Errorf("the delivery's picked quantity: got %v, want 2", p)
	}
	if n := e.wms.Acknowledged("GI-1"); n != 1 {
		t.Errorf("acknowledgements: got %d, want 1", n)
	}
	e.wms.Report("GI-1", goodsIssue("GI-1", id, "1 2 BOX"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("the confirmation listed again: got %d recorded, %v; want 0", n, err)
	}
	if n := e.wms.Acknowledged("GI-1"); n != 1 {
		t.Errorf("acknowledgements after it was listed again: got %d, want 1 (the stand-in counts afresh after Report)", n)
	}
	t.Run("a plant no WMS manages", func(t *testing.T) {
		other, _ := e.delivery(t, e.p2, e.s2, "1")
		e.queued(t, other)
		page, _ := e.Must(200, "GET", "/v1/wms-messages?outbound_delivery_id="+other, "", "")
		if n := len(page["items"].([]any)); n != 0 {
			t.Errorf("messages of a delivery at P2: got %d, want 0", n)
		}
	})
	t.Run("a later event", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM integration.wms_message WHERE tenant_id = $1 AND document_id = $2`, e.Tenant.ID, id)
		var payload []byte
		pgtest.Scan(t, `SELECT payload FROM shipping.outbox WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY id DESC LIMIT 1`, []any{e.Tenant.ID, id}, &payload)
		ev := e.busEvent(t, id)
		ev.Data = payload
		var log pgtest.SyncBuffer
		if err := e.queue(t, &log).Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		page, _ := e.Must(200, "GET", "/v1/wms-messages?direction=outbound&outbound_delivery_id="+id, "", "")
		if n := len(page["items"].([]any)); n != 0 {
			t.Errorf("messages after the delivery's goods issue event: got %d, want 0", n)
		}
	})
}
