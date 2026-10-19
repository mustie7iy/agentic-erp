package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
)

// drain runs rounds until one sends nothing.
func (x *dispatch) drain() {
	x.t.Helper()
	for range 20 {
		sent, _, err := x.d.RunOnce(context.Background())
		if err != nil {
			x.t.Fatalf("round: %v", err)
		}
		if sent == 0 {
			return
		}
	}
	x.t.Fatal("rounds kept sending")
}

// tag is the subscription's current ETag.
func (x *dispatch) tag(sub string) string {
	x.t.Helper()
	_, tag := x.c.Must(200, "GET", hooks+"/"+sub, "", "")
	return tag
}

func (x *dispatch) audits(object, action string) int {
	x.t.Helper()
	var n int
	pgtest.Scan(x.t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND action = $2`, []any{object, action}, &n)
	return n
}

// TestWebhookDeliveryReads: a subscription's deliveries are listed newest
// first, filtered by status and creation time, and read one by one; a
// delivery is found only under its own subscription.
func TestWebhookDeliveryReads(t *testing.T) {
	x := newDispatch(t)
	sub, _ := x.hook(x.c, `"ledger.*"`)
	other, _ := x.hook(x.c, `"payables.*"`)
	sent := x.event(x.c, uuid.New())
	x.round(1)
	held := x.event(x.c, uuid.New())
	list := hooks + "/" + sub + "/deliveries"
	for name, c := range map[string]struct {
		query string
		want  []string
	}{
		"all, newest first": {"", []string{held, sent}},
		"pending":           {"?status=pending", []string{held}},
		"since":             {"?since=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339), nil},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := x.c.In(t).Must(200, "GET", list+c.query, "", "")
			items := page["items"].([]any)
			if len(items) != len(c.want) {
				t.Fatalf("items: got %d, want %d", len(items), len(c.want))
			}
			for i, id := range c.want {
				if got := items[i].(map[string]any)["id"]; got != id {
					t.Errorf("item %d: got %v, want %s", i, got, id)
				}
			}
		})
	}
	if page, _ := x.c.Must(200, "GET", list+"?page_size=1", "", ""); page["next_page_token"] == nil {
		t.Errorf("first page of one: got no next page token")
	}
	got, tag := x.c.Must(200, "GET", list+"/"+sent, "", "")
	if got["status"] != "delivered" || got["attempts"] != float64(1) || got["last_status"] != float64(204) || got["type"] != posted {
		t.Errorf("delivery: got %v, want delivered once with 204", got)
	}
	if got["body"] != nil {
		t.Error("delivery: got its body, want it left out")
	}
	if tag == "" {
		t.Error("delivery: got no ETag")
	}
	if code, _, _ := x.c.Do("GET", list+"?page_token=bad", "", ""); code != 400 {
		t.Errorf("a bad page token: got %d, want 400", code)
	}
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET attempts = 1, last_error = 'no answer', version = version + 1 WHERE id = $1`, held)
	if got, _ := x.c.Must(200, "GET", list+"/"+held, "", ""); got["last_error"] != "no answer" || got["next_attempt_at"] == nil {
		t.Errorf("failed delivery: got %v, want its error and next attempt", got)
	}
	x.c.Problem(404, "request.not_found", "GET", hooks+"/"+other+"/deliveries/"+sent, "", "")
	x.c.Problem(404, "request.not_found", "GET", hooks+"/"+uuid.NewString()+"/deliveries", "", "")
}

// TestWebhookRedeliver: a delivered delivery is queued again as a first
// attempt and sent under its event's webhook-id; doing so twice leaves the
// same state; each is audited; a disabled or deleted subscription refuses.
func TestWebhookRedeliver(t *testing.T) {
	x := newDispatch(t)
	sub, _ := x.hook(x.c, `"ledger.*"`)
	d := x.event(x.c, uuid.New())
	x.round(1)
	path := hooks + "/" + sub + "/deliveries/" + d
	_, tag := x.c.Must(200, "GET", path, "", "")
	x.c.Problem(412, "request.version_mismatch", "POST", path+":redeliver", "", `"1"`)
	for i := range 2 {
		var got map[string]any
		got, tag = x.c.Must(200, "POST", path+":redeliver", "", tag)
		if got["status"] != "pending" || got["attempts"] != float64(0) || got["last_status"] != nil || got["delivered_at"] != nil {
			t.Errorf("redelivery %d: got %v, want pending with no attempt", i+1, got)
		}
	}
	x.round(1)
	reqs := x.rec.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: got %d, want 2", len(reqs))
	}
	if a, b := reqs[0].header.Get(webhook.HeaderID), reqs[1].header.Get(webhook.HeaderID); a != b {
		t.Errorf("webhook-id: got %s then %s, want the event's both times", a, b)
	}
	if row := x.delivery(d); row.status != "delivered" || row.attempts != 1 {
		t.Errorf("after the redelivery: got %s after %d, want delivered after 1", row.status, row.attempts)
	}
	if n := x.audits(d, "integration.webhook_delivery.redeliver"); n != 2 {
		t.Errorf("audit entries: got %d, want 2", n)
	}
	x.c.Problem(404, "request.not_found", "POST", hooks+"/"+sub+"/deliveries/"+uuid.NewString()+":redeliver", "", `"*"`)
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = 'gone', version = version + 1 WHERE id = $1`, sub)
	x.c.Problem(409, "integration.webhook_disabled", "POST", path+":redeliver", "", `"*"`)
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'deleted', disabled_reason = NULL, version = version + 1 WHERE id = $1`, sub)
	x.c.Problem(409, "integration.webhook_deleted", "POST", path+":redeliver", "", `"*"`)
}

// TestWebhookReplay: the deliveries of the events since an instant are
// queued again whatever their status, pings and earlier events aside, and
// sent in event order; replaying twice leaves the same state, a retried
// request answers the first answer; since is within the retention.
func TestWebhookReplay(t *testing.T) {
	x := newDispatch(t)
	sub, _ := x.hook(x.c, `"ledger.*"`)
	res := uuid.New()
	since := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	x.eventAt(x.c, res, since.Add(-time.Hour))
	first := x.eventAt(x.c, res, since.Add(time.Minute))
	second := x.eventAt(x.c, res, since.Add(2*time.Minute))
	other := x.eventAt(x.c, uuid.New(), since)
	x.c.Must(200, "POST", hooks+"/"+sub+":ping", "", x.tag(sub))
	x.drain()
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET status = 'dead', delivered_at = NULL, version = version + 1 WHERE id = $1`, second)
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET status = 'dropped', attempts = 0, delivered_at = NULL, version = version + 1 WHERE id = $1`, other)
	body := `{"since":"` + since.Format(time.RFC3339) + `"}`
	req := apitest.Req{Method: "POST", Path: hooks + "/" + sub + ":replay", Body: body, Tenant: x.c.Tenant.ID, As: x.c.As, Key: apitest.NewKey(), IfMatch: x.tag(sub)}
	for i, want := range []string{"", "true"} {
		r := x.c.API.Do(req)
		if got := apitest.JSON(t, r); r.Code != 200 || got["queued"] != float64(3) {
			t.Fatalf("replay %d: got %d %v, want 3 queued", i+1, r.Code, got)
		}
		if got := r.Header().Get("Idempotent-Replayed"); got != want {
			t.Errorf("replay %d: Idempotent-Replayed %q, want %q", i+1, got, want)
		}
	}
	if got, _ := x.c.Must(200, "POST", hooks+"/"+sub+":replay", body, x.tag(sub)); got["queued"] != float64(3) {
		t.Errorf("replay again: got %v, want the same 3", got)
	}
	for _, id := range []string{first, second, other} {
		if row := x.delivery(id); row.status != "pending" || row.attempts != 0 || row.lastStatus != nil {
			t.Errorf("delivery %s: got %+v, want pending with no attempt", id, row)
		}
	}
	before := len(x.rec.requests())
	x.drain()
	order := map[string]int{}
	for i, r := range x.rec.requests()[before:] {
		order[r.header.Get(integration.HeaderDeliveryID)] = i + 1
	}
	if len(order) != 3 || order[first] == 0 || order[first] > order[second] || order[other] == 0 {
		t.Errorf("sent: got %v, want the three replayed, %s before %s", order, first, second)
	}
	if n := x.audits(sub, "integration.webhook_subscription.replay"); n != 2 {
		t.Errorf("audit entries: got %d, want 2 (the retried request adds none)", n)
	}
	for name, at := range map[string]time.Time{"past the retention": time.Now().Add(-31 * 24 * time.Hour), "in the future": time.Now().Add(time.Hour)} {
		t.Run(name, func(t *testing.T) {
			x.c.In(t).Problem(422, "integration.invalid_replay_since", "POST", hooks+"/"+sub+":replay", `{"since":"`+at.UTC().Format(time.RFC3339)+`"}`, `"*"`)
		})
	}
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = 'failing', version = version + 1 WHERE id = $1`, sub)
	x.c.Problem(409, "integration.webhook_disabled", "POST", hooks+"/"+sub+":replay", body, x.tag(sub))
}

// ping pings the subscription and returns its delivery id.
func (x *dispatch) ping(sub string) string {
	x.t.Helper()
	got, tag := x.c.Must(200, "POST", hooks+"/"+sub+":ping", "", x.tag(sub))
	if tag != fmt.Sprintf(`"%v"`, got["version"]) {
		x.t.Errorf("ping ETag: got %s, want the delivery's version %v", tag, got["version"])
	}
	if got["status"] != "pending" || got["type"] != integration.PingType || got["subject"] != sub {
		x.t.Fatalf("ping: got %v, want a pending ping about the subscription", got)
	}
	return got["id"].(string)
}

// TestWebhookPing: a ping is a signed webhook.ping event sent by the
// dispatcher, not the request; it is attempted once, counts toward no
// failures, and goes out while the subscription is paused or disabled.
func TestWebhookPing(t *testing.T) {
	x := newDispatch(t)
	sub, secret := x.hook(x.c, `"ledger.*"`)
	ok := x.ping(sub)
	if n := len(x.rec.requests()); n != 0 {
		t.Fatalf("requests before the round: got %d, want 0", n)
	}
	x.round(1)
	r := x.rec.requests()[0]
	if err := (webhook.Verifier{Secrets: []webhook.Secret{secret}, Now: x.clock.now}).Verify(r.header, r.body); err != nil {
		t.Errorf("signature: %v", err)
	}
	var ev struct {
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(r.body, &ev); err != nil || ev.Type != integration.PingType || ev.Data["subscription_id"] != sub {
		t.Errorf("body %s (%v): want a webhook.ping about the subscription", r.body, err)
	}
	if row := x.delivery(ok); row.status != "delivered" {
		t.Errorf("ping: got %s, want delivered", row.status)
	}
	failed := x.ping(sub)
	x.rec.answerWith(failed, 500)
	x.round(1)
	if row := x.delivery(failed); row.status != "dead" || row.attempts != 1 {
		t.Errorf("failed ping: got %s after %d, want dead after 1", row.status, row.attempts)
	}
	if status, _, failing := x.subscription(sub); status != "active" || failing != nil {
		t.Errorf("after a failed ping: got %s failing since %v, want active and not failing", status, failing)
	}
	x.c.Must(200, "POST", hooks+"/"+sub+":pause", "", x.tag(sub))
	held := x.event(x.c, uuid.New())
	paused := x.ping(sub)
	x.round(1)
	if row := x.delivery(paused); row.status != "delivered" {
		t.Errorf("ping while paused: got %s, want delivered", row.status)
	}
	if row := x.delivery(held); row.attempts != 0 {
		t.Errorf("held event: got %d attempts, want 0", row.attempts)
	}
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = 'failing', version = version + 1 WHERE id = $1`, sub)
	gone := x.ping(sub)
	x.rec.answerWith(gone, 410)
	x.round(1)
	if row := x.delivery(gone); row.status != "dead" {
		t.Errorf("ping answered 410: got %s, want dead", row.status)
	}
	if status, reason, _ := x.subscription(sub); status != "disabled" || reason == nil || *reason != "failing" {
		t.Errorf("after a 410 to a ping: got %s (%v), want still disabled as failing", status, reason)
	}
	if n := x.audits(sub, "integration.webhook_subscription.ping"); n != 4 {
		t.Errorf("audit entries: got %d, want 4", n)
	}
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'deleted', disabled_reason = NULL, version = version + 1 WHERE id = $1`, sub)
	x.c.Problem(409, "integration.webhook_deleted", "POST", hooks+"/"+sub+":ping", "", x.tag(sub))
}

// TestWebhookPingNotDropped: disabling a subscription drops its pending
// deliveries but not a pending ping.
func TestWebhookPingNotDropped(t *testing.T) {
	x := newDispatch(t)
	sub, _ := x.hook(x.c, `"ledger.*"`)
	ping := x.ping(sub)
	// Not due yet, so the round below only disables.
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET next_attempt_at = $2, version = version + 1 WHERE id = $1`, ping, x.clock.now().Add(time.Hour))
	gone := x.event(x.c, uuid.New())
	x.rec.answerWith(gone, 410)
	x.round(1)
	if status, _, _ := x.subscription(sub); status != "disabled" {
		t.Fatalf("subscription: got %s, want disabled", status)
	}
	if row := x.delivery(ping); row.status != "pending" {
		t.Fatalf("ping after the disabling: got %s, want pending", row.status)
	}
	x.clock.set(x.clock.now().Add(2 * time.Hour))
	x.round(1)
	if row := x.delivery(ping); row.status != "delivered" {
		t.Errorf("ping: got %s, want delivered", row.status)
	}
}
