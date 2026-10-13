// Package operations is the platform's long-running operations resource
// (ADR-0012, 04-api-design §4.8): work that outlives a request is tracked as
// an operation that clients poll, list and cancel. Contexts start operations
// with Start; workers report progress and completion.
//
// It is the first context built on the T1 skeleton: handlers implement the
// generated API interface and use the request transaction, audit and outbox
// from the unit of work, never their own.
package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses.
const (
	Queued    = "queued"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Operation is one row of platform.operation.
type Operation struct {
	ID            uuid.UUID
	Kind          string
	Status        string
	ProgressDone  int64
	ProgressTotal *int64
	Resource      *string
	Result        []byte // JSON or nil
	Error         []byte // problem JSON or nil
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
	WorkflowID    *string // the workflow running the work, once dispatched (ADR-0012)
}

// Cancellable is true while the work has not finished.
func (o Operation) Cancellable() bool { return o.Status == Queued || o.Status == Running }

const columns = `id, kind, status, progress_done, progress_total, resource, result, error, version, created_at, created_by, updated_at, updated_by, workflow_id`

func scan(row pgx.Row) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.Kind, &o.Status, &o.ProgressDone, &o.ProgressTotal, &o.Resource, &o.Result, &o.Error,
		&o.Version, &o.CreatedAt, &o.CreatedBy, &o.UpdatedAt, &o.UpdatedBy, &o.WorkflowID)
	return o, err
}

func request(ctx context.Context) (pgx.Tx, reqctx.Principal, error) {
	tx, ok := reqctx.TxFrom(ctx)
	p, ok2 := reqctx.PrincipalFrom(ctx)
	if !ok || !ok2 {
		return nil, reqctx.Principal{}, errors.New("operations: outside a unit of work")
	}
	return tx, p, nil
}

// Start records a new queued operation in the request's transaction.
func Start(ctx context.Context, kind string, req any) (Operation, error) {
	return start(ctx, kind, req, nil)
}

// StartFor records a new queued operation for a resource the request
// created in an in-progress status (a path such as /v1/payment-runs/{id}),
// so clients can follow it from the start (04-api-design §4.8).
func StartFor(ctx context.Context, kind string, req any, resource string) (Operation, error) {
	return start(ctx, kind, req, &resource)
}

func start(ctx context.Context, kind string, req any, resource *string) (Operation, error) {
	tx, p, err := request(ctx)
	if err != nil {
		return Operation{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Operation{}, fmt.Errorf("operations: request: %w", err)
	}
	return scan(tx.QueryRow(ctx, `INSERT INTO platform.operation (tenant_id, id, kind, status, request, resource, created_by, updated_by)
		VALUES ($1, $2, $3, 'queued', $4, $5, $6, $6) RETURNING `+columns, p.TenantID, uuid.Must(uuid.NewV7()), kind, body, resource, p.ID))
}

// Get reads an operation in the request's transaction.
func Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	tx, _, err := request(ctx)
	if err != nil {
		return Operation{}, err
	}
	return get(ctx, tx, id, false)
}

// get loads an operation; lock takes the row lock for a change.
func get(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Operation, error) {
	q := `SELECT ` + columns + ` FROM platform.operation WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	o, err := scan(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return o, problem.NotFound("No operation with this id.")
	}
	return o, err
}

// filter selects operations for a list.
type filter struct {
	Status, Kind string
	Ascending    bool
	After        uuid.UUID // cursor: the last id of the previous page
	Limit        int
}

// list orders by id: UUIDv7 ids are time-ordered, so this is creation order
// and every page is an index range scan on the primary key.
func list(ctx context.Context, tx pgx.Tx, f filter) ([]Operation, error) {
	cmp, dir := "<", "DESC"
	if f.Ascending {
		cmp, dir = ">", "ASC"
	}
	q := `SELECT ` + columns + ` FROM platform.operation
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR kind = $2) AND ($3::uuid IS NULL OR id ` + cmp + ` $3)
		ORDER BY id ` + dir + ` LIMIT $4`
	var after *uuid.UUID
	if f.After != uuid.Nil {
		after = &f.After
	}
	rows, _ := tx.Query(ctx, q, f.Status, f.Kind, after, f.Limit)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Operation, error) { return scan(r) })
}

// cancel moves a running or queued operation to cancelled and publishes its
// completion.
func cancel(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o Operation) (Operation, error) {
	if !o.Cancellable() {
		return o, problem.Conflict("platform.operation_not_cancellable", "The operation has already "+o.Status+".")
	}
	updated, err := scan(tx.QueryRow(ctx, `UPDATE platform.operation SET status = 'cancelled', version = version + 1,
		updated_by = $2 WHERE id = $1 RETURNING `+columns, o.ID, p.ID))
	if err != nil {
		return o, err
	}
	if _, err := outbox.Add(ctx, completedEvent(updated)); err != nil {
		return o, err
	}
	return updated, nil
}

// completedEvent is OperationCompleted (api/asyncapi.yaml).
func completedEvent(o Operation) outbox.Event {
	var errPayload any
	if o.Error != nil {
		var pr struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(o.Error, &pr) // stored by the platform as a problem document
		errPayload = map[string]string{"code": pr.Code, "message": pr.Detail}
	}
	return outbox.Event{
		Context: "platform", Name: "operation_completed", AggregateType: "platform.operation", AggregateID: o.ID,
		Payload: map[string]any{
			"operation_id": o.ID, "kind": o.Kind, "status": o.Status, "resource": o.Resource,
			"progress": map[string]any{"done": o.ProgressDone, "total": o.ProgressTotal},
			"error":    errPayload,
		},
	}
}

// Succeed completes an operation whose work finished within the request
// that started it: status succeeded, the resource it produced (a path such
// as /v1/supplier-invoices/{id}) and its result.
func Succeed(ctx context.Context, id uuid.UUID, resource string, result any) (Operation, error) {
	body, err := json.Marshal(result)
	if err != nil {
		return Operation{}, fmt.Errorf("operations: result: %w", err)
	}
	return finish(ctx, `status = 'succeeded', resource = $2, result = $3`, id, resource, body)
}

// Fail completes an operation whose work could not be done, with the
// problem that stopped it.
func Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) (Operation, error) {
	body, _ := json.Marshal(p) // a problem is strings, ints and field errors
	return finish(ctx, `status = 'failed', error = $2`, id, body)
}

// finish sets a final status and publishes OperationCompleted; args follow
// the operation id from $2.
func finish(ctx context.Context, set string, id uuid.UUID, args ...any) (Operation, error) {
	tx, p, err := request(ctx)
	if err != nil {
		return Operation{}, err
	}
	o, err := scan(tx.QueryRow(ctx, `UPDATE platform.operation SET `+set+fmt.Sprintf(`, version = version + 1, updated_by = $%d WHERE id = $1 RETURNING `, len(args)+2)+columns,
		append(append([]any{id}, args...), p.ID)...))
	if err != nil {
		return Operation{}, err
	}
	if _, err := outbox.Add(ctx, completedEvent(o)); err != nil {
		return Operation{}, err
	}
	return o, nil
}

// Report records the progress of a running operation.
func Report(ctx context.Context, id uuid.UUID, done, total int64) error {
	tx, p, err := request(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE platform.operation SET status = 'running', progress_done = $2, progress_total = $3, version = version + 1,
		updated_by = $4 WHERE id = $1`, id, done, total, p.ID)
	return err
}
