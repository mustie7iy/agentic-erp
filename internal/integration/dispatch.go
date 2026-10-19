package integration

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Webhook delivery (ADR-0041, T50f): the dispatcher finds the due
// deliveries across tenants, claims each in its tenant's transaction by
// leasing it, sends it signed through the egress client and records the
// answer with a guard on the version it claimed. A delivery whose
// recording was lost is sent again when its lease ends: delivery is at
// least once, and the webhook-id (the delivery's id) lets the endpoint
// drop a duplicate.

// Dispatch limits (ADR-0041).
const (
	// TenantInFlight is how many deliveries of one tenant a replica sends
	// at once.
	TenantInFlight = 10
	// SubscriptionRate is how many deliveries a replica sends to one
	// subscription per second.
	SubscriptionRate = 50
	// DeliveryRetention is how long a delivery is kept, held ones too.
	DeliveryRetention = 30 * 24 * time.Hour
	// FailingLimit is how long a subscription may fail before it is
	// disabled.
	FailingLimit = 5 * 24 * time.Hour
	// RetryWindow bounds the retries: an attempt that would come later,
	// counted on the schedule from the first, is not made and the delivery
	// is dead lettered.
	RetryWindow = 3 * 24 * time.Hour
	// claimLease is how long a claimed delivery is not due again: well past
	// the egress client's 10 s, so only a replica that stopped mid-send
	// lets it lapse.
	claimLease = 2 * time.Minute
	// dispatchBatch is how many deliveries a round claims at most.
	dispatchBatch = 500
)

// backoff is the wait after each failed attempt; after the last, every
// laterBackoff.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

const laterBackoff = 12 * time.Hour

// DispatchActor is the system actor the dispatcher disables subscriptions
// as.
var DispatchActor = uuid.MustParse("0d4c1f8e-5b0a-4e7f-9a63-2f5d7c0e1b41")

// Opener opens a value sealed with the tenant's field key
// (fieldcrypt.Keyring).
type Opener interface {
	Open(ctx context.Context, tenant uuid.UUID, sealed []byte) (string, error)
}

// Poster sends a request to an endpoint (egress.Client).
type Poster interface {
	Post(ctx context.Context, rawURL string, header http.Header, body []byte) (egress.Response, error)
}

// Dispatcher delivers webhook deliveries.
type Dispatcher struct {
	Pool       *database.Pool
	AppRole    string // erp_app: each delivery is claimed and recorded in its tenant's scope
	WorkerRole string // erp_worker: finds due deliveries and prunes old ones across tenants
	Keys       Opener
	Client     Poster
	Interval   time.Duration // between rounds when one found nothing more; default one second
	Batch      int           // deliveries claimed per round; 0: 500
	Now        func() time.Time
	Log        *slog.Logger
	Begin      database.BeginFunc // nil means Pool.Begin; tests inject failures

	once    sync.Once
	limiter *ratelimit.Limiter
}

// claimed is a delivery leased for one attempt.
type claimed struct {
	tenant, id, sub uuid.UUID
	version         int32 // after the lease
	attempts        int32 // before this attempt
	url             string
	body            []byte
	sealed          [][]byte // the current secret, then the previous one while it signs
}

// answer is what an attempt got: an HTTP status, or none and why.
type answer struct {
	status int
	err    string
}

func (a answer) delivered() bool { return a.status >= 200 && a.status < 300 }

// Run delivers in rounds and prunes old deliveries hourly until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() {
		worker.Loop(ctx, time.Hour, time.Hour, func(ctx context.Context) (bool, error) {
			_, err := d.Prune(ctx)
			return false, err
		}, d.Log, "webhook delivery retention failed")
	})
	worker.Loop(ctx, d.Interval, time.Second, func(ctx context.Context) (bool, error) {
		_, more, err := d.RunOnce(ctx)
		return more, err
	}, d.Log, "webhook delivery round failed")
	wg.Wait()
}

// RunOnce claims the due deliveries, every tenant's first before any
// tenant's second and at most TenantInFlight of each, sends them and
// records the answers. It returns how many it sent and whether it found a
// full batch, so the next round runs at once. A tenant whose deliveries
// cannot be claimed or recorded is reported and the others' are sent
// still.
func (d *Dispatcher) RunOnce(ctx context.Context) (sent int, more bool, err error) {
	now := clock.Now(d.Now)
	due, more, err := d.due(ctx, now)
	if err != nil {
		return 0, false, err
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)
	fail := func(err error) {
		mu.Lock()
		failed = append(failed, err)
		mu.Unlock()
	}
	for tenant, ids := range due {
		claims, err := d.claim(ctx, tenant, ids, now)
		if err != nil {
			fail(err)
			continue
		}
		sent += len(claims)
		for _, c := range claims {
			// A panic in a send is reported like a failed round rather
			// than ending the server.
			wg.Go(func() {
				if err := worker.Recover(func() error { return d.deliver(ctx, c) }); err != nil {
					fail(err)
				}
			})
		}
	}
	wg.Wait()
	// A full batch of which nothing was claimed (rate-limited or held by
	// another replica) waits for the interval rather than asking again.
	return sent, more && sent > 0, errors.Join(failed...)
}

// due finds the deliveries to attempt, by tenant: pending, due, of an
// active subscription (a paused one holds them) and the earliest pending
// of their subscription and resource, so a resource's events arrive in
// order and a dead letter releases the next.
func (d *Dispatcher) due(ctx context.Context, now time.Time) (map[uuid.UUID][]uuid.UUID, bool, error) {
	tx, err := database.BeginOr(ctx, d.Pool, d.Begin, database.Scope{Role: d.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("integration: find due webhook deliveries: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := dispatchBatch
	if d.Batch > 0 {
		batch = d.Batch
	}
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id FROM (
			SELECT d.tenant_id, d.id, d.next_attempt_at,
				row_number() OVER (PARTITION BY d.tenant_id ORDER BY d.next_attempt_at, d.id) AS turn
			FROM integration.webhook_delivery d
			JOIN integration.webhook_subscription s ON s.tenant_id = d.tenant_id AND s.id = d.subscription_id AND s.status = 'active'
			WHERE d.status = 'pending' AND d.next_attempt_at <= $1 AND NOT EXISTS (
				SELECT FROM integration.webhook_delivery e WHERE e.tenant_id = d.tenant_id AND e.subscription_id = d.subscription_id
					AND e.subject = d.subject AND e.status = 'pending' AND e.id < d.id)) h
		WHERE turn <= $2 ORDER BY turn, next_attempt_at, id LIMIT $3`, now, TenantInFlight, batch)
	out := map[uuid.UUID][]uuid.UUID{}
	var tenant, id uuid.UUID
	found := 0
	if _, err := pgx.ForEachRow(rows, []any{&tenant, &id}, func() error {
		out[tenant] = append(out[tenant], id)
		found++
		return nil
	}); err != nil {
		return nil, false, fmt.Errorf("integration: find due webhook deliveries: %w", err)
	}
	return out, found == batch, nil
}

func (d *Dispatcher) rate() *ratelimit.Limiter {
	d.once.Do(func() { d.limiter = &ratelimit.Limiter{Now: d.Now} })
	return d.limiter
}

// claim leases the tenant's deliveries that are still due, skipping those
// another replica holds and those whose subscription has used its rate.
func (d *Dispatcher) claim(ctx context.Context, tenant uuid.UUID, ids []uuid.UUID, now time.Time) ([]claimed, error) {
	tx, err := database.BeginOr(ctx, d.Pool, d.Begin, database.Scope{Role: d.AppRole, TenantID: tenant})
	if err != nil {
		return nil, fmt.Errorf("integration: claim webhook deliveries of tenant %s: %w", tenant, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT d.id, d.subscription_id, d.attempts, d.body, s.url, s.secret_cipher,
			CASE WHEN s.previous_expires_at > $2 THEN s.previous_secret_cipher END
		FROM integration.webhook_delivery d JOIN integration.webhook_subscription s ON s.id = d.subscription_id
		WHERE d.id = ANY ($1) AND d.status = 'pending' AND d.next_attempt_at <= $2 AND s.status = 'active' AND NOT EXISTS (
			SELECT FROM integration.webhook_delivery e WHERE e.subscription_id = d.subscription_id
				AND e.subject = d.subject AND e.status = 'pending' AND e.id < d.id)
		ORDER BY d.id FOR UPDATE OF d SKIP LOCKED`, ids, now)
	var claims []claimed
	quota := ratelimit.Quota{Rate: SubscriptionRate, Burst: SubscriptionRate}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		c := claimed{tenant: tenant}
		var current, previous []byte
		err := r.Scan(&c.id, &c.sub, &c.attempts, &c.body, &c.url, &current, &previous)
		c.sealed = [][]byte{current}
		if previous != nil {
			c.sealed = append(c.sealed, previous)
		}
		return c, errs.Wrapf(err, "scan webhook delivery")
	})
	if err != nil {
		return nil, fmt.Errorf("integration: claim webhook deliveries of tenant %s: %w", tenant, err)
	}
	for _, c := range all {
		if d.rate().Allow("webhook:"+c.sub.String(), quota) {
			claims = append(claims, c)
		}
	}
	return d.lease(ctx, tx, tenant, claims, now)
}

// lease makes the claimed deliveries not due for claimLease and commits.
func (d *Dispatcher) lease(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, claims []claimed, now time.Time) ([]claimed, error) {
	ids := make([]uuid.UUID, len(claims))
	for i, c := range claims {
		ids[i] = c.id
	}
	rows, _ := tx.Query(ctx, `UPDATE integration.webhook_delivery SET next_attempt_at = $2, version = version + 1
		WHERE id = ANY ($1) RETURNING id, version`, ids, now.Add(claimLease))
	versions := map[uuid.UUID]int32{}
	var id uuid.UUID
	var version int32
	if _, err := pgx.ForEachRow(rows, []any{&id, &version}, func() error {
		versions[id] = version
		return nil
	}); err != nil {
		return nil, fmt.Errorf("integration: lease webhook deliveries of tenant %s: %w", tenant, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("integration: commit the lease of tenant %s webhook deliveries: %w", tenant, err)
	}
	for i := range claims {
		claims[i].version = versions[claims[i].id]
	}
	return claims, nil
}

// secrets opens the delivery's signing secrets, sealed in hex.
func (d *Dispatcher) secrets(ctx context.Context, c claimed) ([]webhook.Secret, error) {
	out := make([]webhook.Secret, 0, len(c.sealed))
	for _, sealed := range c.sealed {
		plain, err := d.Keys.Open(ctx, c.tenant, sealed)
		if err != nil {
			return nil, fmt.Errorf("integration: open webhook secret of subscription %s: %w", c.sub, err)
		}
		key, err := hex.DecodeString(plain)
		if err != nil {
			return nil, fmt.Errorf("integration: webhook secret of subscription %s is not hex: %w", c.sub, err)
		}
		s, err := webhook.ParseSecret("whsec_" + base64.StdEncoding.EncodeToString(key))
		if err != nil {
			return nil, fmt.Errorf("integration: webhook secret of subscription %s: %w", c.sub, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// deliver makes one attempt and records it. A secret that cannot be opened
// is recorded as a failed attempt and reported; a stop while sending
// records nothing: the lease lapses and the delivery is attempted again.
func (d *Dispatcher) deliver(ctx context.Context, c claimed) error {
	secrets, err := d.secrets(ctx, c)
	if err != nil {
		// Counted as a failed attempt, so the schedule and the dead letter
		// apply and the resource's later deliveries are not held for good.
		if rerr := d.record(ctx, c, answer{err: "the subscription's signing secret could not be opened"}); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	h := http.Header{"Content-Type": {"application/cloudevents+json"}}
	webhook.SetHeaders(h, c.id.String(), clock.Now(d.Now), c.body, secrets...)
	resp, err := d.Client.Post(ctx, c.url, h, c.body)
	if ctx.Err() != nil {
		return fmt.Errorf("integration: webhook delivery %s stopped: %w", c.id, ctx.Err())
	}
	a := answer{status: resp.Status}
	if err != nil {
		a.err = truncate(err.Error(), 1000)
	}
	return d.record(ctx, c, a)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
