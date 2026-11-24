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

// gtsConnector is a request creating a gts connector of the stand-in for
// the plants.
func (e wmsEnv) gtsConnector(name, plants string) string {
	return fmt.Sprintf(`{"kind": "gts", "name": %q, "secret_ref": %q, "settings": {"adapter": "standin", "base_url": %q, "plants": %s}}`,
		name, e.ref, e.wms.URL, plants)
}

// withGTS is the scene with a gts connector for P1, whose id it returns;
// the wms connector is disabled, so the stand-in's confirmations are the
// trade compliance service's alone.
func (e wmsEnv) withGTS(t *testing.T) string {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/connectors", e.gtsConnector("Compliance P1", `{"2000": ["P1"]}`), "")
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.conn)
	return c["id"].(string)
}

// gtsMessages are the trade compliance service's messages listed by the
// query.
func (e wmsEnv) gtsMessages(t *testing.T, query string) []map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/gts-messages?"+query, "", "")
	items, _ := page["items"].([]any)
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = it.(map[string]any)
	}
	return out
}

// gtsInbound is the service's inbound message of the id.
func (e wmsEnv) gtsInbound(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, m := range e.gtsMessages(t, "direction=inbound") {
		if m["message_id"] == id {
			return m
		}
	}
	t.Fatalf("no inbound gts message %s", id)
	return nil
}

// screening is a screening confirmation of the delivery.
func screening(id, delivery, result, reason string) []byte {
	return fmt.Appendf(nil, `{"message_id": %q, "type": "screening", "outbound_delivery_id": %q, "result": %q, "reason_code": %q}`,
		id, delivery, result, reason)
}

// tradeOf is the delivery's trade compliance as the API gives it.
func (e wmsEnv) tradeOf(t *testing.T, delivery string) map[string]any {
	t.Helper()
	d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+delivery, "", "")
	tc, _ := d["trade_compliance"].(map[string]any)
	return tc
}

// TestGTSDeliveryRoundTrip (S2s): a plant has one trade compliance
// service; a delivery created there is pending and queued once, however
// often its event is read, a screening request listed among the service's
// messages alone, sent once; the service's answer is recorded on the
// delivery once by its message id and acknowledged, posting nothing:
// released for one delivery, blocked with its reason for another; listed
// again it is acknowledged again, not recorded twice. A delivery created
// before the service was connected is neither screened nor sent.
func TestGTSDeliveryRoundTrip(t *testing.T) {
	e := setupWMS(t)
	before, _ := e.delivery(t, e.p1, e.s1, "1")
	c, _ := e.Must(201, "POST", "/v1/connectors", e.gtsConnector("Compliance P1", `{"2000": ["P1"]}`), "")
	if c["kind"] != "gts" {
		t.Errorf("the connector's kind: got %v, want gts", c["kind"])
	}
	e.Problem(409, "integration.plant_has_gts", "POST", "/v1/connectors", e.gtsConnector("Second compliance", `{"2000": ["P1"]}`), "")
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.conn)
	e.queued(t, before)
	if got := e.gtsMessages(t, "outbound_delivery_id="+before); len(got) != 0 || e.tradeOf(t, before) != nil {
		t.Errorf("a delivery created before the service: got %d messages, screening %v; want none", len(got), e.tradeOf(t, before))
	}
	released, number := e.delivery(t, e.p1, e.s1, "3")
	blocked, _ := e.delivery(t, e.p1, e.s1, "1")
	if tc := e.tradeOf(t, released); tc["status"] != "pending" {
		t.Errorf("the delivery at P1: got %v, want pending", tc)
	}
	for range 2 {
		e.queued(t, released)
	}
	e.queued(t, blocked)
	out := e.gtsMessages(t, "direction=outbound&outbound_delivery_id="+released)
	if len(out) != 1 || out[0]["kind"] != "screening_request" || out[0]["status"] != "pending" || out[0]["connector_name"] != "Compliance P1" ||
		out[0]["outbound_delivery_number"] != number || out[0]["company_code"] != "2000" || out[0]["plant_code"] != "P1" {
		t.Fatalf("the service's messages: got %v, want one pending screening_request of %s at 2000/P1", out, number)
	}
	x := e.exchange(t, time.Now)
	if n, err := x.SendOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("send: got %d, %v; want 2 sent", n, err)
	}
	out = e.gtsMessages(t, "direction=outbound&outbound_delivery_id="+released)
	body, sends := e.wms.Received(out[0]["message_id"].(string))
	var sent integration.WMSDelivery
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("what the service was sent: %v", err)
	}
	if out[0]["status"] != "sent" || sends != 1 || sent.Type != "screening_request" || sent.CustomerNumber != "BP70000001" || len(sent.Items) != 1 {
		t.Errorf("the screening request: got %v, sent %d times as %s for %s with %d items; want sent once, a screening_request for BP70000001 with 1 item",
			out[0]["status"], sends, sent.Type, sent.CustomerNumber, len(sent.Items))
	}
	e.wms.Report("SC-1", screening("SC-1", released, "released", ""))
	e.wms.Report("SC-2", screening("SC-2", blocked, "blocked", "SPL-HIT"))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("poll: got %d, %v; want 2 recorded", n, err)
	}
	in := e.gtsInbound(t, "SC-1")
	if in["status"] != "recorded" || in["kind"] != "screening_result" || in["outbound_delivery_number"] != number || in["acknowledged_at"] == nil {
		t.Errorf("the answer: got %v %v of %v, acknowledged %v; want a recorded screening_result of %s, acknowledged", in["status"], in["kind"],
			in["outbound_delivery_number"], in["acknowledged_at"], number)
	}
	if tc := e.tradeOf(t, released); tc["status"] != "released" || tc["screened_at"] == nil {
		t.Errorf("the released delivery: got %v, want released", tc)
	}
	if tc := e.tradeOf(t, blocked); tc["status"] != "blocked" || tc["reason_code"] != "SPL-HIT" {
		t.Errorf("the blocked delivery: got %v, want blocked by SPL-HIT", tc)
	}
	e.wms.Report("SC-2", screening("SC-2", blocked, "released", ""))
	if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("the answer listed again: got %d recorded, %v; want 0", n, err)
	}
	if tc := e.tradeOf(t, blocked); tc["status"] != "blocked" || e.wms.Acknowledged("SC-2") != 1 {
		t.Errorf("the blocked delivery after SC-2 listed again: got %v, acknowledged %d; want blocked, acknowledged once", tc, e.wms.Acknowledged("SC-2"))
	}
	if tms := e.tmsMessages(t, "outbound_delivery_id="+released); len(tms) != 0 {
		t.Errorf("the transport system's messages of the delivery: got %v, want none", tms)
	}
}

// TestGTSScreeningRefusals (S2s): an answer for a delivery the service
// was not sent, one shipping refuses (another result, a reason code too
// long, a delivery whose goods issue is posted), and a confirmation that
// is not a screening of its message id naming a delivery, are recorded
// refused, nothing recorded on the delivery.
func TestGTSScreeningRefusals(t *testing.T) {
	e := setupWMS(t)
	e.withGTS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	issued, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, issued)
	other, _ := e.delivery(t, e.p2, e.s2, "1")
	x := e.exchange(t, time.Now)
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, tag := e.Must(200, "GET", "/v1/outbound-deliveries/"+issued, "", "")
	_, tag = e.Must(200, "POST", "/v1/outbound-deliveries/"+issued+":releaseTradeComplianceBlock", "", tag)
	_, tag = e.Must(200, "POST", "/v1/outbound-deliveries/"+issued+":pick", `{"items": [{"item_no": 1, "quantity": {"value": "1", "unit": "BOX"}}]}`, tag)
	e.Must(200, "POST", "/v1/outbound-deliveries/"+issued+":postGoodsIssue", "", tag)
	for name, c := range map[string]struct {
		body func(msg string) []byte
		code string
	}{
		"a delivery not sent":    {func(msg string) []byte { return screening(msg, other, "blocked", "") }, "integration.gts_unknown_delivery"},
		"another result":         {func(msg string) []byte { return screening(msg, id, "review", "") }, "shipping.invalid_screening"},
		"a reason code too long": {func(msg string) []byte { return screening(msg, id, "blocked", strings.Repeat("R", 41)) }, "shipping.invalid_screening"},
		"after the goods issue":  {func(msg string) []byte { return screening(msg, issued, "blocked", "") }, "shipping.goods_issued"},
		"a shipment":             {func(msg string) []byte { return shipmentReport(msg, id, "SH-1", "1.00 EUR") }, "integration.wms_message_unreadable"},
		"another message id":     {func(string) []byte { return screening("other", id, "blocked", "") }, "integration.wms_message_unreadable"},
		"no delivery": {func(msg string) []byte {
			return fmt.Appendf(nil, `{"message_id": %q, "type": "screening", "result": "blocked"}`, msg)
		}, "integration.wms_message_unreadable"},
		"not a JSON object": {func(string) []byte { return []byte(`[1]`) }, "integration.wms_message_unreadable"},
		"a message id not a string": {func(string) []byte { return []byte(`{"message_id": 7, "type": "screening"}`) },
			"integration.wms_message_unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := uuid.NewString()
			e.wms.Report(msg, c.body(msg))
			if _, err := x.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll: got %v, want nil", err)
			}
			in := e.gtsInbound(t, msg)
			if in["status"] != "refused" || in["problem_code"] != c.code || in["kind"] != "screening_result" {
				t.Errorf("got %v %v (%v), want a refused screening_result, %s", in["status"], in["kind"], in["problem_code"], c.code)
			}
		})
	}
	if tc := e.tradeOf(t, id); tc["status"] != "pending" {
		t.Errorf("the delivery after the refusals: got %v, want pending still", tc)
	}
}

// TestGTSBlocksTheWMSGoodsIssue (S2s): a goods issue a warehouse system
// reports for a delivery whose screening is pending is refused, nothing
// posted, as a person's is: the delivery stays created. The trade
// compliance service's connector is disabled once the delivery is queued,
// so the stand-in's confirmation is the warehouse system's alone.
func TestGTSBlocksTheWMSGoodsIssue(t *testing.T) {
	e := setupWMS(t)
	c, _ := e.Must(201, "POST", "/v1/connectors", e.gtsConnector("Compliance P1", `{"2000": ["P1"]}`), "")
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, c["id"])
	x := e.exchange(t, time.Now)
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.wms.Report("GI-held", goodsIssue("GI-held", id, "1 1 BOX"))
	if _, err := x.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if in := e.inbound(t, "GI-held"); in["status"] != "refused" || in["problem_code"] != "shipping.trade_compliance_block" {
		t.Errorf("the goods issue reported: got %v (%v), want refused, shipping.trade_compliance_block", in["status"], in["problem_code"])
	}
	if d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+id, "", ""); d["status"] != "created" {
		t.Errorf("the delivery: got %v, want created", d["status"])
	}
}

// TestGTSConnectorUnusable (S2s): a screening request whose connector is
// of another kind is a failed attempt, tried again; one whose connector
// was disabled before the event was read is not queued, and the delivery
// waits, pending.
func TestGTSConnectorUnusable(t *testing.T) {
	e := setupWMS(t)
	gts := e.withGTS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	pgtest.Exec(t, `UPDATE integration.connector SET kind = 'email', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, gts)
	x := e.exchange(t, func() time.Time { return time.Now().Add(time.Minute) })
	if _, err := x.SendOnce(context.Background()); err != nil {
		t.Fatalf("send: got %v, want nil", err)
	}
	var status, code string
	pgtest.Scan(t, `SELECT status, coalesce(last_error, '') FROM integration.wms_message WHERE tenant_id = $1 AND document_id = $2
		AND kind = 'screening_request'`, []any{e.Tenant.ID, id}, &status, &code)
	if status != "pending" || code != "integration.not_a_gts" {
		t.Errorf("got %s, %s; want pending, integration.not_a_gts", status, code)
	}
	pgtest.Exec(t, `UPDATE integration.connector SET kind = 'gts', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, gts)
	waiting, _ := e.delivery(t, e.p1, e.s1, "1")
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, gts)
	e.queued(t, waiting)
	if got := e.gtsMessages(t, "outbound_delivery_id="+waiting); len(got) != 0 || e.tradeOf(t, waiting)["status"] != "pending" {
		t.Errorf("a delivery whose connector was disabled since: got %d messages, %v; want none, pending", len(got), e.tradeOf(t, waiting))
	}
}

// TestGTSList (S2s): the service's messages are listed by connector and
// status a page at a time; a page size out of range is refused, and the
// list refuses outside a request, as does the screening read shipping
// asks.
func TestGTSList(t *testing.T) {
	e := setupWMS(t)
	gts := e.withGTS(t)
	for range 2 {
		id, _ := e.delivery(t, e.p1, e.s1, "1")
		e.queued(t, id)
	}
	if got := e.gtsMessages(t, "connector_id="+gts+"&status=pending"); len(got) != 2 {
		t.Errorf("the pending screening requests: got %d, want 2", len(got))
	}
	first, _ := e.Must(200, "GET", "/v1/gts-messages?page_size=1", "", "")
	tok, _ := first["next_page_token"].(string)
	second, _ := e.Must(200, "GET", "/v1/gts-messages?page_size=1&page_token="+tok, "", "")
	if a, b := first["items"].([]any)[0].(map[string]any)["id"], second["items"].([]any)[0].(map[string]any)["id"]; tok == "" || a == b {
		t.Errorf("the pages: got %v then %v (token %q), want two messages", a, b, tok)
	}
	if got := e.gtsMessages(t, "connector_id="+e.conn); len(got) != 0 {
		t.Errorf("the wms connector's messages among the service's: got %d, want 0", len(got))
	}
	h := app.Integration(app.Config{Pages: wmsPages(t)})
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListGTSMessages(ctx, gen.ListGTSMessagesParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if p := problem.From(err); p.Code != "request.invalid_page_size" {
		t.Errorf("a page size of 0: got %v, want request.invalid_page_size", err)
	}
	if _, err := h.ListGTSMessages(context.Background(), gen.ListGTSMessagesParams{}); err == nil {
		t.Error("outside a request: got nil, want refused")
	}
	if _, err := (integration.TradeScreening{}).Screens(context.Background(), "2000", "P1"); err == nil {
		t.Error("the screening read outside a request: got nil, want refused")
	}
}
