package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// record writes an attempt's answer to the delivery, guarded by the version
// it was claimed at, and its consequence for the subscription. A delivery
// recorded already (a lost commit, a lease that lapsed and was taken over)
// is left as it is.
func (d *Dispatcher) record(ctx context.Context, c claimed, a answer) error {
	tx, err := database.BeginOr(ctx, d.Pool, d.Begin, database.Scope{Role: d.AppRole, TenantID: c.tenant})
	if err != nil {
		return fmt.Errorf("integration: record webhook delivery %s: %w", c.id, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// The subscription is locked before the delivery, the order the API's
	// redelivery and replay and a disabling take them in, so neither waits
	// for the other in a cycle. A ping changes nothing of the subscription.
	var sub subscriptionState
	if !c.ping {
		if sub, err = lockSubscription(ctx, tx, c.sub); err != nil {
			return fmt.Errorf("integration: record webhook delivery %s: %w", c.id, err)
		}
	}
	now := clock.Now(d.Now)
	status, next := nextState(c, a, now)
	var lastStatus *int
	if a.status != 0 {
		lastStatus = &a.status
	}
	var lastError *string
	if a.err != "" {
		lastError = &a.err
	}
	tag, err := tx.Exec(ctx, `UPDATE integration.webhook_delivery SET status = $3, attempts = attempts + 1, next_attempt_at = $4,
		last_status = $5, last_error = $6, delivered_at = CASE WHEN $3 = 'delivered' THEN $7::timestamptz END, version = version + 1
		WHERE id = $1 AND version = $2 AND status = 'pending'`, c.id, c.version, status, next, lastStatus, lastError, now)
	if err != nil {
		return fmt.Errorf("integration: record webhook delivery %s: %w", c.id, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	// A ping only tests the endpoint: its outcome is the delivery's alone.
	if !c.ping {
		if err := d.afterAttempt(ctx, tx, c, sub, a, now); err != nil {
			return fmt.Errorf("integration: record webhook delivery %s: %w", c.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("integration: commit webhook delivery %s: %w", c.id, err)
	}
	return nil
}

// nextState is the delivery's status after the attempt and, while pending,
// when it is attempted next: any 2xx delivers, a 410 Gone dead letters at
// once, anything else retries on the schedule until RetryWindow; a ping
// is attempted once.
func nextState(c claimed, a answer, now time.Time) (string, *time.Time) {
	switch {
	case a.delivered():
		return "delivered", nil
	case a.status == http.StatusGone, c.ping:
		return "dead", nil
	}
	wait, ok := retryAfter(c.id, int(c.attempts)+1)
	if !ok {
		return "dead", nil
	}
	next := now.Add(wait)
	return "pending", &next
}

// retryAfter is the wait after failed attempt n (from 1), with up to 10 %
// added from the delivery id, so endpoints that failed together are not
// retried together, and the same delivery always waits the same; false
// when the schedule, counted from the first attempt, passes RetryWindow.
func retryAfter(id uuid.UUID, n int) (time.Duration, bool) {
	var wait, total time.Duration
	for i := range n {
		wait = laterBackoff
		if i < len(backoff) {
			wait = backoff[i]
		}
		total += wait
	}
	if total > RetryWindow {
		return 0, false
	}
	return wait + wait*time.Duration(binary.BigEndian.Uint64(id[8:])%100)/1000, true
}

// subscriptionState is what recording an attempt reads of its
// subscription.
type subscriptionState struct {
	status, url  string
	failingSince *time.Time
}

func lockSubscription(ctx context.Context, tx pgx.Tx, id uuid.UUID) (subscriptionState, error) {
	var s subscriptionState
	if err := tx.QueryRow(ctx, `SELECT status, url, failing_since FROM integration.webhook_subscription WHERE id = $1 FOR UPDATE`,
		id).Scan(&s.status, &s.url, &s.failingSince); err != nil {
		return s, fmt.Errorf("read subscription %s: %w", id, err)
	}
	return s, nil
}

// afterAttempt keeps the subscription's failing_since: a delivery clears
// it, the first failure sets it, and a failure FailingLimit after it, or a
// 410 Gone, disables a subscription that is active or paused.
// failing_since is the dispatcher's bookkeeping: changing it leaves the
// version (the ETag) and updated_by alone, which only the tenant's changes
// and disabling move.
func (d *Dispatcher) afterAttempt(ctx context.Context, tx pgx.Tx, c claimed, s subscriptionState, a answer, now time.Time) error {
	sub, url, failingSince := c.sub, s.url, s.failingSince
	live := s.status == "active" || s.status == "paused"
	switch {
	case a.delivered():
		if failingSince == nil {
			return nil
		}
		return setSubscription(ctx, tx, sub, `UPDATE integration.webhook_subscription SET failing_since = NULL WHERE id = $1`, sub)
	case !live:
		return nil
	case a.status == http.StatusGone:
		return d.disable(ctx, tx, c, "gone", url, now)
	case failingSince == nil:
		return setSubscription(ctx, tx, sub, `UPDATE integration.webhook_subscription SET failing_since = $2 WHERE id = $1`, sub, now)
	case now.Sub(*failingSince) >= FailingLimit:
		return d.disable(ctx, tx, c, "failing", url, now)
	}
	return nil
}

func setSubscription(ctx context.Context, tx pgx.Tx, sub uuid.UUID, sql string, args ...any) error {
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("update subscription %s: %w", sub, err)
	}
	return nil
}

// disable stops deliveries to a subscription, audited as the dispatcher's:
// a change the tenant sees, so it moves the version. Its pending
// deliveries are dropped, pings aside; resuming it does not bring them
// back.
func (d *Dispatcher) disable(ctx context.Context, tx pgx.Tx, c claimed, reason, url string, now time.Time) error {
	sub, tenant := c.sub, c.tenant
	if err := setSubscription(ctx, tx, sub, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = $3,
		failing_since = coalesce(failing_since, $4), version = version + 1, updated_by = $2 WHERE id = $1`, sub, DispatchActor, reason, now); err != nil {
		return err
	}
	if err := setSubscription(ctx, tx, sub, `UPDATE integration.webhook_delivery SET status = 'dropped', next_attempt_at = NULL, version = version + 1
		WHERE subscription_id = $1 AND status = 'pending' AND type <> $2`, sub, PingType); err != nil {
		return err
	}
	actor := reqctx.Principal{ID: DispatchActor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "integration.webhook_subscription.disable",
		ObjectType: "integration.webhook_subscription", ObjectID: sub, Outcome: "succeeded",
		Detail: map[string]any{"reason": reason, "host": host(url)}}); err != nil {
		return fmt.Errorf("audit disabling subscription %s: %w", sub, err)
	}
	d.Log.WarnContext(ctx, "webhook subscription disabled", "tenant", tenant, "subscription", sub, "reason", reason)
	return nil
}

// Prune deletes up to PruneBatch of the deliveries older than
// DeliveryRetention, held ones too; it returns how many and whether it
// found a full batch, so more may be left.
func (d *Dispatcher) Prune(ctx context.Context) (n int64, more bool, err error) {
	tx, err := database.BeginOr(ctx, d.Pool, d.Begin, database.Scope{Role: d.WorkerRole})
	if err != nil {
		return 0, false, fmt.Errorf("integration: prune webhook deliveries: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := pruneBatch
	if d.PruneBatch > 0 {
		batch = d.PruneBatch
	}
	tag, err := tx.Exec(ctx, `DELETE FROM integration.webhook_delivery WHERE (tenant_id, id) IN (
		SELECT tenant_id, id FROM integration.webhook_delivery WHERE created_at < $1 LIMIT $2)`, clock.Now(d.Now).Add(-DeliveryRetention), batch)
	if err != nil {
		return 0, false, fmt.Errorf("integration: prune webhook deliveries: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("integration: commit the webhook delivery pruning: %w", err)
	}
	return tag.RowsAffected(), tag.RowsAffected() == int64(batch), nil
}
