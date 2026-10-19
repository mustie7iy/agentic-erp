package integration

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

func subscriptionToAPI(s Subscription) gen.WebhookSubscription {
	out := gen.WebhookSubscription{ID: s.ID, Version: s.Version, CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy, UpdatedAt: s.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(s.UpdatedBy), URL: s.URL, Description: s.Description, EventTypes: s.EventTypes, Status: s.Status}
	if s.DisabledReason != nil {
		out.DisabledReason.SetTo(*s.DisabledReason)
	}
	if s.FailingSince != nil {
		out.FailingSince.SetTo(*s.FailingSince)
	}
	if s.PreviousExpiresAt != nil {
		out.PreviousSecretExpiresAt.SetTo(*s.PreviousExpiresAt)
	}
	if s.Secret != "" {
		out.Secret.SetTo(s.Secret)
	}
	return out
}

// withSecret answers with the secret once: the answer stored for the
// idempotency key leaves it out (ADR-0041).
func withSecret(ctx context.Context, s Subscription) gen.WebhookSubscription {
	uow.OmitOnReplay(ctx, "secret")
	return subscriptionToAPI(s)
}

func subscriptionOK(s Subscription, body gen.WebhookSubscription) *gen.UpdateWebhookSubscriptionOKHeaders {
	return &gen.UpdateWebhookSubscriptionOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: body}
}

// CreateWebhookSubscription implements POST /v1/webhook-subscriptions.
func (e *Endpoints) CreateWebhookSubscription(ctx context.Context, req *gen.WebhookSubscriptionCreate, _ gen.CreateWebhookSubscriptionParams) (gen.CreateWebhookSubscriptionRes, error) {
	s, err := e.CreateSubscription(ctx, NewSubscription{URL: req.URL, Description: req.Description.Value, EventTypes: req.EventTypes})
	if err != nil {
		return nil, err
	}
	return &gen.WebhookSubscriptionHeaders{ETag: gen.NewOptString(etag.Format(s.Version)),
		Location: gen.NewOptString("/v1/webhook-subscriptions/" + s.ID.String()), Response: withSecret(ctx, s)}, nil
}

// GetWebhookSubscription implements GET /v1/webhook-subscriptions/{id}.
func (e *Endpoints) GetWebhookSubscription(ctx context.Context, params gen.GetWebhookSubscriptionParams) (gen.GetWebhookSubscriptionRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	s, err := getSubscription(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetWebhookSubscriptionOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: subscriptionToAPI(s)}, nil
}

var subscriptionSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
}}

// ListWebhookSubscriptions implements GET /v1/webhook-subscriptions:
// without a status, every subscription but the deleted ones.
func (e *Endpoints) ListWebhookSubscriptions(ctx context.Context, params gen.ListWebhookSubscriptionsParams) (gen.ListWebhookSubscriptionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(subscriptionSort, "listWebhookSubscriptions|"+params.Status.Value,
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+subscriptionColumns+` FROM integration.webhook_subscription
		WHERE (status = $1 OR ($1 = '' AND status <> 'deleted'))`, []any{params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subscription, error) { return scanSubscription(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: list webhook subscriptions: %w", err)
	}
	items, next := apitypes.PageOf(ks, ss, func(s Subscription) (string, uuid.UUID) {
		return s.CreatedAt.UTC().Format(time.RFC3339Nano), s.ID
	}, subscriptionToAPI)
	return &gen.WebhookSubscriptionPageHeaders{Response: gen.WebhookSubscriptionPage{Items: items, NextPageToken: next}}, nil
}

// UpdateWebhookSubscription implements PATCH /v1/webhook-subscriptions/{id}.
func (e *Endpoints) UpdateWebhookSubscription(ctx context.Context, req *gen.WebhookSubscriptionUpdate, params gen.UpdateWebhookSubscriptionParams) (gen.UpdateWebhookSubscriptionRes, error) {
	ch := SubscriptionChange{EventTypes: req.EventTypes}
	if u, ok := req.URL.Get(); ok {
		ch.URL = &u
	}
	if d, ok := req.Description.Get(); ok {
		ch.Description = &d
	}
	s, err := e.UpdateSubscription(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return subscriptionOK(s, subscriptionToAPI(s)), nil
}

// DeleteWebhookSubscription implements POST /v1/webhook-subscriptions/{id}:delete.
func (e *Endpoints) DeleteWebhookSubscription(ctx context.Context, params gen.DeleteWebhookSubscriptionParams) (gen.DeleteWebhookSubscriptionRes, error) {
	s, err := e.DeleteSubscription(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return subscriptionOK(s, subscriptionToAPI(s)), nil
}

// RotateWebhookSubscriptionSecret implements POST
// /v1/webhook-subscriptions/{id}:rotateSecret.
func (e *Endpoints) RotateWebhookSubscriptionSecret(ctx context.Context, params gen.RotateWebhookSubscriptionSecretParams) (gen.RotateWebhookSubscriptionSecretRes, error) {
	s, err := e.RotateSecret(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return subscriptionOK(s, withSecret(ctx, s)), nil
}

// PauseWebhookSubscription implements POST /v1/webhook-subscriptions/{id}:pause.
func (e *Endpoints) PauseWebhookSubscription(ctx context.Context, params gen.PauseWebhookSubscriptionParams) (gen.PauseWebhookSubscriptionRes, error) {
	s, err := e.PauseSubscription(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return subscriptionOK(s, subscriptionToAPI(s)), nil
}

// ResumeWebhookSubscription implements POST /v1/webhook-subscriptions/{id}:resume.
func (e *Endpoints) ResumeWebhookSubscription(ctx context.Context, params gen.ResumeWebhookSubscriptionParams) (gen.ResumeWebhookSubscriptionRes, error) {
	s, err := e.ResumeSubscription(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return subscriptionOK(s, subscriptionToAPI(s)), nil
}

func deliveryToAPI(d Delivery) gen.WebhookDelivery {
	out := gen.WebhookDelivery{ID: d.ID, Version: d.Version, SubscriptionID: d.SubscriptionID, EventID: d.EventID, Type: d.Type,
		Subject: d.Subject, OccurredAt: d.OccurredAt, Status: d.Status, Attempts: d.Attempts, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if d.NextAttemptAt != nil {
		out.NextAttemptAt.SetTo(*d.NextAttemptAt)
	}
	if d.LastStatus != nil {
		out.LastStatus.SetTo(*d.LastStatus)
	}
	if d.LastError != nil {
		out.LastError.SetTo(*d.LastError)
	}
	if d.DeliveredAt != nil {
		out.DeliveredAt.SetTo(*d.DeliveredAt)
	}
	return out
}

var deliverySort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
}}

// ListWebhookDeliveries implements GET
// /v1/webhook-subscriptions/{id}/deliveries.
func (e *Endpoints) ListWebhookDeliveries(ctx context.Context, params gen.ListWebhookDeliveriesParams) (gen.ListWebhookDeliveriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := getSubscription(ctx, tx, params.ID, false); err != nil {
		return nil, err
	}
	var since *time.Time
	if s, ok := params.Since.Get(); ok {
		since = &s
	}
	ks, err := e.Pages.Keyset(deliverySort, fmt.Sprintf("listWebhookDeliveries|%s|%s|%v", params.ID, params.Status.Value, since),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+deliveryColumns+` FROM integration.webhook_delivery
		WHERE subscription_id = $1 AND ($2 = '' OR status = $2) AND ($3::timestamptz IS NULL OR created_at >= $3)`,
		[]any{params.ID, params.Status.Value, since})
	rows, _ := tx.Query(ctx, q, args...)
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) { return scanDelivery(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: list webhook deliveries of %s: %w", params.ID, err)
	}
	items, next := apitypes.PageOf(ks, ds, func(d Delivery) (string, uuid.UUID) {
		return d.CreatedAt.UTC().Format(time.RFC3339Nano), d.ID
	}, deliveryToAPI)
	return &gen.WebhookDeliveryPageHeaders{Response: gen.WebhookDeliveryPage{Items: items, NextPageToken: next}}, nil
}

// GetWebhookDelivery implements GET
// /v1/webhook-subscriptions/{id}/deliveries/{delivery_id}.
func (e *Endpoints) GetWebhookDelivery(ctx context.Context, params gen.GetWebhookDeliveryParams) (gen.GetWebhookDeliveryRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	d, err := getDelivery(ctx, tx, params.ID, params.DeliveryID, false)
	if err != nil {
		return nil, err
	}
	return &gen.WebhookDeliveryHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: deliveryToAPI(d)}, nil
}

// RedeliverWebhookDelivery implements POST
// /v1/webhook-subscriptions/{id}/deliveries/{delivery_id}:redeliver.
func (e *Endpoints) RedeliverWebhookDelivery(ctx context.Context, params gen.RedeliverWebhookDeliveryParams) (gen.RedeliverWebhookDeliveryRes, error) {
	d, err := e.Redeliver(ctx, params.ID, params.DeliveryID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.RedeliverWebhookDeliveryOKHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: deliveryToAPI(d)}, nil
}

// ReplayWebhookDeliveries implements POST /v1/webhook-subscriptions/{id}:replay.
func (e *Endpoints) ReplayWebhookDeliveries(ctx context.Context, req *gen.WebhookReplay, params gen.ReplayWebhookDeliveriesParams) (gen.ReplayWebhookDeliveriesRes, error) {
	n, err := e.Replay(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Since)
	if err != nil {
		return nil, err
	}
	return &gen.WebhookReplayResultHeaders{Response: gen.WebhookReplayResult{Queued: int32(min(n, math.MaxInt32))}}, nil
}

// PingWebhookSubscription implements POST /v1/webhook-subscriptions/{id}:ping.
func (e *Endpoints) PingWebhookSubscription(ctx context.Context, params gen.PingWebhookSubscriptionParams) (gen.PingWebhookSubscriptionRes, error) {
	d, err := e.Ping(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.RedeliverWebhookDeliveryOKHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: deliveryToAPI(d)}, nil
}
