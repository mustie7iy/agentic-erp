// Package outbox writes domain events to the owning context's outbox in the
// request's transaction and relays them to the event bus (ADR-0007,
// ADR-0014). The envelope is specified in api/asyncapi.yaml.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Contexts are the database schemas that have an outbox, in relay order.
var Contexts = []string{"platform", "tenancy", "identity", "config", "partner", "ledger", "procurement",
	"payables", "payments", "workflow", "integration", "agents", "cash", "controlling", "tax"}

// PublicContext is the context name events carry: schema config publishes as
// configuration; every other schema publishes under its own name.
func PublicContext(schema string) string {
	if schema == "config" {
		return "configuration"
	}
	return schema
}

// Event is a domain event to publish.
type Event struct {
	Context       string    // owning schema, one of Contexts
	Name          string    // snake_case event name, e.g. operation_completed
	Version       int       // major version; 0 means 1
	AggregateType string    // e.g. platform.operation
	AggregateID   uuid.UUID // the resource the event is about (CloudEvents subject)
	Payload       any       // JSON-encodable facts
}

// Type is the CloudEvents type, e.g. com.example.erp.platform.operation_completed.v1.
func (e Event) Type() string {
	v := e.Version
	if v == 0 {
		v = 1
	}
	return fmt.Sprintf("com.example.erp.%s.%s.v%d", PublicContext(e.Context), e.Name, v)
}

// Date is an optional date as event payloads carry it: YYYY-MM-DD, or
// null when there is none.
func Date(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.DateOnly)
}

// ErrOutsideRequest means Add was called without a request transaction.
var ErrOutsideRequest = errors.New("outbox: no transaction and principal in context (outside a unit of work)")

// Add writes the event in the request's transaction, as the request's actor.
func Add(ctx context.Context, e Event) (uuid.UUID, error) {
	tx, ok := reqctx.TxFrom(ctx)
	p, ok2 := reqctx.PrincipalFrom(ctx)
	if !ok || !ok2 {
		return uuid.Nil, ErrOutsideRequest
	}
	return Write(ctx, tx, p.TenantID, p.ID, reqctx.TraceID(ctx), e)
}

// Write inserts the event into its context's outbox and returns its id.
func Write(ctx context.Context, tx pgx.Tx, tenant, actor uuid.UUID, traceID string, e Event) (uuid.UUID, error) {
	if !slices.Contains(Contexts, e.Context) {
		return uuid.Nil, fmt.Errorf("outbox: unknown context %q", e.Context)
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("outbox: %s payload: %w", e.Type(), err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{e.Context, "outbox"}.Sanitize()+
		` (tenant_id, id, aggregate_type, aggregate_id, event_type, payload, actor_id, trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,nullif($8, ''))`,
		tenant, id, e.AggregateType, e.AggregateID, e.Type(), payload, actor, traceID); err != nil {
		return uuid.Nil, fmt.Errorf("outbox: %s: %w", e.Type(), err)
	}
	return id, nil
}
