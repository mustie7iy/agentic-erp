package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// tmsConnector is a request creating a tms connector of the stand-in for
// the plants.
func (e wmsEnv) tmsConnector(name, plants string) string {
	return fmt.Sprintf(`{"kind": "tms", "name": %q, "secret_ref": %q, "settings": {"adapter": "standin", "base_url": %q, "plants": %s}}`,
		name, e.ref, e.wms.URL, plants)
}

// withTMS is the scene with a tms connector for P1 beside its wms one,
// whose id it returns; the wms connector is disabled, so the stand-in's
// confirmations are the transport system's alone.
func (e wmsEnv) withTMS(t *testing.T) string {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/connectors", e.tmsConnector("Transport P1", `{"2000": ["P1"]}`), "")
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.conn)
	return c["id"].(string)
}

// tmsMessages are the transport system's messages listed by the query.
func (e wmsEnv) tmsMessages(t *testing.T, query string) []map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/tms-messages?"+query, "", "")
	items, _ := page["items"].([]any)
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = it.(map[string]any)
	}
	return out
}

// tmsInbound is the transport system's inbound message of the id.
func (e wmsEnv) tmsInbound(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, m := range e.tmsMessages(t, "direction=inbound") {
		if m["message_id"] == id {
			return m
		}
	}
	t.Fatalf("no inbound tms message %s", id)
	return nil
}

// shipmentReport is a shipment confirmation of the delivery, its freight
// cost "amount currency".
func shipmentReport(id, delivery, number, freight string) []byte {
	f := strings.Fields(freight)
	return fmt.Appendf(nil, `{"message_id": %q, "type": "shipment", "outbound_delivery_id": %q, "shipment_number": %q, "carrier_code": "NWTRANS",
		"freight_cost": {"amount": %q, "currency": %q}}`, id, delivery, number, f[0], f[1])
}

// TestTMSDeliveryRoundTrip (S2r): a plant has one transport system, which
// may stand beside its warehouse system; a delivery created there is
// queued once for each, however often its event is read, the transport
// system's a transport request listed among its messages alone, sent
// once; the shipment it reports is recorded on the delivery once by its
// message id and acknowledged, posting nothing; listed again it is
// acknowledged again, not recorded twice.
func TestTMSDeliveryRoundTrip(t *testing.T) {
	e := setupWMS(t)
	c, _ := e.Must(201, "POST", "/v1/connectors", e.tmsConnector("Transport P1", `{"2000": ["P1"]}`), "")
	if c["kind"] != "tms" {
		t.Errorf("the connector's kind: got %v, want tms", c["kind"])
	}
	e.Problem(409, "integration.plant_has_tms", "POST", "/v1/connectors", e.tmsConnector("Second transport", `{"2000": ["P1"]}`), "")
	id, number := e.delivery(t, e.p1, e.s1, "3")
	for range 2 {
		e.queued(t, id)
	}
	out := e.tmsMessages(t, "direction=outbound&outbound_delivery_id="+id)
	if len(out) != 1 || out[0]["kind"] != "transport_request" || out[0]["status"] != "pending" || out[0]["connector_name"] != "Transport P1" ||
		out[0]["plant_code"] != "P1" || out[0]["outbound_delivery_number"] != number || out[0]["company_code"] != "2000" {
		t.Fatalf("the transport system's messages: got %v, want one pending transport_request of %s at 2000/P1", out, number)
	}
	if w := e.message(t, id); w["kind"] != "outbound_delivery" {
		t.Errorf("the warehouse system's message: got %v, want outbound_delivery beside it", w["kind"])
	}
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.conn)
	x := e.exchange(t, time.Now)
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatalf("send: got %v, want nil", err)
	}
	out = e.tmsMessages(t, "direction=outbound&outbound_delivery_id="+id)
	body, sends := e.wms.Received(out[0]["message_id"].(string))
	var sent integration.WMSDelivery
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("what the TMS was sent: %v", err)
	}
	if out[0]["status"] != "sent" || sends != 1 || sent.Type != "transport_request" || sent.DeliveryNumber != number || len(sent.Items) != 1 {
		t.Errorf("the transport request: got %v, sent %d times as %s of %s with %d items; want sent once, a transport_request of %s with 1 item",
			out[0]["status"], sends, sent.Type, sent.DeliveryNumber, len(sent.Items), number)
	}
	e.wms.Report("SH-1", shipmentReport("SH-1", id, "SH-1001", "45.50 EUR"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll: got %d, %v; want 1 recorded", n, err)
	}
	in := e.tmsInbound(t, "SH-1")
	if in["status"] != "recorded" || in["kind"] != "shipment" || in["outbound_delivery_number"] != number || in["acknowledged_at"] == nil {
		t.Errorf("the shipment reported: got %v %v of %v, acknowledged %v; want a recorded shipment of %s, acknowledged", in["status"], in["kind"],
			in["outbound_delivery_number"], in["acknowledged_at"], number)
	}
	d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+id, "", "")
	s, _ := d["shipment"].(map[string]any)
	if fc, _ := s["freight_cost"].(map[string]any); s["shipment_number"] != "SH-1001" || s["carrier_code"] != "NWTRANS" || fc["amount"] != "45.50" ||
		fc["currency"] != "EUR" || d["status"] != "created" {
		t.Errorf("the delivery: got %v with %v, want created with SH-1001 by NWTRANS at 45.50 EUR", d["status"], s)
	}
	e.wms.Report("SH-1", shipmentReport("SH-1", id, "SH-1001", "45.50 EUR"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("the shipment listed again: got %d recorded, %v; want 0", n, err)
	}
	if n := e.wms.Acknowledged("SH-1"); n != 1 {
		t.Errorf("acknowledgements after it was listed again: got %d, want 1", n)
	}
	if wms, _ := e.Must(200, "GET", "/v1/wms-messages?outbound_delivery_id="+id, "", ""); len(wms["items"].([]any)) != 1 {
		t.Errorf("the warehouse system's messages of the delivery: got %v, want its outbound_delivery alone", wms["items"])
	}
}

// TestTMSShipmentRefusals (S2r): a shipment of a delivery the transport
// system was not sent, a second shipment of a delivery, one shipping
// refuses (its amount), and a confirmation that is not a shipment of its message id
// naming a delivery, are recorded refused, nothing recorded on the
// delivery.
func TestTMSShipmentRefusals(t *testing.T) {
	e := setupWMS(t)
	e.withTMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	unshipped, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, unshipped)
	other, _ := e.delivery(t, e.p2, e.s2, "1")
	x := e.exchange(t, time.Now)
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.wms.Report("SH-OK", shipmentReport("SH-OK", id, "SH-1", "10.00 EUR"))
	if _, err := x.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body func(msg string) []byte
		code string
	}{
		"a delivery not sent": {func(msg string) []byte { return shipmentReport(msg, other, "SH-2", "1.00 EUR") }, "integration.tms_unknown_delivery"},
		"a second shipment":   {func(msg string) []byte { return shipmentReport(msg, id, "SH-3", "1.00 EUR") }, "shipping.shipment_recorded"},
		"three decimals":      {func(msg string) []byte { return shipmentReport(msg, unshipped, "SH-4", "1.000 EUR") }, "request.invalid_amount"},
		"a goods issue":       {func(msg string) []byte { return goodsIssue(msg, id, "1 1 BOX") }, "integration.wms_message_unreadable"},
		"another message id":  {func(string) []byte { return shipmentReport("other", id, "SH-5", "1.00 EUR") }, "integration.wms_message_unreadable"},
		"no delivery": {func(msg string) []byte {
			return fmt.Appendf(nil, `{"message_id": %q, "type": "shipment", "shipment_number": "SH-6"}`, msg)
		}, "integration.wms_message_unreadable"},
		"a message id not a string": {func(string) []byte { return []byte(`{"message_id": 7, "type": "shipment"}`) }, "integration.wms_message_unreadable"},
		"not a JSON object":         {func(string) []byte { return []byte(`[1]`) }, "integration.wms_message_unreadable"},
		"larger than 1 MiB": {func(string) []byte { return []byte(`{"x": "` + strings.Repeat("x", integration.MaxWMSMessage) + `"}`) },
			"integration.wms_message_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := uuid.NewString()
			e.wms.Report(msg, c.body(msg))
			if _, err := x.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll: got %v, want nil", err)
			}
			in := e.tmsInbound(t, msg)
			if in["status"] != "refused" || in["problem_code"] != c.code || in["kind"] != "shipment" {
				t.Errorf("got %v %v (%v), want a refused shipment, %s", in["status"], in["kind"], in["problem_code"], c.code)
			}
		})
	}
	d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+id, "", "")
	if s, _ := d["shipment"].(map[string]any); s["shipment_number"] != "SH-1" {
		t.Errorf("the delivery's shipment after the refusals: got %v, want SH-1 kept", s)
	}
	if d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+other, "", ""); d["shipment"] != nil {
		t.Errorf("the delivery not sent: got shipment %v, want none", d["shipment"])
	}
}

// TestTMSConnectorUnusable (S2r): a transport request whose connector is
// of another kind, or the stand-in in production, is a failed attempt,
// tried again.
func TestTMSConnectorUnusable(t *testing.T) {
	e := setupWMS(t)
	tms := e.withTMS(t)
	at := time.Now().Add(time.Minute)
	for name, c := range map[string]struct {
		change func(x *integration.WMSExchange)
		code   string
	}{
		"of another kind": {func(*integration.WMSExchange) {
			pgtest.Exec(t, `UPDATE integration.connector SET kind = 'email', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, tms)
		}, "integration.not_a_tms"},
		"the stand-in in production": {func(x *integration.WMSExchange) { x.RefuseStandIn = true }, "integration.adapter_not_certified"},
	} {
		t.Run(name, func(t *testing.T) {
			pgtest.Exec(t, `UPDATE integration.connector SET kind = 'tms', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, tms)
			id, _ := e.delivery(t, e.p1, e.s1, "1")
			e.queued(t, id)
			x := e.exchange(t, func() time.Time { return at })
			c.change(x)
			if _, err := x.SendOnce(context.Background()); err != nil {
				t.Fatalf("send: got %v, want nil", err)
			}
			var status, code string
			pgtest.Scan(t, `SELECT status, coalesce(last_error, '') FROM integration.wms_message WHERE tenant_id = $1 AND document_id = $2
				AND kind = 'transport_request'`, []any{e.Tenant.ID, id}, &status, &code)
			if status != "pending" || code != c.code {
				t.Errorf("got %s, %s; want pending, %s", status, code, c.code)
			}
		})
	}
}

// TestTMSList (S2r): the transport system's messages are listed by
// connector and status a page at a time, of the companies the caller may
// act in; a page size out of range is refused, and the list refuses
// outside a request.
func TestTMSList(t *testing.T) {
	e := setupWMS(t)
	tms := e.withTMS(t)
	for range 2 {
		id, _ := e.delivery(t, e.p1, e.s1, "1")
		e.queued(t, id)
	}
	if got := e.tmsMessages(t, "connector_id="+tms+"&status=pending"); len(got) != 2 {
		t.Errorf("the pending transport requests: got %d, want 2", len(got))
	}
	first, _ := e.Must(200, "GET", "/v1/tms-messages?page_size=1", "", "")
	tok, _ := first["next_page_token"].(string)
	second, _ := e.Must(200, "GET", "/v1/tms-messages?page_size=1&page_token="+tok, "", "")
	if a, b := first["items"].([]any)[0].(map[string]any)["id"], second["items"].([]any)[0].(map[string]any)["id"]; tok == "" || a == b {
		t.Errorf("the pages: got %v then %v (token %q), want two messages", a, b, tok)
	}
	if got := e.tmsMessages(t, "connector_id="+e.conn); len(got) != 0 {
		t.Errorf("the wms connector's messages among the transport system's: got %d, want 0", len(got))
	}
	h := app.Integration(app.Config{Pages: wmsPages(t)})
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListTMSMessages(ctx, gen.ListTMSMessagesParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if p := problem.From(err); p.Code != "request.invalid_page_size" {
		t.Errorf("a page size of 0: got %v, want request.invalid_page_size", err)
	}
	if _, err := h.ListTMSMessages(context.Background(), gen.ListTMSMessagesParams{}); err == nil {
		t.Error("outside a request: got nil, want refused")
	}
}
