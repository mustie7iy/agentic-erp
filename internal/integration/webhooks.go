package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Webhook subscriptions (ADR-0041, T50d): a tenant's HTTPS endpoints and
// the event types each receives. The subscription API only records them;
// the dispatcher (T50e) delivers. Changes are audited; they publish no
// event, since nothing inside the platform reacts to them.

func init() {
	// Both secrets are re-sealed when the tenant's key is rotated; the
	// previous one is NULL outside a rotation.
	for _, c := range []string{"secret_cipher", "previous_secret_cipher"} {
		fieldcrypt.Register(fieldcrypt.Column{Table: "integration.webhook_subscription", Keys: []string{"id"}, Cipher: c, Versioned: true})
	}
}

// MaxSubscriptions is how many subscriptions that are not deleted a tenant
// may have (ADR-0041).
const MaxSubscriptions = 20

// SecretOverlap is how long the previous secret keeps signing after a
// rotation (ADR-0041).
const SecretOverlap = 24 * time.Hour

// secretBytes is the key length of a new secret: Standard Webhooks allows
// 24 to 64.
const secretBytes = 32

// webhookEvents are the event types a subscription may name: those marked
// x-webhook in the embedded AsyncAPI contract, sorted.
var webhookEvents = api.MustWebhookEvents()

// Sealer seals a value with the tenant's field key (fieldcrypt.Keyring).
type Sealer interface {
	Seal(ctx context.Context, tenant uuid.UUID, value string) ([]byte, error)
}

// Subscription is a row of integration.webhook_subscription, without its
// secrets.
type Subscription struct {
	ID                uuid.UUID
	URL               string
	Description       string
	EventTypes        []string
	Status            string
	DisabledReason    *string
	FailingSince      *time.Time
	PreviousExpiresAt *time.Time // only while the previous secret still signs
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
	Secret            string // the whsec_ secret, set only by create and rotate
}

const subscriptionColumns = `id, url, description, event_types, status, disabled_reason, failing_since,
	CASE WHEN previous_expires_at > now() THEN previous_expires_at END, version, created_at, created_by, updated_at, updated_by`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.URL, &s.Description, &s.EventTypes, &s.Status, &s.DisabledReason, &s.FailingSince,
		&s.PreviousExpiresAt, &s.Version, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt, &s.UpdatedBy)
	return s, errs.Wrapf(err, "scan webhook subscription")
}

func getSubscription(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Subscription, error) {
	q := `SELECT ` + subscriptionColumns + ` FROM integration.webhook_subscription WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	s, err := scanSubscription(tx.QueryRow(ctx, q, id))
	if noRows(err) {
		return s, problem.NotFoundOf("webhook subscription")
	}
	if err != nil {
		return s, fmt.Errorf("integration: read webhook subscription %s: %w", id, err)
	}
	return s, nil
}

// changeable reads a subscription for a change: locked, at the version the
// client read, and not deleted.
func changeable(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	s, err := getSubscription(ctx, tx, id, true)
	if err != nil {
		return s, err
	}
	if err := ifMatch(s.Version); err != nil {
		return s, err
	}
	if s.Status == "deleted" {
		return s, problem.Conflict("integration.webhook_deleted", "The webhook subscription is deleted.")
	}
	return s, nil
}

// checkEventTypes accepts event types offered to webhooks and `<context>.*`
// for a context that offers any, dropping duplicates in order.
func checkEventTypes(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for i, t := range in {
		if !offered(t) {
			msg := "An event type marked x-webhook in the AsyncAPI contract, or <context>.* for a context offering any."
			return nil, problem.Unprocessable("integration.unknown_event_type", msg,
				problem.FieldError{Field: fmt.Sprintf("event_types[%d]", i), Code: "invalid", Message: msg})
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

func offered(t string) bool {
	if c, ok := strings.CutSuffix(t, ".*"); ok {
		return !strings.Contains(c, ".") && slices.ContainsFunc(webhookEvents, func(e string) bool {
			return strings.HasPrefix(e, "com.example.erp."+c+".")
		})
	}
	_, found := slices.BinarySearch(webhookEvents, t)
	return found
}

// checkURL applies the egress client's rules when the URL is written, so a
// tenant learns at once of an endpoint that could never be reached.
func (e *Endpoints) checkURL(ctx context.Context, raw string) error {
	if err := egress.CheckURL(ctx, e.Resolver, raw); err != nil {
		return invalid("integration.invalid_webhook_url", "url",
			"An https URL without credentials or fragment, whose host resolves (now) to public addresses only; a host that failed to resolve may be retried.")
	}
	return nil
}

// host is the URL's host, all the audit trail keeps of it: the path or
// query may carry the endpoint's token; "" for a URL that does not parse.
func host(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// newSecret makes a secret: the whsec_ form shown to the client once, and
// the form sealed. fieldcrypt seals values upper-cased (its normal form for
// account numbers), which base64 would not survive, so the key is sealed
// in hex, which does.
func newSecret() (shown, sealable string) {
	key := make([]byte, secretBytes)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error: it crashes the program instead (Go 1.24+)
	return "whsec_" + base64.StdEncoding.EncodeToString(key), hex.EncodeToString(key)
}

// sealSecret makes and seals a new secret for the request's tenant.
func (e *Endpoints) sealSecret(ctx context.Context, tenant uuid.UUID) (shown string, sealed []byte, err error) {
	shown, sealable := newSecret()
	sealed, err = e.Keys.Seal(ctx, tenant, sealable)
	if err != nil {
		return "", nil, fmt.Errorf("integration: seal webhook secret: %w", err)
	}
	return shown, sealed, nil
}

// NewSubscription is a subscription as a client creates it.
type NewSubscription struct {
	URL, Description string
	EventTypes       []string
}

// lockSubscriptions serializes the tenant's subscription creations, so
// two cannot both pass the limit.
func lockSubscriptions(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('integration.webhook_subscription:' || platform.require_tenant(), 0))`); err != nil {
		return fmt.Errorf("integration: lock webhook subscriptions: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM integration.webhook_subscription WHERE status <> 'deleted'`).Scan(&n); err != nil {
		return fmt.Errorf("integration: count webhook subscriptions: %w", err)
	}
	if n >= MaxSubscriptions {
		return problem.Unprocessable("integration.webhook_limit",
			fmt.Sprintf("A tenant may have at most %d webhook subscriptions that are not deleted; delete one first.", MaxSubscriptions))
	}
	return nil
}

// CreateSubscription creates an active subscription with a new secret,
// which only the answer carries.
func (e *Endpoints) CreateSubscription(ctx context.Context, in NewSubscription) (Subscription, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Subscription{}, err
	}
	if err := e.checkURL(ctx, in.URL); err != nil {
		return Subscription{}, err
	}
	types, err := checkEventTypes(in.EventTypes)
	if err != nil {
		return Subscription{}, err
	}
	if err := lockSubscriptions(ctx, tx); err != nil {
		return Subscription{}, err
	}
	secret, sealed, err := e.sealSecret(ctx, p.TenantID)
	if err != nil {
		return Subscription{}, err
	}
	s, err := scanSubscription(tx.QueryRow(ctx, `INSERT INTO integration.webhook_subscription
		(tenant_id, id, url, description, event_types, status, secret_cipher, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $7) RETURNING `+subscriptionColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), in.URL, in.Description, types, sealed, p.ID))
	if err != nil {
		return Subscription{}, fmt.Errorf("integration: create webhook subscription: %w", err)
	}
	s.Secret = secret
	return s, record(ctx, "integration.webhook_subscription.create", s.ID, map[string]any{"host": host(s.URL), "event_types": s.EventTypes})
}

// SubscriptionChange is a merge patch of a subscription.
type SubscriptionChange struct {
	URL, Description *string
	EventTypes       []string // replaced as a whole when not nil
}

// UpdateSubscription changes a subscription's URL, description or event
// types, checked as on create.
func (e *Endpoints) UpdateSubscription(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch SubscriptionChange) (Subscription, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Subscription{}, err
	}
	// The URL is checked before the row is locked: the check resolves its
	// host, which may take a while.
	if ch.URL != nil {
		if err := e.checkURL(ctx, *ch.URL); err != nil {
			return Subscription{}, err
		}
	}
	s, err := changeable(ctx, tx, id, ifMatch)
	if err != nil {
		return Subscription{}, err
	}
	if ch.URL != nil {
		s.URL = *ch.URL
	}
	if ch.Description != nil {
		s.Description = *ch.Description
	}
	if ch.EventTypes != nil {
		if s.EventTypes, err = checkEventTypes(ch.EventTypes); err != nil {
			return Subscription{}, err
		}
	}
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE integration.webhook_subscription SET url = $2, description = $3, event_types = $4,
		version = version + 1, updated_by = $5 WHERE id = $1 RETURNING `+subscriptionColumns, id, s.URL, s.Description, s.EventTypes, p.ID))
	if err != nil {
		return Subscription{}, fmt.Errorf("integration: update webhook subscription %s: %w", id, err)
	}
	return updated, record(ctx, "integration.webhook_subscription.update", id,
		map[string]any{"host": host(updated.URL), "url_changed": ch.URL != nil, "event_types": updated.EventTypes})
}

// DeleteSubscription ends a subscription: status deleted, nothing removed
// (03 §3.1). Its previous secret, if any, is dropped with it.
func (e *Endpoints) DeleteSubscription(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Subscription{}, err
	}
	s, err := changeable(ctx, tx, id, ifMatch)
	if err != nil {
		return Subscription{}, err
	}
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE integration.webhook_subscription SET status = 'deleted', disabled_reason = NULL,
		previous_secret_cipher = NULL, previous_expires_at = NULL, version = version + 1, updated_by = $2
		WHERE id = $1 RETURNING `+subscriptionColumns, id, p.ID))
	if err != nil {
		return Subscription{}, fmt.Errorf("integration: delete webhook subscription %s: %w", id, err)
	}
	return updated, record(ctx, "integration.webhook_subscription.delete", id, map[string]any{"host": host(updated.URL), "previous_status": s.Status})
}

// RotateSecret issues a new secret; the current one keeps signing for
// SecretOverlap. A rotation within that time retires the older previous
// secret at once: only two secrets ever sign.
func (e *Endpoints) RotateSecret(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Subscription{}, err
	}
	if _, err := changeable(ctx, tx, id, ifMatch); err != nil {
		return Subscription{}, err
	}
	secret, sealed, err := e.sealSecret(ctx, p.TenantID)
	if err != nil {
		return Subscription{}, err
	}
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE integration.webhook_subscription SET previous_secret_cipher = secret_cipher,
		previous_expires_at = now() + $3 * interval '1 second', secret_cipher = $2, version = version + 1, updated_by = $4
		WHERE id = $1 RETURNING `+subscriptionColumns, id, sealed, int(SecretOverlap.Seconds()), p.ID))
	if err != nil {
		return Subscription{}, fmt.Errorf("integration: rotate webhook subscription %s secret: %w", id, err)
	}
	updated.Secret = secret
	return updated, record(ctx, "integration.webhook_subscription.rotate_secret", id,
		map[string]any{"host": host(updated.URL), "previous_expires_at": updated.PreviousExpiresAt})
}

// PauseSubscription holds an active subscription's deliveries: the fan-out
// keeps recording them and none is sent until it is resumed, when they go
// out in order (those past the retention are gone).
func (e *Endpoints) PauseSubscription(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	return e.moveSubscription(ctx, id, ifMatch, "active", "paused", "pause",
		problem.Conflict("integration.webhook_not_active", "Only an active webhook subscription can be paused."))
}

// ResumeSubscription sends a paused subscription's held deliveries and the
// later ones as they come.
func (e *Endpoints) ResumeSubscription(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Subscription, error) {
	return e.moveSubscription(ctx, id, ifMatch, "paused", "active", "resume",
		problem.Conflict("integration.webhook_not_paused", "Only a paused webhook subscription can be resumed."))
}

// moveSubscription moves a subscription from one status to another,
// audited as verb; refused is the answer when it is in another status.
func (e *Endpoints) moveSubscription(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, from, to, verb string, refused error) (Subscription, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Subscription{}, err
	}
	s, err := changeable(ctx, tx, id, ifMatch)
	if err != nil {
		return Subscription{}, err
	}
	if s.Status != from {
		return Subscription{}, refused
	}
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE integration.webhook_subscription SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+subscriptionColumns, id, to, p.ID))
	if err != nil {
		return Subscription{}, fmt.Errorf("integration: %s webhook subscription %s: %w", verb, id, err)
	}
	return updated, record(ctx, "integration.webhook_subscription."+verb, id, map[string]any{"host": host(updated.URL)})
}
