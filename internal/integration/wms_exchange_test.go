package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// wmsRow is the delivery's outbound message's status, attempts and last
// error as stored.
func (e wmsEnv) wmsRow(t *testing.T, delivery string) (status string, attempts int, lastError string) {
	t.Helper()
	pgtest.Scan(t, `SELECT status, attempts, coalesce(last_error, '') FROM integration.wms_message WHERE tenant_id = $1 AND document_id = $2
		AND direction = 'outbound'`, []any{e.Tenant.ID, delivery}, &status, &attempts, &lastError)
	return status, attempts, lastError
}

// due makes the delivery's pending message due now, at attempts.
func (e wmsEnv) due(t *testing.T, delivery string, attempts int) {
	t.Helper()
	pgtest.Exec(t, `UPDATE integration.wms_message SET attempts = $3, next_attempt_at = now() - interval '1 second', version = version + 1
		WHERE tenant_id = $1 AND document_id = $2 AND status = 'pending'`, e.Tenant.ID, delivery, attempts)
}

// queuedDelivery is a new delivery at P1, its message queued.
func (e wmsEnv) queuedDelivery(t *testing.T) string {
	t.Helper()
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	return id
}

// failingSecrets is a vault that cannot be reached.
type failingSecrets struct{}

func (failingSecrets) Resolve(context.Context, string) (string, error) {
	return "", errors.New("vault down")
}

// TestWMSSendOutcomes: a failed call is tried again after its backoff,
// and the sixth fails the message; the warehouse system's refusal fails
// it at once; a connector that cannot be used, credentials that do not
// resolve, a vault or a client that cannot be reached are failed
// attempts; a lapsed last claim fails it as of unknown outcome; a claim
// taken over while its call ran records nothing.
func TestWMSSendOutcomes(t *testing.T) {
	e := setupWMS(t)
	at := time.Now().Add(time.Minute) // after the queue's time, which the messages are due at
	x := e.exchange(t, func() time.Time { return at })
	send := func(t *testing.T) {
		t.Helper()
		if _, err := x.SendOnce(context.Background()); err != nil {
			t.Fatalf("send: got %v, want nil", err)
		}
	}
	t.Run("tried again after its backoff, failed after the sixth", func(t *testing.T) {
		id := e.queuedDelivery(t)
		e.wms.Fail("PUT /messages", 6, http.StatusServiceUnavailable)
		var log pgtest.SyncBuffer
		x.Log = slog.New(slog.NewTextHandler(&log, nil))
		defer func() { x.Log = nil }()
		for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour} {
			send(t)
			if s, n, code := e.wmsRow(t, id); s != "pending" || n != i+1 || code != "platform.internal" {
				t.Fatalf("after attempt %d: got %s, %d, %s; want pending, %d, platform.internal", i+1, s, n, code, i+1)
			}
			at = at.Add(wait)
		}
		send(t)
		if s, n, _ := e.wmsRow(t, id); s != "failed" || n != 6 {
			t.Errorf("after the sixth: got %s, %d; want failed, 6", s, n)
		}
	})
	for name, c := range map[string]struct {
		change func(t *testing.T, x *integration.WMSExchange, id string)
		status string
		code   string
	}{
		"refused by the WMS": {func(t *testing.T, _ *integration.WMSExchange, _ string) {
			t.Helper()
			e.wms.Fail("PUT /messages", 1, http.StatusBadRequest)
		},
			"failed", "integration.wms_refused"},
		"the connector disabled": {func(t *testing.T, _ *integration.WMSExchange, _ string) {
			t.Helper()
			e.connectorSet(t, `status = 'disabled'`)
		}, "pending",
			"integration.connector_disabled"},
		"no secret_ref": {func(t *testing.T, _ *integration.WMSExchange, _ string) {
			t.Helper()
			e.connectorSet(t, `secret_ref = NULL`)
		}, "pending", "integration.secret_required"},
		"settings changed by hand": {func(t *testing.T, _ *integration.WMSExchange, _ string) {
			t.Helper()
			e.connectorSet(t, `settings = '{"adapter": "acme"}'`)
		}, "pending",
			"integration.invalid_settings"},
		"the stand-in in production": {func(_ *testing.T, x *integration.WMSExchange, _ string) { x.RefuseStandIn = true }, "pending",
			"integration.adapter_not_certified"},
		"credentials not the tenant's": {func(_ *testing.T, x *integration.WMSExchange, _ string) { x.Secrets = secretsNone{} }, "pending",
			"integration.secret_unavailable"},
		"the vault down": {func(_ *testing.T, x *integration.WMSExchange, _ string) { x.Secrets = failingSecrets{} }, "pending", "platform.internal"},
		"no client":      {func(_ *testing.T, x *integration.WMSExchange, _ string) { x.Adapter = integration.WMSAdapters(nil) }, "pending", "platform.unavailable"},
		"a lapsed last attempt": {func(t *testing.T, _ *integration.WMSExchange, id string) { t.Helper(); e.due(t, id, 6) }, "failed",
			"integration.wms_outcome_unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			e.connectorSet(t, `status = 'active', secret_ref = '`+e.ref+`', settings = '{"adapter": "standin", "base_url": "`+e.wms.URL+`", "plants": {"2000": ["P1"]}}'`)
			id := e.queuedDelivery(t)
			x := e.exchange(t, func() time.Time { return at })
			c.change(t, x, id)
			if _, err := x.SendOnce(context.Background()); err != nil {
				t.Fatalf("send: got %v, want nil", err)
			}
			if s, _, code := e.wmsRow(t, id); s != c.status || code != c.code {
				t.Errorf("got %s, %s; want %s, %s", s, code, c.status, c.code)
			}
		})
	}
	e.connectorSet(t, `status = 'active', secret_ref = '`+e.ref+`', settings = '{"adapter": "standin", "base_url": "`+e.wms.URL+`", "plants": {"2000": ["P1"]}}'`)
	t.Run("a connector of another kind", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.wms_message SET status = 'sent', sent_at = now(), next_attempt_at = NULL, attempts = 1, version = version + 1
			WHERE tenant_id = $1 AND status = 'pending'`, e.Tenant.ID)
		other, msg, doc := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, $2, 'email', 'Mail', '{}', 'active', $3, $3)`, e.Tenant.ID, other, e.Tenant.Human.ID)
		pgtest.Exec(t, `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
				document_number, plant_code, body, status, next_attempt_at)
			VALUES ($1, $2, $3, 'outbound', $4, 'outbound_delivery', $5, $6, 'OD9', 'P1', '{}', 'pending', now() - interval '1 second')`,
			e.Tenant.ID, msg, other, msg.String(), e.DE, doc)
		send(t)
		if s, _, code := e.wmsRow(t, doc.String()); s != "pending" || code != "integration.not_a_wms" {
			t.Errorf("got %s, %s; want pending, integration.not_a_wms", s, code)
		}
	})
	t.Run("a claim taken over", func(t *testing.T) {
		id2, _ := e.delivery(t, e.p1, e.s1, "1")
		e.queued(t, id2)
		e.wms.Fail("PUT /messages", 0, http.StatusServiceUnavailable)
		x := e.exchange(t, time.Now)
		orig := x.Adapter
		var once sync.Once
		x.Adapter = func(c integration.WMSConnection) (integration.Warehouse, error) {
			w, err := orig(c)
			// Taken over once: the taking claim's lease (here, a time in
			// the past) would make the next round claim it again.
			return takenOver{Warehouse: w, take: func() {
				once.Do(func() {
					pgtest.Exec(t, `UPDATE integration.wms_message SET attempts = 3, next_attempt_at = now() + interval '1 hour', version = version + 1
						WHERE tenant_id = $1 AND document_id = $2`, e.Tenant.ID, id2)
				})
			}}, err
		}
		if _, err := x.SendOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s, n, _ := e.wmsRow(t, id2); s != "pending" || n != 3 {
			t.Errorf("got %s after %d attempts, want pending as the taking claim left it (3)", s, n)
		}
	})
}

// secretsNone resolves nothing: the reference names no secret.
type secretsNone struct{}

func (secretsNone) Resolve(context.Context, string) (string, error) { return "", secrets.ErrNotFound }

// takenOver is a warehouse whose sends let another claim take the
// message over while they run.
type takenOver struct {
	integration.Warehouse
	take func()
}

func (w takenOver) Send(ctx context.Context, id string, body []byte) error {
	w.take()
	return w.Warehouse.Send(ctx, id, body)
}

// connectorSet changes the connector as an operator would by hand.
func (e wmsEnv) connectorSet(t *testing.T, set string) {
	t.Helper()
	pgtest.Exec(t, `UPDATE integration.connector SET `+set+`, version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.conn)
}

// TestWMSConfirmationOutcomes: a confirmation that is not a goods issue
// of a delivery the connector was sent, or that shipping refuses, is
// recorded refused with its problem and acknowledged, nothing of it
// posted; one that is not a JSON object is kept as its text.
func TestWMSConfirmationOutcomes(t *testing.T) {
	e := setupWMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "2")
	e.queued(t, id)
	other, _ := e.delivery(t, e.p2, e.s2, "1")
	x := e.exchange(t, time.Now)
	for name, c := range map[string]struct {
		body []byte
		code string
		text string // the body kept as {"text": ...}; "" when kept as sent
	}{
		"not JSON":               {[]byte("hello"), "integration.wms_message_unreadable", "hello"},
		"not text":               {[]byte{'a', 0xff, 0xfe, 0}, "integration.wms_message_unreadable", "a"},
		"a NUL in an object":     {[]byte(`{"message_id": "M-nul", "type": "goods_issue", "note": "a\u0000b"}`), "integration.wms_message_unreadable", `{"message_id": "M-nul", "type": "goods_issue", "note": "a\u0000b"}`},
		"a JSON array":           {[]byte(`[1]`), "integration.wms_message_unreadable", "[1]"},
		"another type":           {[]byte(`{"message_id": "M-type", "type": "goods_receipt", "outbound_delivery_id": "` + id + `"}`), "integration.wms_message_unreadable", ""},
		"another message id":     {[]byte(`{"message_id": "M-x", "type": "goods_issue", "outbound_delivery_id": "` + id + `"}`), "integration.wms_message_unreadable", ""},
		"no delivery":            {[]byte(`{"message_id": "M-none", "type": "goods_issue"}`), "integration.wms_message_unreadable", ""},
		"items not a list":       {[]byte(`{"message_id": "M-items", "type": "goods_issue", "items": "all"}`), "integration.wms_message_unreadable", ""},
		"a delivery not sent it": {goodsIssue("M-other", other, "1 1 BOX"), "integration.wms_unknown_delivery", ""},
		"more than delivered":    {goodsIssue("M-more", id, "1 9 BOX"), "shipping.exceeds_delivery_quantity", ""},
		"nothing picked":         {goodsIssue("M-nothing", id, "1 0 BOX"), "shipping.nothing_picked", ""},
	} {
		t.Run(name, func(t *testing.T) {
			mid := "M-" + strings.ReplaceAll(name, " ", "-")
			switch name {
			case "a NUL in an object", "items not a list", "another type", "no delivery", "a delivery not sent it", "more than delivered", "nothing picked":
				mid = strings.Split(strings.Split(string(c.body), `"message_id": "`)[1], `"`)[0]
			}
			e.wms.Report(mid, c.body)
			if n, err := x.PollOnce(context.Background()); err != nil || n != 1 {
				t.Fatalf("poll: got %d, %v; want 1 recorded", n, err)
			}
			in := e.inbound(t, mid)
			if in["status"] != "refused" || in["problem_code"] != c.code || in["acknowledged_at"] == nil {
				t.Errorf("got %v %v, acknowledged %v; want refused %s, acknowledged", in["status"], in["problem_code"], in["acknowledged_at"], c.code)
			}
			if text, _ := in["body"].(map[string]any)["text"].(string); text != c.text {
				t.Errorf("the body kept as text: got %q, want %q", text, c.text)
			}
		})
	}
	d, _ := e.Must(200, "GET", "/v1/outbound-deliveries/"+id, "", "")
	if d["status"] != "created" || d["picking_status"] != "not_picked" {
		t.Errorf("the delivery after the refused ones: got %v, %v; want created, not_picked (nothing of them kept)", d["status"], d["picking_status"])
	}
	t.Run("too large", func(t *testing.T) {
		e.wms.Report("M-large", []byte(`{"pad": "`+strings.Repeat("x", integration.MaxWMSMessage)+`"}`))
		if _, err := x.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if in := e.inbound(t, "M-large"); in["problem_code"] != "integration.wms_message_too_large" {
			t.Errorf("got %v, want integration.wms_message_too_large", in["problem_code"])
		}
	})
}

// TestWMSPollFailures: a warehouse system that cannot be listed, a
// confirmation that cannot be read, a shipping port that fails, or
// credentials that do not resolve record nothing and are reported; an
// acknowledgement that fails leaves the confirmation recorded, and the
// next poll acknowledges it; the stand-in in production and a connector
// without a secret_ref are not polled; an id too long to record is
// reported and the others are taken still.
func TestWMSPollFailures(t *testing.T) {
	e := setupWMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "2")
	e.queued(t, id)
	x := e.exchange(t, time.Now)
	e.wms.Report("GI-9", goodsIssue("GI-9", id, "1 1 BOX"))
	for name, c := range map[string]func(x *integration.WMSExchange){
		"the list": func(*integration.WMSExchange) { e.wms.Fail("GET /confirmations", 1, http.StatusServiceUnavailable) },
		"the confirmation": func(*integration.WMSExchange) {
			e.wms.Fail("GET /confirmations/GI-9", 1, http.StatusServiceUnavailable)
		},
		"the credentials": func(x *integration.WMSExchange) { x.Secrets = secretsNone{} },
		"the port":        func(x *integration.WMSExchange) { x.Deliveries = failingDeliveries{errors.New("shipping down")} },
		"a retry asked":   func(x *integration.WMSExchange) { x.Deliveries = failingDeliveries{problem.Unavailable("busy", 1)} },
	} {
		t.Run(name, func(t *testing.T) {
			x := e.exchange(t, time.Now)
			c(x)
			if _, err := x.PollOnce(context.Background()); err == nil {
				t.Fatal("got nil, want the failure reported")
			}
			page, _ := e.Must(200, "GET", "/v1/wms-messages?direction=inbound", "", "")
			if n := len(page["items"].([]any)); n != 0 {
				t.Errorf("recorded: got %d, want 0", n)
			}
		})
	}
	for name, c := range map[string]func(x *integration.WMSExchange){
		"the stand-in in production": func(x *integration.WMSExchange) { x.RefuseStandIn = true },
		"no secret_ref":              func(*integration.WMSExchange) { e.connectorSet(t, `secret_ref = NULL`) },
	} {
		t.Run(name, func(t *testing.T) {
			x := e.exchange(t, time.Now)
			c(x)
			if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
				t.Errorf("got %d, %v; want nothing polled", n, err)
			}
			e.connectorSet(t, `secret_ref = '`+e.ref+`'`)
		})
	}
	e.wms.Fail("POST /confirmations/GI-9/acknowledge", 1, http.StatusServiceUnavailable)
	if n, err := x.PollOnce(context.Background()); err == nil || n != 1 {
		t.Fatalf("an acknowledgement failing: got %d, %v; want 1 recorded and the failure reported", n, err)
	}
	if in := e.inbound(t, "GI-9"); in["status"] != "posted" || in["acknowledged_at"] != nil {
		t.Errorf("got %v, acknowledged %v; want posted, not acknowledged", in["status"], in["acknowledged_at"])
	}
	if n, err := x.PollOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("the next poll: got %d, %v; want nothing recorded", n, err)
	}
	if in := e.inbound(t, "GI-9"); in["acknowledged_at"] == nil || e.wms.Acknowledged("GI-9") != 1 {
		t.Errorf("the next poll's acknowledgement: got %v, %d; want acknowledged once", in["acknowledged_at"], e.wms.Acknowledged("GI-9"))
	}
	long := strings.Repeat("x", 201)
	e.wms.Report(long, goodsIssue(long, id, "1 1 BOX"))
	e.wms.Report("GI-10", goodsIssue("GI-10", uuid.NewString(), "1 1 BOX"))
	if n, err := x.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "201 characters") || n != 1 {
		t.Errorf("an id too long beside another: got %d, %v; want the other recorded and the long one reported", n, err)
	}
}

// failingDeliveries is shipping's port, failing with err.
type failingDeliveries struct{ err error }

func (f failingDeliveries) IssueReported(context.Context, uuid.UUID, []integration.WMSPicked) (integration.WMSIssued, error) {
	return integration.WMSIssued{}, f.err
}

// TestWMSDatabaseFailures: each statement of the queue's, the send's and
// the poll's transactions failing, and each commit, is reported, and the
// work is done once nothing fails; Run stops with its context.
func TestWMSDatabaseFailures(t *testing.T) {
	e := setupWMS(t)
	pool := pgtest.Pool(t)
	failing := func(failAt int, count *int, commit bool) database.BeginFunc {
		return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil || s.TenantID == uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt, Count: count, FailCommit: commit}, nil
		}
	}
	until := func(t *testing.T, name string, run func(begin database.BeginFunc) error, done func() bool) {
		t.Helper()
		if err := run(failing(0, new(int), true)); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("%s, the commit failing: got %v, want the injected failure", name, err)
		}
		for failAt := 1; failAt <= 200; failAt++ {
			count := 0
			err := run(failing(failAt, &count, false))
			if err != nil && !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("%s, statement %d: got %v, want the injected failure", name, failAt, err)
			}
			if err == nil && count < failAt && done() {
				return
			}
		}
		t.Fatalf("%s: still failing after 200 statements", name)
	}
	id, _ := e.delivery(t, e.p1, e.s1, "2")
	var log pgtest.SyncBuffer
	m := e.busEvent(t, id)
	until(t, "the queue", func(begin database.BeginFunc) error {
		q := e.queue(t, &log)
		q.Begin = begin
		return q.Handle(context.Background(), m)
	}, func() bool {
		page, _ := e.Must(200, "GET", "/v1/wms-messages?outbound_delivery_id="+id, "", "")
		return len(page["items"].([]any)) == 1
	})
	until(t, "the send", func(begin database.BeginFunc) error {
		e.due(t, id, 0)
		x := e.exchange(t, time.Now)
		x.Begin = begin
		_, err := x.SendOnce(context.Background())
		return err
	}, func() bool { s, _, _ := e.wmsRow(t, id); return s == "sent" })
	until(t, "the poll", func(begin database.BeginFunc) error {
		// Listed again each time: an acknowledgement made whose recording
		// failed is made again by the next poll, which finds it recorded.
		e.wms.Report("GI-db", goodsIssue("GI-db", id, "1 1 BOX"))
		x := e.exchange(t, time.Now)
		x.Begin = begin
		_, err := x.PollOnce(context.Background())
		return err
	}, func() bool { return e.inbound(t, "GI-db")["acknowledged_at"] != nil })
	for name, c := range map[string]struct {
		status int
		want   string
	}{"retried": {http.StatusServiceUnavailable, "pending"}, "refused": {http.StatusBadRequest, "failed"}} {
		other := e.queuedDelivery(t)
		until(t, "the send "+name, func(begin database.BeginFunc) error {
			e.due(t, other, 0)
			e.wms.Fail("PUT /messages", 1, c.status)
			x := e.exchange(t, time.Now)
			x.Begin = begin
			_, err := x.SendOnce(context.Background())
			return err
		}, func() bool { s, n, _ := e.wmsRow(t, other); return s == c.want && n >= 1 })
	}
	refusedID, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, refusedID)
	until(t, "the poll of a refusal", func(begin database.BeginFunc) error {
		e.wms.Report("GI-db-refused", goodsIssue("GI-db-refused", refusedID, "1 9 BOX"))
		x := e.exchange(t, time.Now)
		x.Begin = begin
		_, err := x.PollOnce(context.Background())
		return err
	}, func() bool { return e.inbound(t, "GI-db-refused")["acknowledged_at"] != nil })
	t.Run("the worker role's reads", func(t *testing.T) {
		x := e.exchange(t, time.Now)
		x.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
		if _, err := x.SendOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("send: got %v, want the injected failure", err)
		}
		if _, err := x.PollOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("poll: got %v, want the injected failure", err)
		}
	})
	t.Run("run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.exchange(t, time.Now).Run(ctx)
	})
}
