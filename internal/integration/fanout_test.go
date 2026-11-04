package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

const posted = "com.example.erp.ledger.journal_entry_posted.v1"

// busEvent is an event as the relay publishes it for the tenant.
func busEvent(tenant uuid.UUID, typ, name string) outbox.Message {
	id := uuid.Must(uuid.NewV7())
	return outbox.Message{ID: id, Subject: "erp.test.ledger." + tenant.String() + "." + name, Data: []byte(`{"entry_id": "e-1"}`),
		Header: map[string]string{"ce-specversion": "1.0", "ce-id": id.String(), "ce-source": "/erp/test/ledger", "ce-type": typ,
			"ce-subject": uuid.NewString(), "ce-time": "2026-10-19T08:00:00Z", "ce-datacontenttype": "application/json",
			"ce-tenantid": tenant.String(), "ce-actorid": uuid.NewString(), "Content-Type": "application/json"}}
}

// deliveries are the subscriptions with a delivery of the event, sorted.
func deliveries(t *testing.T, event uuid.UUID) []string {
	t.Helper()
	var subs []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(subscription_id::text ORDER BY subscription_id), '{}') FROM integration.webhook_delivery WHERE event_id = $1`,
		[]any{event}, &subs)
	return subs
}

func fanoutFor(t *testing.T, log *pgtest.SyncBuffer) *integration.Fanout {
	t.Helper()
	return &integration.Fanout{Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.NewTextHandler(log, nil))}
}

// TestWebhookFanout: an offered event becomes one pending delivery per
// active or paused subscription naming its type or context (a paused one
// holds it, a disabled one gets none), in its tenant only; read
// twice it adds nothing (the inbox).
func TestWebhookFanout(t *testing.T) {
	c := webhookClient(t)
	byContext, _ := createHook(c, `"ledger.*"`)
	byType, _ := createHook(c, `"`+posted+`"`)
	createHook(c, `"tenancy.*"`)
	paused, _ := createHook(c, `"ledger.*"`)
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'paused', version = version + 1 WHERE id = $1`, paused["id"])
	disabled, _ := createHook(c, `"ledger.*"`)
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = 'gone', version = version + 1 WHERE id = $1`, disabled["id"])
	other := webhookClient(t)
	createHook(other, `"ledger.*"`)

	var log pgtest.SyncBuffer
	f := fanoutFor(t, &log)
	m := busEvent(c.Tenant.ID, posted, "journal_entry_posted")
	for i := range 2 {
		if err := f.Handle(context.Background(), m); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	want := []string{byContext["id"].(string), byType["id"].(string), paused["id"].(string)}
	slices.Sort(want)
	if got := deliveries(t, m.ID); !slices.Equal(got, want) {
		t.Fatalf("deliveries to %v, want one to each of %v", got, want)
	}
	var status, typ string
	var subject uuid.UUID
	var body []byte
	pgtest.Scan(t, `SELECT status, type, subject, body FROM integration.webhook_delivery WHERE event_id = $1 LIMIT 1`, []any{m.ID}, &status, &typ, &subject, &body)
	if status != "pending" || typ != posted || subject.String() != m.Header["ce-subject"] {
		t.Errorf("delivery %s %s %s, want pending %s %s", status, typ, subject, posted, m.Header["ce-subject"])
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["id"] != m.ID.String() || doc["tenantid"] != c.Tenant.ID.String() || doc["specversion"] != "1.0" {
		t.Errorf("body %s lacks the event's attributes", body)
	}
	if data, ok := doc["data"].(map[string]any); !ok || data["entry_id"] != "e-1" {
		t.Errorf("body data %v, want the event's data", doc["data"])
	}
	if _, ok := doc["Content-Type"]; ok {
		t.Errorf("body %s carries a transport header", body)
	}
	if log.String() != "" {
		t.Errorf("log %q, want nothing", log.String())
	}
}

// TestWebhookFanoutSkipsAndRefuses: an event not offered to webhooks is
// skipped, as is one no subscription names; a malformed one, or one whose subject names another tenant, is
// refused and logged. Both are acknowledged and deliver nothing.
func TestWebhookFanoutSkipsAndRefuses(t *testing.T) {
	c := webhookClient(t)
	createHook(c, `"ledger.*"`)
	stranger := uuid.New()
	for name, change := range map[string]func(*outbox.Message){
		"not offered": func(m *outbox.Message) { m.Header["ce-type"] = "com.example.erp.platform.operation_completed.v1" },
		"no subscription names it": func(m *outbox.Message) {
			m.Header["ce-type"] = "com.example.erp.tenancy.legal_entity_created.v1"
			m.Subject = strings.Replace(m.Subject, ".ledger.", ".tenancy.", 1)
		},
		"subject of another form":  func(m *outbox.Message) { m.Subject = "erp.test." + c.Tenant.ID.String() },
		"another context's stream": func(m *outbox.Message) { m.Subject = strings.Replace(m.Subject, ".ledger.", ".payables.", 1) },
		"another tenant": func(m *outbox.Message) {
			m.Subject = strings.Replace(m.Subject, c.Tenant.ID.String(), stranger.String(), 1)
		},
		"tenant not a uuid": func(m *outbox.Message) {
			m.Subject = "erp.test.ledger.x.journal_entry_posted"
			m.Header["ce-tenantid"] = "x"
		},
		"id not a uuid":        func(m *outbox.Message) { m.Header["ce-id"] = "e-1" },
		"subject not a uuid":   func(m *outbox.Message) { m.Header["ce-subject"] = "s-1" },
		"time not a timestamp": func(m *outbox.Message) { m.Header["ce-time"] = "yesterday" },
		"data not json":        func(m *outbox.Message) { m.Data = []byte("{") },
	} {
		t.Run(name, func(t *testing.T) {
			var log pgtest.SyncBuffer
			m := busEvent(c.Tenant.ID, posted, "journal_entry_posted")
			change(&m)
			if err := fanoutFor(t, &log).Handle(context.Background(), m); err != nil {
				t.Fatalf("got %v, want nil (acknowledged)", err)
			}
			if got := deliveries(t, m.ID); len(got) != 0 {
				t.Errorf("deliveries to %v, want none", got)
			}
			want := name != "not offered" && name != "no subscription names it"
			if refused := strings.Contains(log.String(), "refused"); refused != want {
				t.Errorf("logged %q: refused %v, want %v", log.String(), refused, want)
			}
		})
	}
}

// TestWebhookFanoutDatabaseFailures: every statement, the begin and the
// commit failing is an error, so the event is read again.
func TestWebhookFanoutDatabaseFailures(t *testing.T) {
	c := webhookClient(t)
	createHook(c, `"ledger.*"`)
	pool := pgtest.Pool(t)
	for name, fail := range map[string]func(pgx.Tx) (pgx.Tx, error){
		"begin":      func(pgx.Tx) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"statement1": func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil },
		"statement2": func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailAt: 2}, nil },
		"commit":     func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := &integration.Fanout{Pool: pool, AppRole: "erp_app", Log: slog.New(slog.DiscardHandler),
				Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx, s)
					if err != nil {
						return nil, err
					}
					failing, ferr := fail(tx)
					if ferr != nil {
						_ = tx.Rollback(ctx) // the injected begin failure ends the real transaction too
					}
					return failing, ferr
				}}
			m := busEvent(c.Tenant.ID, posted, "journal_entry_posted")
			if err := f.Handle(context.Background(), m); !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("got %v, want the injected failure", err)
			}
			if got := deliveries(t, m.ID); len(got) != 0 {
				t.Errorf("deliveries to %v, want none", got)
			}
		})
	}
}

// TestWebhookContexts: the fan-out reads the streams of contexts offering
// webhook events, and not those offering none.
func TestWebhookContexts(t *testing.T) {
	got := integration.WebhookContexts()
	for _, want := range []string{"ledger", "tenancy"} {
		if !slices.Contains(got, want) {
			t.Errorf("contexts %v lack %s", got, want)
		}
	}
	if slices.Contains(got, "platform") {
		t.Errorf("contexts %v include platform, which offers no webhook events", got)
	}
}

// TestCashSnapshotTakenWebhook: CashSnapshotTaken is offered to webhooks
// (ADR-0051 §14, T53h): a subscription to cash.* gets a delivery of it,
// which the dispatcher sends, signed, with the snapshot it names, for a
// treasury system that pulls.
func TestCashSnapshotTakenWebhook(t *testing.T) {
	const taken = "com.example.erp.cash.cash_snapshot_taken.v1"
	if !slices.Contains(integration.WebhookContexts(), "cash") {
		t.Fatalf("webhook contexts %v, want cash among them", integration.WebhookContexts())
	}
	x := newDispatch(t)
	x.hook(x.c, `"cash.*"`)
	m := busEvent(x.c.Tenant.ID, taken, "cash_snapshot_taken")
	m.Subject = strings.Replace(m.Subject, ".ledger.", ".cash.", 1)
	m.Header["ce-source"] = "/erp/test/cash"
	m.Data = []byte(`{"cash_snapshot_id": "` + m.Header["ce-subject"] + `", "company_code": "3000", "kind": "daily", "snapshot_date": "2026-04-17", "line_count": 16}`)
	var log pgtest.SyncBuffer
	if err := fanoutFor(t, &log).Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	x.round(1)
	got := x.rec.requests()
	if len(got) != 1 {
		t.Fatalf("requests: got %d, want the one delivery", len(got))
	}
	var doc struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Type != taken || doc.Data["cash_snapshot_id"] != m.Header["ce-subject"] {
		t.Errorf("body: got %s %v, want %s naming snapshot %s", doc.Type, doc.Data, taken, m.Header["ce-subject"])
	}
	if got[0].header.Get("webhook-signature") == "" {
		t.Error("signature: got none, want the delivery signed")
	}
}
