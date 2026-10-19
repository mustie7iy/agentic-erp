package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Delivery operations (ADR-0041, T50g): a tenant reads a subscription's
// deliveries, queues one again (redelivery) or all since an instant
// (replay), and pings its endpoint. None of them calls the endpoint: they
// leave pending deliveries that the dispatcher sends, each under its
// event's webhook-id, so a deduplicating endpoint takes an event once.

// PingType is the CloudEvents type of a ping: a synthetic event outside
// the contract, sent whatever the subscription's status but deleted,
// attempted once and not counted toward the subscription's failures.
const PingType = "webhook.ping"

// Delivery is a row of integration.webhook_delivery without its body.
type Delivery struct {
	ID, SubscriptionID, EventID uuid.UUID
	Type                        string
	Subject                     uuid.UUID
	OccurredAt                  time.Time
	Status                      string
	Attempts                    int32
	NextAttemptAt               *time.Time
	LastStatus                  *int32
	LastError                   *string
	DeliveredAt                 *time.Time
	Version                     int32
	CreatedAt, UpdatedAt        time.Time
}

const deliveryColumns = `id, subscription_id, event_id, type, subject, occurred_at, status, attempts, next_attempt_at,
	last_status, last_error, delivered_at, version, created_at, updated_at`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.SubscriptionID, &d.EventID, &d.Type, &d.Subject, &d.OccurredAt, &d.Status, &d.Attempts, &d.NextAttemptAt,
		&d.LastStatus, &d.LastError, &d.DeliveredAt, &d.Version, &d.CreatedAt, &d.UpdatedAt)
	return d, errs.Wrapf(err, "scan webhook delivery")
}

func getDelivery(ctx context.Context, tx pgx.Tx, sub, id uuid.UUID, lock bool) (Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM integration.webhook_delivery WHERE id = $1 AND subscription_id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	d, err := scanDelivery(tx.QueryRow(ctx, q, id, sub))
	if noRows(err) {
		return d, problem.NotFoundOf("webhook delivery")
	}
	if err != nil {
		return d, fmt.Errorf("integration: read webhook delivery %s: %w", id, err)
	}
	return d, nil
}

// requeue resets deliveries to a first attempt, due now.
const requeue = `UPDATE integration.webhook_delivery SET status = 'pending', attempts = 0, next_attempt_at = now(),
	last_status = NULL, last_error = NULL, delivered_at = NULL, version = version + 1`

// anyVersion accepts the subscription at any version: the request's
// If-Match is checked against the delivery.
func anyVersion(int32) error { return nil }

// queueable reads a subscription to queue deliveries to: locked, so a
// disabling (which drops the pending ones) waits or is seen, and neither
// deleted nor disabled.
func queueable(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	s, err := changeable(ctx, tx, id, ifMatch)
	if err != nil {
		return s, err
	}
	if s.Status == "disabled" {
		return s, problem.Conflict("integration.webhook_disabled", "The webhook subscription is disabled; resume it first.")
	}
	return s, nil
}

// Redeliver queues one delivery again, whatever its status, as a first
// attempt due now; it goes out in its resource's event order.
func (e *Endpoints) Redeliver(ctx context.Context, sub, id uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if _, err := queueable(ctx, tx, sub, anyVersion); err != nil {
		return Delivery{}, err
	}
	d, err := getDelivery(ctx, tx, sub, id, true)
	if err != nil {
		return Delivery{}, err
	}
	if err := ifMatch(d.Version); err != nil {
		return Delivery{}, err
	}
	updated, err := scanDelivery(tx.QueryRow(ctx, requeue+` WHERE id = $1 RETURNING `+deliveryColumns, id))
	if err != nil {
		return Delivery{}, fmt.Errorf("integration: redeliver webhook delivery %s: %w", id, err)
	}
	return updated, record(ctx, "integration.webhook_delivery.redeliver", id,
		map[string]any{"subscription_id": sub, "event_id": d.EventID, "previous_status": d.Status})
}

// Replay queues again every kept delivery of the subscription whose event
// occurred at or after since, whatever its status (pings aside), as first
// attempts due now; the dispatcher sends each resource's in event order.
// since is within DeliveryRetention of now.
func (e *Endpoints) Replay(ctx context.Context, sub uuid.UUID, ifMatch func(int32) error, since time.Time) (int64, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	if now := clock.Now(e.Now); since.Before(now.Add(-DeliveryRetention)) || since.After(now) {
		return 0, invalid("integration.invalid_replay_since", "since", "An instant in the past, at most 30 days back: deliveries are kept 30 days.")
	}
	if _, err := queueable(ctx, tx, sub, ifMatch); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, requeue+` WHERE subscription_id = $1 AND occurred_at >= $2 AND type <> $3`, sub, since, PingType)
	if err != nil {
		return 0, fmt.Errorf("integration: replay webhook subscription %s: %w", sub, err)
	}
	return tag.RowsAffected(), record(ctx, "integration.webhook_subscription.replay", sub,
		map[string]any{"since": since, "queued": tag.RowsAffected()})
}

// Ping queues a ping to the subscription's endpoint and returns its
// delivery, whose outcome the tenant reads once the dispatcher has sent
// it: the request never waits for the endpoint.
func (e *Endpoints) Ping(ctx context.Context, sub uuid.UUID, ifMatch func(int32) error) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if _, err := changeable(ctx, tx, sub, ifMatch); err != nil {
		return Delivery{}, err
	}
	event, now := uuid.Must(uuid.NewV7()), clock.Now(e.Now).UTC()
	body := pingBody(p.TenantID, sub, event, now)
	d, err := scanDelivery(tx.QueryRow(ctx, `INSERT INTO integration.webhook_delivery
		(tenant_id, id, subscription_id, event_id, type, subject, occurred_at, body, status, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $3, $6, $7, 'pending', now()) RETURNING `+deliveryColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), sub, event, PingType, now, body))
	if err != nil {
		return Delivery{}, fmt.Errorf("integration: ping webhook subscription %s: %w", sub, err)
	}
	return d, record(ctx, "integration.webhook_subscription.ping", sub, map[string]any{"delivery_id": d.ID})
}

// pingBody is the ping in CloudEvents structured JSON, as a bus event is
// rendered; its subject and data name the subscription.
func pingBody(tenant, sub, event uuid.UUID, at time.Time) []byte {
	body, _ := json.Marshal(map[string]any{ // a map of strings always marshals
		"specversion": "1.0", "id": event.String(), "source": "/erp/integration/webhooks", "type": PingType,
		"subject": sub.String(), "time": at.Format(time.RFC3339Nano), "datacontenttype": "application/json",
		"tenantid": tenant.String(), "data": map[string]string{"subscription_id": sub.String()},
	})
	return body
}
