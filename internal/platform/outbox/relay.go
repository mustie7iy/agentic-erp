package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Message is one event in CloudEvents binary mode, ready for the bus.
type Message struct {
	ID      uuid.UUID
	Subject string            // erp.<cell>.<context>.<tenant>.<event>
	Header  map[string]string // ce-* attributes and Content-Type
	Data    []byte
}

// Publisher sends messages to the bus. It must de-duplicate on Message.ID,
// because a relay that crashes after publishing republishes the batch.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Relay moves unpublished outbox rows to the bus, oldest first.
type Relay struct {
	Pool      *database.Pool
	Role      string // erp_relay: may read every tenant's outbox and nothing else
	Publisher Publisher
	Cell      string
	Batch     int           // rows per context per round; default 100
	Interval  time.Duration // idle wait between rounds; default 1s
	Log       *slog.Logger
}

// RunOnce relays one batch from every context and returns how many events
// were published.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	total := 0
	for _, c := range Contexts {
		n, err := r.relay(ctx, c)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *Relay) batch() int {
	if r.Batch <= 0 {
		return 100
	}
	return r.Batch
}

type row struct {
	tenant, id, aggregate, actor uuid.UUID
	eventType, traceID           string
	payload                      []byte
	occurred                     time.Time
}

func (r *Relay) relay(ctx context.Context, schema string) (int, error) {
	published := 0
	err := r.Pool.InTx(ctx, database.Scope{Role: r.Role}, func(ctx context.Context, tx pgx.Tx) error {
		// One publisher per context at a time keeps events in order per
		// resource: without the context's advisory lock the query returns nothing.
		table := pgx.Identifier{schema, "outbox"}.Sanitize()
		rows, _ := tx.Query(ctx, `WITH lock AS MATERIALIZED (SELECT pg_try_advisory_xact_lock(hashtext('erp.outbox.' || $2)) AS held)
			SELECT tenant_id, id, aggregate_id, actor_id, event_type, coalesce(trace_id, ''), payload, occurred_at
			FROM `+table+`, lock WHERE lock.held AND published_at IS NULL ORDER BY occurred_at, id LIMIT $1`, r.batch(), schema)
		var rs []row
		var x row
		if _, err := pgx.ForEachRow(rows, []any{&x.tenant, &x.id, &x.aggregate, &x.actor, &x.eventType, &x.traceID, &x.payload, &x.occurred},
			func() error { rs = append(rs, x); return nil }); err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		if len(rs) == 0 {
			return nil
		}
		tenants, ids := make([]uuid.UUID, len(rs)), make([]uuid.UUID, len(rs))
		for i, e := range rs {
			if err := r.Publisher.Publish(ctx, r.message(schema, e)); err != nil {
				return fmt.Errorf("publish %s: %w", e.id, err)
			}
			tenants[i], ids[i] = e.tenant, e.id
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET published_at = now()
			WHERE (tenant_id, id) IN (SELECT * FROM unnest($1::uuid[], $2::uuid[]))`, tenants, ids); err != nil {
			return fmt.Errorf("mark %d events published: %w", len(rs), err)
		}
		published = len(rs)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("outbox relay %s: %w", schema, err)
	}
	return published, nil
}

func (r *Relay) message(schema string, e row) Message {
	ctxName := PublicContext(schema)
	// com.example.erp.<context>.<event>.v<n> → <event>
	name := strings.TrimPrefix(e.eventType, "com.example.erp."+ctxName+".")
	// The outbox's CHECK refuses a type without the version suffix. Should
	// one get past it, it is published under the name it has: refusing it
	// would leave the row at the head of the context's ordered queue and
	// stop every event behind it, and ADR-0007 promises delivery.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[:i]
	}
	h := map[string]string{
		"ce-specversion":     "1.0",
		"ce-id":              e.id.String(),
		"ce-source":          "/erp/" + r.Cell + "/" + ctxName,
		"ce-type":            e.eventType,
		"ce-subject":         e.aggregate.String(),
		"ce-time":            e.occurred.UTC().Format(time.RFC3339Nano),
		"ce-datacontenttype": "application/json",
		"ce-tenantid":        e.tenant.String(),
		"ce-actorid":         e.actor.String(),
		"Content-Type":       "application/json",
	}
	if e.traceID != "" {
		h["ce-traceid"] = e.traceID
	}
	return Message{
		ID:      e.id,
		Subject: strings.Join([]string{"erp", r.Cell, ctxName, e.tenant.String(), name}, "."),
		Header:  h,
		Data:    e.payload,
	}
}

// Run relays until ctx ends. A round that published a full batch is
// followed immediately by the next; errors are logged and retried after the
// interval.
func (r *Relay) Run(ctx context.Context) {
	worker.Loop(ctx, r.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := r.RunOnce(ctx)
		return n >= r.batch(), err
	}, r.Log, "outbox relay failed")
}
