package db

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// row is a test row: column -> value, inserted with parameters.
type row map[string]any

// with returns a copy of r with the columns of o set (a nil value inserts NULL).
func (r row) with(o row) row {
	out := row{}
	for k, v := range r {
		out[k] = v
	}
	for k, v := range o {
		out[k] = v
	}
	return out
}

// insert inserts r into table; the columns are the test's own constants.
func (r row) insert(table string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		cols := make([]string, 0, len(r))
		for k := range r {
			cols = append(cols, k)
		}
		sort.Strings(cols)
		marks, args := make([]string, len(cols)), make([]any, len(cols))
		for i, c := range cols {
			marks[i], args[i] = "$"+strconv.Itoa(i+1), r[c]
		}
		return exec(tx, "INSERT INTO "+table+" ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", args...)
	}
}

func webhookSubscription(tenant, actor uuid.UUID) row {
	return row{"tenant_id": tenant, "id": id(), "url": "https://hooks.example.com/erp", "event_types": []string{"ledger.*", "com.example.erp.payables.supplier_invoice_posted.v1"},
		"status": "active", "secret_cipher": []byte{1}, "created_by": actor, "updated_by": actor}
}

func webhookDelivery(tenant, subscription uuid.UUID) row {
	return row{"tenant_id": tenant, "id": id(), "subscription_id": subscription, "event_id": id(), "type": "com.example.erp.ledger.journal_entry_posted.v1",
		"subject": id(), "occurred_at": "2026-10-19T07:59:59Z", "body": []byte(`{}`), "status": "pending", "next_attempt_at": "2026-10-19T08:00:00Z"}
}

// TestWebhookSubscriptions: a subscription sends to an https URL the
// event types or contexts it lists, is disabled only with a reason and
// keeps a previous secret only with its expiry (T50c, ADR-0041).
func TestWebhookSubscriptions(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	sub := webhookSubscription(tenant, actor)
	mustApp(t, tenant, sub.insert("integration.webhook_subscription"))
	mustApp(t, tenant, sub.with(row{"id": id(), "status": "disabled", "disabled_reason": "gone",
		"previous_secret_cipher": []byte{2}, "previous_expires_at": "2026-10-20T08:00:00Z"}).insert("integration.webhook_subscription"))
	mustApp(t, tenant, sub.with(row{"id": id(), "url": "https://hooks.example.com?token=x"}).insert("integration.webhook_subscription"))
	const check = "webhook_subscription_url_check"
	for _, c := range []struct {
		name  string
		set   row
		fails string
	}{
		{"an http URL", row{"url": "http://hooks.example.com"}, check},
		{"a URL without a host", row{"url": "https://"}, check},
		{"a URL with a space", row{"url": "https://hooks.example.com/a b"}, check},
		{"a URL with a fragment", row{"url": "https://hooks.example.com/erp#x"}, check},
		{"a URL with credentials", row{"url": "https://u:p@hooks.example.com/erp"}, check},
		{"a URL over 2048 characters", row{"url": "https://" + strings.Repeat("a", 2041)}, check},
		{"a description over 200 characters", row{"description": strings.Repeat("a", 201)}, "webhook_subscription_description_check"},
		{"no event types", row{"event_types": []string{}}, "webhook_subscription_event_types_check"},
		{"an event type of no form", row{"event_types": []string{"ledger.journal_entry_posted"}}, "webhook_subscription_event_types_check"},
		{"an unversioned event type", row{"event_types": []string{"com.example.erp.ledger.journal_entry_posted"}}, "webhook_subscription_event_types_check"},
		{"a NULL event type", row{"event_types": []*string{nil}}, "webhook_subscription_event_types_check"},
		{"another status", row{"status": "inactive"}, "webhook_subscription_status_check"},
		{"disabled without a reason", row{"status": "disabled"}, "webhook_subscription_disabled_reason"},
		{"a reason while active", row{"disabled_reason": "gone"}, "webhook_subscription_disabled_reason"},
		{"another reason", row{"status": "disabled", "disabled_reason": "manual"}, "webhook_subscription_disabled_reason_check"},
		{"a previous secret without expiry", row{"previous_secret_cipher": []byte{2}}, "webhook_subscription_previous_secret"},
		{"an expiry without a previous secret", row{"previous_expires_at": "2026-10-20T08:00:00Z"}, "webhook_subscription_previous_secret"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failsOn(t, asApp(t, tenant, sub.with(c.set).with(row{"id": id()}).insert("integration.webhook_subscription")), c.fails)
		})
	}
}

// TestWebhookDeliveries: one delivery per event and subscription, pending
// exactly while an attempt is due and delivered exactly when it has a
// delivery time (T50c, ADR-0041).
func TestWebhookDeliveries(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	sub := webhookSubscription(tenant, actor)
	mustApp(t, tenant, sub.insert("integration.webhook_subscription"))
	d := webhookDelivery(tenant, sub["id"].(uuid.UUID))
	mustApp(t, tenant, d.insert("integration.webhook_delivery"))
	mustApp(t, tenant, d.with(row{"id": id(), "event_id": id(), "status": "delivered", "next_attempt_at": nil,
		"delivered_at": "2026-10-19T08:00:01Z", "attempts": 1, "last_status": 204}).insert("integration.webhook_delivery"))
	for _, c := range []struct {
		name  string
		set   row
		fails string
	}{
		{"the same event again", row{"event_id": d["event_id"]}, "webhook_delivery_tenant_id_subscription_id_event_id_key"},
		{"an unknown subscription", row{"subscription_id": id()}, "webhook_delivery_tenant_id_subscription_id_fkey"},
		{"an unversioned type", row{"type": "com.example.erp.ledger.journal_entry_posted"}, "webhook_delivery_type_check"},
		{"another status", row{"status": "failed", "next_attempt_at": nil, "attempts": 1}, "webhook_delivery_status_check"},
		{"negative attempts", row{"attempts": -1}, "webhook_delivery_attempts_check"},
		{"pending with no attempt due", row{"next_attempt_at": nil}, "webhook_delivery_next_attempt"},
		{"dead with an attempt due", row{"status": "dead", "attempts": 1}, "webhook_delivery_next_attempt"},
		{"delivered without a time", row{"status": "delivered", "next_attempt_at": nil, "attempts": 1}, "webhook_delivery_delivered"},
		{"dead without an attempt", row{"status": "dead", "next_attempt_at": nil}, "webhook_delivery_attempted"},
		{"a delivery time while pending", row{"delivered_at": "2026-10-19T08:00:01Z"}, "webhook_delivery_delivered"},
		{"an HTTP status below 100", row{"last_status": 99}, "webhook_delivery_last_status_check"},
		{"an HTTP status above 599", row{"last_status": 600}, "webhook_delivery_last_status_check"},
		{"an error over 1000 characters", row{"last_error": strings.Repeat("e", 1001)}, "webhook_delivery_last_error_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			set := row{"id": id()}
			if _, same := c.set["event_id"]; !same {
				set["event_id"] = id()
			}
			failsOn(t, asApp(t, tenant, d.with(set).with(c.set).insert("integration.webhook_delivery")), c.fails)
		})
	}
}

// TestWebhookIsolation: another tenant neither sees nor writes a tenant's
// subscriptions and deliveries, and the application deletes neither.
func TestWebhookIsolation(t *testing.T) {
	need(t)
	tenant, other, actor := id(), id(), id()
	sub := webhookSubscription(tenant, actor)
	mustApp(t, tenant, sub.insert("integration.webhook_subscription"))
	mustApp(t, tenant, webhookDelivery(tenant, sub["id"].(uuid.UUID)).insert("integration.webhook_delivery"))
	for _, table := range []string{"integration.webhook_subscription", "integration.webhook_delivery"} {
		t.Run(table, func(t *testing.T) {
			var seen int
			mustApp(t, other, func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&seen)
			})
			if seen != 0 {
				t.Errorf("rows another tenant sees: got %d, want 0", seen)
			}
			failsWith(t, asApp(t, tenant, func(tx pgx.Tx) error { return exec(tx, "DELETE FROM "+table) }), "permission denied")
		})
	}
	t.Run("another tenant's subscription", func(t *testing.T) {
		failsWith(t, asApp(t, other, webhookSubscription(tenant, actor).insert("integration.webhook_subscription")), "row-level security")
	})
	t.Run("the permissions exist", func(t *testing.T) {
		var n int
		if err := testDB.QueryRow(context.Background(), `SELECT count(*) FROM ref.permission
			WHERE code IN ('integration.webhook.read', 'integration.webhook.manage') AND NOT is_approval`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("webhook permissions: got %d, want 2", n)
		}
	})
}

// TestWebhookWorkerGrants: the delivery worker's role (000092) finds and
// deletes deliveries across tenants but reads no body, URL or secret.
func TestWebhookWorkerGrants(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	sub := webhookSubscription(tenant, actor)
	mustApp(t, tenant, sub.insert("integration.webhook_subscription"))
	d := webhookDelivery(tenant, sub["id"].(uuid.UUID))
	mustApp(t, tenant, d.insert("integration.webhook_delivery"))
	asWorker := func(sql string, args ...any) error {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE erp_worker"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, args...)
		return err
	}
	for name, sql := range map[string]string{
		"due deliveries": `SELECT d.tenant_id, d.id, d.subscription_id, d.event_id, d.subject, d.occurred_at, d.status, d.next_attempt_at, d.created_at, s.status
			FROM integration.webhook_delivery d JOIN integration.webhook_subscription s ON s.tenant_id = d.tenant_id AND s.id = d.subscription_id`,
		"the retention": `DELETE FROM integration.webhook_delivery WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			args := []any{}
			if strings.Contains(sql, "$1") {
				args = append(args, d["id"])
			}
			if err := asWorker(sql, args...); err != nil {
				t.Errorf("got %v, want allowed", err)
			}
		})
	}
	for name, sql := range map[string]string{
		"a body":   `SELECT body FROM integration.webhook_delivery`,
		"a URL":    `SELECT url FROM integration.webhook_subscription`,
		"a secret": `SELECT secret_cipher FROM integration.webhook_subscription`,
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, asWorker(sql), "permission denied") })
	}
}

// TestWebhookBookkeeping (000092): failing_since alone moves without the
// version, any other change of a subscription still needs it; a delivery
// dropped unsent needs no attempt.
func TestWebhookBookkeeping(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	sub := webhookSubscription(tenant, actor)
	mustApp(t, tenant, sub.insert("integration.webhook_subscription"))
	t.Run("failing_since without the version", func(t *testing.T) {
		mustApp(t, tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE integration.webhook_subscription SET failing_since = now() WHERE id = $1`, sub["id"])
		})
	})
	t.Run("another change without the version", func(t *testing.T) {
		failsWith(t, asApp(t, tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE integration.webhook_subscription SET failing_since = NULL, description = 'x' WHERE id = $1`, sub["id"])
		}), "optimistic lock")
	})
	t.Run("dropped unsent", func(t *testing.T) {
		mustApp(t, tenant, webhookDelivery(tenant, sub["id"].(uuid.UUID)).with(row{"status": "dropped", "next_attempt_at": nil}).insert("integration.webhook_delivery"))
	})
}
