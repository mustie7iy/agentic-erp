package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// Webhook fan-out (ADR-0041, T50e): the events the outbox relay publishes
// are read from JetStream by a durable consumer, and each event offered to
// webhooks becomes one pending delivery per active or paused subscription
// naming its type or context: a paused one holds its deliveries until it is
// resumed. The unique (tenant_id, subscription_id, event_id) is the
// inbox: an event read twice (a lost acknowledgement, a second replica)
// adds nothing.

// FanoutConsumer is the durable JetStream consumer the fan-out reads with;
// replicas share it, so each event reaches one of them.
const FanoutConsumer = "webhook-fanout"

// WebhookContexts are the public names of the contexts offering events to
// webhooks: the streams the fan-out reads.
func WebhookContexts() []string {
	var out []string
	for _, t := range webhookEvents {
		if c := eventContext(t); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// eventContext is the context of a type com.example.erp.<context>.<event>.v<n>.
func eventContext(t string) string {
	return strings.SplitN(strings.TrimPrefix(t, "com.example.erp."), ".", 2)[0]
}

// Fanout turns bus events into webhook deliveries.
type Fanout struct {
	Pool    *database.Pool
	AppRole string // the tenant-scoped role: the fan-out runs under the event's tenant
	Log     *slog.Logger
	Begin   database.BeginFunc // nil means Pool.Begin; tests inject failures
}

// fanEvent is a bus event offered to webhooks, with its body rendered.
type fanEvent struct {
	tenant, id, subject uuid.UUID
	typ                 string
	occurred            time.Time // the event's time, which orders a resource's deliveries
	body                []byte    // the structured-mode CloudEvent
}

// errRefused marks an event the fan-out will never accept; it is logged and
// acknowledged, since reading it again cannot change the answer.
var errRefused = errors.New("refused")

// Handle fans one bus event out. An event not offered to webhooks is
// skipped; one that is malformed, or whose subject names another tenant than
// its tenantid, is refused and logged. Both are acknowledged (nil). An
// error is a database failure: the event is read again.
func (f *Fanout) Handle(ctx context.Context, m outbox.Message) error {
	ev, offered, err := parseEvent(m)
	if err != nil {
		f.Log.WarnContext(ctx, "webhook fan-out refused an event", "subject", m.Subject, "ce_id", m.Header["ce-id"], "error", err)
		return nil
	}
	if !offered {
		return nil
	}
	tx, err := database.BeginOr(ctx, f.Pool, f.Begin, database.Scope{Role: f.AppRole, TenantID: ev.tenant})
	if err != nil {
		return fmt.Errorf("integration: fan out event %s: %w", ev.id, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fanout(ctx, tx, ev); err != nil {
		return fmt.Errorf("integration: fan out event %s: %w", ev.id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("integration: fan out event %s: commit: %w", ev.id, err)
	}
	return nil
}

// parseEvent reads a bus event: whether it is offered to webhooks and, if
// so, its tenant (the subject's, which must be its tenantid), ids and body.
func parseEvent(m outbox.Message) (ev fanEvent, offered bool, err error) {
	ev.typ = m.Header["ce-type"]
	if _, found := slices.BinarySearch(webhookEvents, ev.typ); !found {
		return ev, false, nil
	}
	// erp.<cell>.<context>.<tenant>.<event>
	parts := strings.Split(m.Subject, ".")
	if len(parts) != 5 || parts[2] != eventContext(ev.typ) {
		return ev, true, fmt.Errorf("%w: subject is not erp.<cell>.<context>.<tenant>.<event> of the type's context", errRefused)
	}
	if ev.tenant, err = uuid.Parse(parts[3]); err != nil || parts[3] != m.Header["ce-tenantid"] {
		return ev, true, fmt.Errorf("%w: the subject's tenant %q is not the event's tenantid %q", errRefused, parts[3], m.Header["ce-tenantid"])
	}
	if ev.id, err = uuid.Parse(m.Header["ce-id"]); err != nil {
		return ev, true, fmt.Errorf("%w: id: %w", errRefused, err)
	}
	if ev.subject, err = uuid.Parse(m.Header["ce-subject"]); err != nil {
		return ev, true, fmt.Errorf("%w: subject attribute: %w", errRefused, err)
	}
	if ev.occurred, err = time.Parse(time.RFC3339Nano, m.Header["ce-time"]); err != nil {
		return ev, true, fmt.Errorf("%w: time: %w", errRefused, err)
	}
	if ev.body, err = structured(m); err != nil {
		return ev, true, fmt.Errorf("%w: data: %w", errRefused, err)
	}
	return ev, true, nil
}

// structured renders the event in CloudEvents structured JSON mode: every
// ce-* attribute the bus carries and the data as JSON. Keys are sorted, so
// the same event always renders the same bytes.
func structured(m outbox.Message) ([]byte, error) {
	doc := map[string]any{"data": json.RawMessage(m.Data)}
	for k, v := range m.Header {
		if name, ok := strings.CutPrefix(k, "ce-"); ok {
			doc[name] = v
		}
	}
	return json.Marshal(doc) //nolint:wrapcheck // the caller says what failed to render
}

// fanout inserts one pending delivery per active or paused subscription of
// the tenant naming the event's type or context; one that exists is kept. The
// subscriptions are read FOR SHARE, so a concurrent pause or delete waits
// for the fan-out or is seen by it.
func fanout(ctx context.Context, tx pgx.Tx, ev fanEvent) error {
	rows, _ := tx.Query(ctx, `SELECT id FROM integration.webhook_subscription
		WHERE status IN ('active', 'paused') AND ($1 = ANY (event_types) OR $2 = ANY (event_types)) ORDER BY id FOR SHARE`,
		ev.typ, eventContext(ev.typ)+".*")
	subs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("read subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(subs))
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO integration.webhook_delivery
		(tenant_id, id, subscription_id, event_id, type, subject, occurred_at, body, status, next_attempt_at)
		SELECT current_setting('app.tenant_id')::uuid, d.id, d.sub, $3, $4, $5, $7, $6, 'pending', now()
		FROM unnest($1::uuid[], $2::uuid[]) AS d (id, sub)
		ON CONFLICT (tenant_id, subscription_id, event_id) DO NOTHING`,
		ids, subs, ev.id, ev.typ, ev.subject, ev.body, ev.occurred); err != nil {
		return fmt.Errorf("insert deliveries: %w", err)
	}
	return nil
}
