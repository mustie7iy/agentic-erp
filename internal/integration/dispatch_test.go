package integration_test

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
)

// testClock is a clock the test moves; the dispatcher reads it from its
// sending goroutines.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

// received is a request a receiver got.
type received struct {
	header http.Header
	body   []byte
}

// receiver is a subscriber's endpoint answering each delivery with the
// status answer gives its delivery id (204 when none).
type receiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []received
	answer map[string]int
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{answer: map[string]int{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("receiver: read body: %v", err)
		}
		r.mu.Lock()
		r.got = append(r.got, received{header: req.Header.Clone(), body: body})
		status, ok := r.answer[req.Header.Get(integration.HeaderDeliveryID)]
		if !ok {
			status = r.answer["*"]
		}
		r.mu.Unlock()
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// answerWith makes the receiver answer status to delivery id ("*": every
// one not named).
func (r *receiver) answerWith(id string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer[id] = status
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

// dispatch is a dispatcher test: a tenant, its endpoint and the clock.
type dispatch struct {
	t     *testing.T
	c     apitest.Client
	clock *testClock
	rec   *receiver
	d     *integration.Dispatcher
	log   *pgtest.SyncBuffer
}

// newDispatch starts with no deliveries in the database: the dispatcher
// sends every tenant's, and those of other tests name public addresses.
func newDispatch(t *testing.T) *dispatch {
	t.Helper()
	pgtest.Exec(t, `DELETE FROM integration.webhook_delivery`)
	rec := newReceiver(t)
	roots := x509.NewCertPool()
	roots.AddCert(rec.srv.Certificate())
	client, err := egress.New(egress.LoopbackForTests(roots))
	if err != nil {
		t.Fatal(err)
	}
	keys := apitest.FieldKeys()
	keys.Pool = pgtest.Pool(t)
	// Deliveries are due at the database's now; a whole second is stored
	// exactly.
	clock := &testClock{at: time.Now().Add(time.Minute).Truncate(time.Second)}
	var log pgtest.SyncBuffer
	return &dispatch{t: t, c: webhookClient(t), clock: clock, rec: rec, log: &log, d: &integration.Dispatcher{
		Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Keys: keys, Client: client, Now: clock.now,
		Log: slog.New(slog.NewTextHandler(&log, nil)),
	}}
}

// hook creates a subscription of the client to types, sending to the
// receiver, and returns its id and secret.
func (x *dispatch) hook(c apitest.Client, types string) (string, webhook.Secret) {
	x.t.Helper()
	created, _ := createHook(c, types)
	// The API refuses a loopback URL; the test endpoint is one.
	pgtest.Exec(x.t, `UPDATE integration.webhook_subscription SET url = $2, version = version + 1 WHERE id = $1`, created["id"], x.rec.srv.URL+"/hook?token=tok-1")
	s, err := webhook.ParseSecret(created["secret"].(string))
	if err != nil {
		x.t.Fatal(err)
	}
	return created["id"].(string), s
}

// event fans out an event of the client's tenant about subject and
// returns the id of its first delivery.
func (x *dispatch) event(c apitest.Client, subject uuid.UUID) string {
	x.t.Helper()
	return x.eventAt(c, subject, time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC))
}

// eventAt is event with the event's time.
func (x *dispatch) eventAt(c apitest.Client, subject uuid.UUID, at time.Time) string {
	x.t.Helper()
	m := busEvent(c.Tenant.ID, posted, "journal_entry_posted")
	m.Header["ce-subject"] = subject.String()
	m.Header["ce-time"] = at.Format(time.RFC3339Nano)
	f := &integration.Fanout{Pool: pgtest.Pool(x.t), AppRole: "erp_app", Log: slog.New(slog.DiscardHandler)}
	if err := f.Handle(context.Background(), m); err != nil {
		x.t.Fatal(err)
	}
	var id string
	pgtest.Scan(x.t, `SELECT coalesce(min(id::text), '') FROM integration.webhook_delivery WHERE event_id = $1`, []any{m.ID}, &id)
	return id
}

// round runs one round and checks how many it sent.
func (x *dispatch) round(want int) {
	x.t.Helper()
	sent, _, err := x.d.RunOnce(context.Background())
	if err != nil {
		x.t.Fatalf("round: %v", err)
	}
	if sent != want {
		x.t.Fatalf("round sent %d, want %d", sent, want)
	}
}

// deliveryRow is a delivery as stored.
type deliveryRow struct {
	status     string
	attempts   int
	next       *time.Time
	lastStatus *int
	lastError  *string
}

func (x *dispatch) delivery(id string) deliveryRow {
	x.t.Helper()
	var r deliveryRow
	pgtest.Scan(x.t, `SELECT status, attempts, next_attempt_at, last_status, last_error FROM integration.webhook_delivery WHERE id = $1`,
		[]any{id}, &r.status, &r.attempts, &r.next, &r.lastStatus, &r.lastError)
	return r
}

func (x *dispatch) subscription(id string) (status string, reason *string, failingSince *time.Time) {
	x.t.Helper()
	pgtest.Scan(x.t, `SELECT status, disabled_reason, failing_since FROM integration.webhook_subscription WHERE id = $1`,
		[]any{id}, &status, &reason, &failingSince)
	return status, reason, failingSince
}

func (x *dispatch) version(id string) int {
	x.t.Helper()
	var v int
	pgtest.Scan(x.t, `SELECT version FROM integration.webhook_subscription WHERE id = $1`, []any{id}, &v)
	return v
}

// TestWebhookDispatchDelivers: a due delivery is sent once, signed with
// the current and the previous secret during a rotation, as the
// structured CloudEvent; a second round sends nothing more.
func TestWebhookDispatchDelivers(t *testing.T) {
	x := newDispatch(t)
	id, first := x.hook(x.c, `"ledger.*"`)
	_, tag := x.c.Must(200, "GET", hooks+"/"+id, "", "")
	rotated, _ := x.c.Must(200, "POST", hooks+"/"+id+":rotateSecret", "", tag)
	second, err := webhook.ParseSecret(rotated["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	d := x.event(x.c, uuid.New())
	x.round(1)
	x.round(0)
	got := x.rec.requests()
	if len(got) != 1 {
		t.Fatalf("requests: got %d, want 1", len(got))
	}
	r := got[0]
	var body []byte
	pgtest.Scan(t, `SELECT body FROM integration.webhook_delivery WHERE id = $1`, []any{d}, &body)
	if string(r.body) != string(body) {
		t.Errorf("body: got %s, want the stored %s", r.body, body)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/cloudevents+json" {
		t.Errorf("content type: got %q, want application/cloudevents+json", ct)
	}
	var event string
	pgtest.Scan(t, `SELECT event_id::text FROM integration.webhook_delivery WHERE id = $1`, []any{d}, &event)
	if h := r.header.Get(webhook.HeaderID); h != event {
		t.Errorf("webhook-id: got %q, want the event id %s", h, event)
	}
	if h := r.header.Get(integration.HeaderDeliveryID); h != d {
		t.Errorf("webhook-delivery-id: got %q, want the delivery id %s", h, d)
	}
	for name, s := range map[string]webhook.Secret{"new secret": second, "previous secret": first} {
		if err := (webhook.Verifier{Secrets: []webhook.Secret{s}, Now: x.clock.now}).Verify(r.header, r.body); err != nil {
			t.Errorf("%s: %v, want verified", name, err)
		}
	}
	row := x.delivery(d)
	if row.status != "delivered" || row.attempts != 1 || row.lastStatus == nil || *row.lastStatus != 204 {
		t.Errorf("delivery: got %s after %d attempts (%v), want delivered after 1 with 204", row.status, row.attempts, row.lastStatus)
	}
}

// TestWebhookDispatchRetries: a failing endpoint is retried on the
// schedule with jitter until the window passes, then the delivery is dead
// lettered; failing five days disables the subscription, audited.
func TestWebhookDispatchRetries(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	x.rec.answerWith("*", http.StatusInternalServerError)
	d := x.event(x.c, uuid.New())
	start := x.clock.now()
	before := x.version(id)
	waits := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour,
		12 * time.Hour, 12 * time.Hour, 12 * time.Hour, 12 * time.Hour, 12 * time.Hour}
	for i, wait := range waits {
		x.round(1)
		row := x.delivery(d)
		if row.status != "pending" || row.attempts != i+1 || row.next == nil {
			t.Fatalf("attempt %d: got %s after %d, want pending with a next attempt", i+1, row.status, row.attempts)
		}
		if got := row.next.Sub(x.clock.now()); got < wait || got >= wait+wait/10 {
			t.Errorf("wait after attempt %d: got %v, want %v plus under 10 %%", i+1, got, wait)
		}
		x.round(0) // not due yet
		x.clock.set(*row.next)
	}
	x.round(1)
	if row := x.delivery(d); row.status != "dead" || row.attempts != 11 || row.lastStatus == nil || *row.lastStatus != 500 {
		t.Fatalf("after the window: got %s after %d attempts, want dead after 11 with 500", row.status, row.attempts)
	}
	status, _, since := x.subscription(id)
	if status != "active" || since == nil || !since.Equal(start) {
		t.Fatalf("subscription: got %s failing since %v, want active failing since %v", status, since, start)
	}
	if v := x.version(id); v != before {
		t.Errorf("version after failures: got %d, want %d (failing_since is bookkeeping)", v, before)
	}
	x.clock.set(start.Add(integration.FailingLimit))
	last := x.event(x.c, uuid.New())
	x.round(1)
	status, reason, _ := x.subscription(id)
	if status != "disabled" || reason == nil || *reason != "failing" {
		t.Errorf("after five days failing: got %s (%v), want disabled as failing", status, reason)
	}
	if row := x.delivery(last); row.status != "dropped" {
		t.Errorf("the delivery retried as it was disabled: got %s, want dropped", row.status)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND action = 'integration.webhook_subscription.disable'`, []any{id}, &audited)
	if audited != 1 {
		t.Errorf("disable audit records: got %d, want 1", audited)
	}
	if !strings.Contains(x.log.String(), "webhook subscription disabled") {
		t.Errorf("log %q, want the disabling", x.log.String())
	}
}

// TestWebhookDispatchSuccessClearsFailing: a delivery after failures
// clears failing_since; an endpoint that cannot be reached is recorded with
// the error and no status.
func TestWebhookDispatchSuccessClearsFailing(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET url = $2, version = version + 1 WHERE id = $1`, id, closed.URL)
	d := x.event(x.c, uuid.New())
	x.round(1)
	row := x.delivery(d)
	if row.status != "pending" || row.lastStatus != nil || row.lastError == nil || *row.lastError == "" {
		t.Fatalf("unreachable: got %s, status %v, error %v; want pending with an error and no status", row.status, row.lastStatus, row.lastError)
	}
	if _, _, since := x.subscription(id); since == nil {
		t.Fatal("failing_since: got none, want the first failure")
	}
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET url = $2, version = version + 1 WHERE id = $1`, id, x.rec.srv.URL)
	x.clock.set(*row.next)
	x.round(1)
	if row := x.delivery(d); row.status != "delivered" || row.lastError != nil {
		t.Errorf("delivery: got %s with error %v, want delivered without one", row.status, row.lastError)
	}
	if _, _, since := x.subscription(id); since != nil {
		t.Errorf("failing_since: got %v, want cleared", since)
	}
}

// TestWebhookDispatchGone: a 410 dead letters the delivery and disables
// the subscription at once, audited.
func TestWebhookDispatchGone(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	x.rec.answerWith("*", http.StatusGone)
	resource := uuid.New()
	before := x.version(id)
	d := x.event(x.c, resource)
	waiting := x.event(x.c, resource)
	x.round(1)
	if row := x.delivery(d); row.status != "dead" || row.attempts != 1 {
		t.Errorf("delivery: got %s after %d, want dead after 1", row.status, row.attempts)
	}
	if row := x.delivery(waiting); row.status != "dropped" || row.attempts != 0 {
		t.Errorf("waiting delivery: got %s after %d, want dropped unsent", row.status, row.attempts)
	}
	if v := x.version(id); v != before+1 {
		t.Errorf("version: got %d, want %d (disabling is a visible change)", v, before+1)
	}
	if status, reason, _ := x.subscription(id); status != "disabled" || reason == nil || *reason != "gone" {
		t.Errorf("subscription: got %s (%v), want disabled as gone", status, reason)
	}
	var actor uuid.UUID
	pgtest.Scan(t, `SELECT actor_id FROM audit.audit_event WHERE object_id = $1 AND action = 'integration.webhook_subscription.disable'`, []any{id}, &actor)
	if actor != integration.DispatchActor {
		t.Errorf("disable audited as %s, want the dispatcher %s", actor, integration.DispatchActor)
	}
}

// TestWebhookResumeDisabled: resuming a disabled subscription makes it
// active with its failure tracking reset; what was dropped stays dropped
// and later events are sent.
func TestWebhookResumeDisabled(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	resource := uuid.New()
	x.event(x.c, resource)
	dropped := x.event(x.c, resource)
	x.rec.answerWith("*", http.StatusGone)
	x.round(1)
	x.rec.answerWith("*", http.StatusNoContent)
	_, tag := x.c.Must(200, "GET", hooks+"/"+id, "", "")
	resumed, _ := x.c.Must(200, "POST", hooks+"/"+id+":resume", "", tag)
	for _, field := range []string{"disabled_reason", "failing_since"} {
		if v, ok := resumed[field]; ok {
			t.Errorf("%s: got %v, want none", field, v)
		}
	}
	if resumed["status"] != "active" {
		t.Errorf("status: got %v, want active", resumed["status"])
	}
	x.round(0)
	if row := x.delivery(dropped); row.status != "dropped" {
		t.Errorf("dropped delivery: got %s, want still dropped", row.status)
	}
	later := x.event(x.c, resource)
	x.round(1)
	if row := x.delivery(later); row.status != "delivered" {
		t.Errorf("later event: got %s, want delivered", row.status)
	}
}

// TestWebhookDispatchEventOrder: deliveries fanned out in the wrong order
// (two replicas) are sent in the order of their events.
func TestWebhookDispatchEventOrder(t *testing.T) {
	x := newDispatch(t)
	x.hook(x.c, `"ledger.*"`)
	resource := uuid.New()
	at := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	second := x.eventAt(x.c, resource, at.Add(time.Second))
	first := x.eventAt(x.c, resource, at)
	x.round(1)
	if row := x.delivery(first); row.status != "delivered" {
		t.Fatalf("earlier event: got %s, want delivered first", row.status)
	}
	if row := x.delivery(second); row.attempts != 0 {
		t.Fatalf("later event: got %d attempts, want it waiting", row.attempts)
	}
	x.round(1)
	if row := x.delivery(second); row.status != "delivered" {
		t.Errorf("later event: got %s, want delivered next", row.status)
	}
}

// TestWebhookDispatchOrder: a resource's later delivery waits for the
// earlier one, until it is delivered or dead lettered; another resource's
// is not held up.
func TestWebhookDispatchOrder(t *testing.T) {
	x := newDispatch(t)
	x.hook(x.c, `"ledger.*"`)
	resource := uuid.New()
	first := x.event(x.c, resource)
	later := x.event(x.c, resource)
	other := x.event(x.c, uuid.New())
	x.rec.answerWith(first, http.StatusServiceUnavailable)
	x.round(2)
	if row := x.delivery(later); row.attempts != 0 {
		t.Fatalf("later delivery: got %d attempts, want it waiting", row.attempts)
	}
	if row := x.delivery(other); row.status != "delivered" {
		t.Errorf("other resource: got %s, want delivered", row.status)
	}
	// One attempt before the window ends, the next dead letters it.
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET attempts = 10, version = version + 1 WHERE id = $1`, first)
	x.clock.set(*x.delivery(first).next)
	x.round(1)
	if row := x.delivery(first); row.status != "dead" {
		t.Fatalf("first: got %s, want dead", row.status)
	}
	x.round(1)
	if row := x.delivery(later); row.status != "delivered" {
		t.Errorf("later after the dead letter: got %s, want delivered", row.status)
	}
}

// TestWebhookDispatchLimits: a round sends at most ten of a tenant's
// deliveries, each tenant's first before any second, and at most 50 a
// second to one subscription.
func TestWebhookDispatchLimits(t *testing.T) {
	x := newDispatch(t)
	x.hook(x.c, `"ledger.*"`)
	for range 60 {
		x.event(x.c, uuid.New())
	}
	for range 5 {
		x.round(integration.TenantInFlight)
	}
	x.round(0) // the subscription's 50 this second are spent
	x.clock.set(x.clock.now().Add(time.Second))
	x.round(integration.TenantInFlight)

	pgtest.Exec(t, `DELETE FROM integration.webhook_delivery`)
	other := webhookClient(t)
	x.hook(other, `"ledger.*"`)
	x.clock.set(x.clock.now().Add(time.Second))
	for range 3 {
		x.event(x.c, uuid.New())
	}
	theirs := x.event(other, uuid.New())
	x.d.Batch = 2
	sent, more, err := x.d.RunOnce(context.Background())
	if err != nil || sent != 2 || !more {
		t.Fatalf("round of 2: got %d sent (more %v, %v), want 2 and more", sent, more, err)
	}
	if row := x.delivery(theirs); row.status != "delivered" {
		t.Errorf("the other tenant's delivery: got %s, want delivered in the first round", row.status)
	}
}

// TestWebhookPauseHolds: a paused subscription's deliveries are recorded
// but held, and sent on resume; pausing and resuming are audited and
// refused from the wrong status.
func TestWebhookPauseHolds(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	_, tag := x.c.Must(200, "GET", hooks+"/"+id, "", "")
	x.c.Problem(409, "integration.webhook_already_active", "POST", hooks+"/"+id+":resume", "", tag)
	paused, tag := x.c.Must(200, "POST", hooks+"/"+id+":pause", "", tag)
	if paused["status"] != "paused" {
		t.Fatalf("pause: got %v, want paused", paused["status"])
	}
	x.c.Problem(409, "integration.webhook_not_active", "POST", hooks+"/"+id+":pause", "", tag)
	d := x.event(x.c, uuid.New())
	x.round(0)
	if row := x.delivery(d); row.status != "pending" || row.attempts != 0 {
		t.Fatalf("held: got %s after %d, want pending unsent", row.status, row.attempts)
	}
	resumed, _ := x.c.Must(200, "POST", hooks+"/"+id+":resume", "", tag)
	if resumed["status"] != "active" {
		t.Fatalf("resume: got %v, want active", resumed["status"])
	}
	x.round(1)
	if row := x.delivery(d); row.status != "delivered" {
		t.Errorf("after resume: got %s, want delivered", row.status)
	}
	var actions string
	pgtest.Scan(t, `SELECT string_agg(action, ',' ORDER BY occurred_at, action) FROM audit.audit_event
		WHERE object_id = $1 AND action IN ('integration.webhook_subscription.pause', 'integration.webhook_subscription.resume')`, []any{id}, &actions)
	if actions != "integration.webhook_subscription.pause,integration.webhook_subscription.resume" {
		t.Errorf("audit: got %s, want pause then resume", actions)
	}
	_, tag = x.c.Must(200, "GET", hooks+"/"+id, "", "")
	x.c.Must(200, "POST", hooks+"/"+id+":delete", "", tag)
	_, tag = x.c.Must(200, "GET", hooks+"/"+id, "", "")
	x.c.Problem(409, "integration.webhook_deleted", "POST", hooks+"/"+id+":pause", "", tag)
}

// TestWebhookPauseStopsFailingClock: time paused does not count toward
// FailingLimit. Pausing clears failing_since and the count starts again
// from the first failure after the resume.
func TestWebhookPauseStopsFailingClock(t *testing.T) {
	x := newDispatch(t)
	id, _ := x.hook(x.c, `"ledger.*"`)
	x.rec.answerWith("*", http.StatusInternalServerError)
	d := x.event(x.c, uuid.New())
	x.round(1)
	if _, _, since := x.subscription(id); since == nil {
		t.Fatal("failing_since before the pause: got none, want the first failure")
	}
	_, tag := x.c.Must(200, "GET", hooks+"/"+id, "", "")
	paused, tag := x.c.Must(200, "POST", hooks+"/"+id+":pause", "", tag)
	if v, ok := paused["failing_since"]; ok {
		t.Errorf("failing_since after the pause: got %v, want none", v)
	}
	x.clock.set(x.clock.now().Add(integration.FailingLimit + time.Hour))
	x.c.Must(200, "POST", hooks+"/"+id+":resume", "", tag)
	x.round(1)
	status, _, since := x.subscription(id)
	if status != "active" {
		t.Errorf("status after a failure past the limit counted from before the pause: got %s, want active", status)
	}
	if since == nil || !since.Equal(x.clock.now()) {
		t.Errorf("failing_since after the resume: got %v, want the first failure after it, %v", since, x.clock.now())
	}
	if row := x.delivery(d); row.status != "pending" || row.attempts != 2 {
		t.Errorf("delivery: got %s after %d, want pending after 2", row.status, row.attempts)
	}
}

// TestWebhookDispatchPrune: deliveries past the retention are deleted,
// held ones too, in batches: a full batch says more may be left; a last
// pruning finds none.
func TestWebhookDispatchPrune(t *testing.T) {
	x := newDispatch(t)
	x.hook(x.c, `"ledger.*"`)
	old := []string{x.event(x.c, uuid.New()), x.event(x.c, uuid.New())}
	kept := x.event(x.c, uuid.New())
	pgtest.Exec(t, `UPDATE integration.webhook_delivery SET created_at = $2, version = version + 1 WHERE id = ANY ($1::uuid[])`, old, x.clock.now().Add(-integration.DeliveryRetention-time.Minute))
	x.d.PruneBatch = 1
	for i, want := range []struct {
		n    int64
		more bool
	}{{1, true}, {1, true}, {0, false}} {
		n, more, err := x.d.Prune(context.Background())
		if err != nil {
			t.Fatalf("prune %d: %v", i+1, err)
		}
		if n != want.n || more != want.more {
			t.Errorf("prune %d: got %d (more %v), want %d (more %v)", i+1, n, more, want.n, want.more)
		}
	}
	var left []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(id::text), '{}') FROM integration.webhook_delivery`, nil, &left)
	if len(left) != 1 || left[0] != kept {
		t.Errorf("left: got %v, want only %s", left, kept)
	}
}

// faults injects failures into the dispatcher's transactions: the begin
// numbered beginAt, the statement numbered failAt across all, or the
// commit of the transaction numbered commitAt.
type faults struct {
	mu                        sync.Mutex
	begins, count             int
	beginAt, failAt, commitAt int
}

func (f *faults) begin(pool *database.Pool) database.BeginFunc {
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.begins++
		if f.begins == f.beginAt {
			return nil, pgtest.ErrInjected
		}
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: f.failAt, Count: &f.count, FailCommit: f.begins == f.commitAt}, nil
	}
}

// TestWebhookDispatchDatabaseFailures: a failure at any begin, statement or
// commit of a round, for a delivery that succeeds, fails, or is gone, is
// reported and leaves the delivery to a later round, which sends it.
func TestWebhookDispatchDatabaseFailures(t *testing.T) {
	for name, status := range map[string]int{"delivered after failing": 204, "failing": 500, "gone": 410} {
		t.Run(name, func(t *testing.T) {
			// One tenant and endpoint for every fault: each run starts
			// from a fresh delivery and an active subscription.
			x := newDispatch(t)
			id, _ := x.hook(x.c, `"ledger.*"`)
			x.rec.answerWith("*", status)
			for _, kind := range []string{"begin", "statement", "commit"} {
				// The first transaction only reads: its commit is never
				// reached, so commits are tried up to the third.
				for n := 1; ; n++ {
					f := &faults{}
					switch kind {
					case "begin":
						f.beginAt = n
					case "statement":
						f.failAt = n
					default:
						f.commitAt = n
					}
					if !x.failOnce(id, status, f) && (kind != "commit" || n >= 3) {
						break
					}
				}
			}
		})
	}
}

// failOnce runs a round under the faults and reports whether one hit; when
// one did, the next round sends the delivery and records it.
func (x *dispatch) failOnce(id string, status int, f *faults) bool {
	t := x.t
	t.Helper()
	pgtest.Exec(t, `DELETE FROM integration.webhook_delivery`)
	failing := "NULL"
	if status == 204 {
		failing = "now()"
	}
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'active', disabled_reason = NULL, failing_since = `+failing+`,
		version = version + 1 WHERE id = $1`, id)
	x.clock.set(x.clock.now().Add(3 * time.Minute)) // past any lease, with the rate refilled
	d := x.event(x.c, uuid.New())
	x.d.Begin = f.begin(pgtest.Pool(t))
	_, _, err := x.d.RunOnce(context.Background())
	x.d.Begin = nil
	if err == nil {
		if row := x.delivery(d); row.attempts != 1 {
			t.Fatalf("%+v: no fault hit, delivery attempted %d times, want 1", f, row.attempts)
		}
		return false
	}
	if !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("%+v: got %v, want the injected failure", f, err)
	}
	x.clock.set(x.clock.now().Add(3 * time.Minute)) // past a lease the failed round took
	x.round(1)
	if row := x.delivery(d); row.attempts != 1 {
		t.Errorf("%+v: after recovery the delivery has %d attempts, want 1", f, row.attempts)
	}
	return true
}

// TestWebhookDispatchPruneFailures: a failed pruning is reported.
func TestWebhookDispatchPruneFailures(t *testing.T) {
	for _, f := range []*faults{{beginAt: 1}, {failAt: 1}, {commitAt: 1}} {
		x := newDispatch(t)
		x.d.Begin = f.begin(pgtest.Pool(t))
		if _, _, err := x.d.Prune(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("%+v: got %v, want the injected failure", f, err)
		}
	}
}

// TestWebhookDispatchRun: the loop sends due deliveries and logs a failed
// round.
func TestWebhookDispatchRun(t *testing.T) {
	x := newDispatch(t)
	x.hook(x.c, `"ledger.*"`)
	d := x.event(x.c, uuid.New())
	x.d.Begin = (&faults{beginAt: 1}).begin(pgtest.Pool(t))
	x.d.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { x.d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	pgtest.Eventually(t, 10*time.Second, func() bool { return x.delivery(d).status == "delivered" }, "delivery %s not sent", d)
	cancel()
	<-done
	if !strings.Contains(x.log.String(), "failed") {
		t.Errorf("log %q, want the failed round", x.log.String())
	}
}
