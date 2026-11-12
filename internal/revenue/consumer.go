package revenue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// The automatic contract (ADR-0068 §4; product owner 2026-11-12): every
// confirmed sales order gets its revenue contract without the sales
// context knowing of revenue, which is a leaf. The sales context
// publishes SalesOrderChanged through its outbox (ADR-0007); the relay
// puts it on the event bus, and this consumer reads it there, at least
// once, and creates the contract of an order that was confirmed. Two
// things make a second reading add nothing: the event's id is recorded in
// revenue.inbox in the contract's transaction, and an order has one
// contract (contract_one_per_sales_order), which also settles the race
// with a person's request for the same order.

// ContractConsumer is the durable JetStream consumer the contracts are
// created with; replicas share it, so each event reaches one of them.
const ContractConsumer = "revenue-contracts"

// orderChanged is the event type the consumer acts on.
const orderChanged = "com.example.erp.sales.sales_order_changed.v1"

// ConsumedContexts are the public names of the contexts whose streams the
// consumer reads.
func ConsumedContexts() []string { return []string{"sales"} }

// BuilderActor is the principal of the contracts the consumer creates,
// in every tenant: the system's, since the person who confirmed the order
// did not ask for a revenue contract and need not hold the permission to
// create one.
var BuilderActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:revenue-contract-builder"))

// Consumer turns the sales context's bus events into revenue contracts.
type Consumer struct {
	Endpoints *Endpoints
	Pool      *database.Pool
	AppRole   string // the tenant-scoped role: a contract is created under the event's tenant
	Log       *slog.Logger
	Begin     database.BeginFunc // nil means Pool.Begin; tests inject failures
	Now       func() time.Time   // stamps the audit record; nil: the wall clock
}

// confirmation is a SalesOrderChanged event as the consumer reads it.
type confirmation struct {
	tenant, id, order uuid.UUID
	status            string
}

// Handle creates the revenue contract of the sales order a
// SalesOrderChanged event says was confirmed. Another event is skipped,
// as is an order's change to another status. A malformed event, or one
// whose subject names another tenant than its tenantid, is refused and
// logged, and so is an order no contract is made for (one in a foreign
// currency, ADR-0068 §3): reading them again cannot change the answer,
// so all of these are acknowledged (nil). An error is a database
// failure: the event is read again.
func (c *Consumer) Handle(ctx context.Context, m outbox.Message) error {
	if m.Header["ce-type"] != orderChanged {
		return nil
	}
	ev, err := parseConfirmation(m)
	if err != nil {
		c.Log.WarnContext(ctx, "revenue contracts refused an event", "subject", m.Subject, "ce_id", m.Header["ce-id"], "error", err)
		return nil
	}
	if ev.status != sales.Confirmed {
		return nil
	}
	job := uow.Job{Begin: c.begin, Scope: database.Scope{Role: c.AppRole, TenantID: ev.tenant}, Now: c.Now,
		Name: "revenue: contract of sales order " + ev.order.String()}
	builder := reqctx.Principal{ID: BuilderActor, Kind: reqctx.KindSystem, TenantID: ev.tenant}
	var tx pgx.Tx // the job's transaction, which the unit of work's context also holds
	err = job.As(ctx, func(ctx context.Context, begun pgx.Tx) (reqctx.Principal, bool, error) {
		tx = begun
		first, err := claim(ctx, tx, ev)
		return builder, first, err
	}, func(ctx context.Context) error { return c.contract(ctx, tx, builder, ev) })
	if err != nil {
		return fmt.Errorf("revenue: contract of sales order %s (event %s): %w", ev.order, ev.id, err)
	}
	return nil
}

func (c *Consumer) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, c.Pool, c.Begin, s)
}

// parseConfirmation reads a SalesOrderChanged bus event: its tenant (the
// subject's, which must be its tenantid), its id, and the order and the
// status its data names.
func parseConfirmation(m outbox.Message) (confirmation, error) {
	var ev confirmation
	var err error
	if ev.tenant, err = m.Tenant("sales"); err != nil {
		return ev, err
	}
	if ev.id, err = uuid.Parse(m.Header["ce-id"]); err != nil {
		return ev, fmt.Errorf("id: %w", err)
	}
	var data struct {
		Order  uuid.UUID `json:"sales_order_id"`
		Status string    `json:"status"`
	}
	if err := json.Unmarshal(m.Data, &data); err != nil {
		return ev, fmt.Errorf("data: %w", err)
	}
	ev.order, ev.status = data.Order, data.Status
	return ev, nil
}

// claim records the event in the context's inbox (ADR-0007) and says
// whether this is its first reading; the record commits with what the
// reading does, so an event is acted on once.
func claim(ctx context.Context, tx pgx.Tx, ev confirmation) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO revenue.inbox (tenant_id, consumer, event_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		ev.tenant, ContractConsumer, ev.id)
	if err != nil {
		return false, fmt.Errorf("record event %s in the inbox: %w", ev.id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// contract creates the confirmed order's contract unless it has one. An
// order no contract is made for is logged and left: the event stays
// recorded as read.
func (c *Consumer) contract(ctx context.Context, tx pgx.Tx, builder reqctx.Principal, ev confirmation) error {
	b, no, err := c.Endpoints.basisOf(ctx, ev.order, func(uuid.UUID) bool { return true })
	if err != nil {
		return err
	}
	if no != nil {
		c.Log.InfoContext(ctx, "no revenue contract for a confirmed sales order", "sales_order_id", ev.order, "code", no.code, "detail", no.detail)
		return nil
	}
	_, _, err = c.Endpoints.make(ctx, tx, builder, b, byConfirmation)
	return err
}
