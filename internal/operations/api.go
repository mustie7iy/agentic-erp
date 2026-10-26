package operations

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
)

// Endpoints implements the operations part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	// Workflows cancels the workflow behind an operation (ADR-0012); nil
	// when the cell runs no workflow engine. The database row is the truth:
	// an activity that finds its operation cancelled fails the work itself,
	// so a cancellation the engine does not get is caught up with there.
	Workflows workflow.Client
	Log       *slog.Logger
}

// GetOperation implements GET /v1/operations/{id}.
func (e *Endpoints) GetOperation(ctx context.Context, params gen.GetOperationParams) (gen.GetOperationRes, error) {
	tx, _, err := request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := get(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetOperationOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: ToAPI(o)}, nil
}

type cursor struct {
	After uuid.UUID `json:"a"`
}

// ListOperations implements GET /v1/operations.
func (e *Endpoints) ListOperations(ctx context.Context, params gen.ListOperationsParams) (gen.ListOperationsRes, error) {
	tx, _, err := request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	f := filter{Status: params.Status.Value, Kind: params.Kind.Value, Limit: size + 1}
	switch strings.Join(strings.Fields(params.OrderBy.Value), " ") {
	case "", "created_at desc", "created_at":
	case "created_at asc":
		f.Ascending = true
	default:
		return nil, problem.BadRequest("request.invalid_order_by", "order_by accepts created_at asc or created_at desc.",
			problem.FieldError{Field: "order_by", Code: "unsupported", Message: "created_at asc|desc"})
	}
	scope := "listOperations|" + f.Status + "|" + f.Kind + "|" + params.OrderBy.Value
	if params.PageToken.Set {
		var c cursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		f.After = c.After
	}
	ops, err := list(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	page := gen.OperationPage{Items: []gen.Operation{}}
	if len(ops) > size {
		ops = ops[:size]
		tok, _ := e.Pages.Encode(scope, cursor{After: ops[size-1].ID}) // a uuid always encodes
		page.NextPageToken = gen.NewOptString(tok)
	}
	for _, o := range ops {
		page.Items = append(page.Items, ToAPI(o))
	}
	return &page, nil
}

// CancelOperation implements POST /v1/operations/{id}:cancel.
func (e *Endpoints) CancelOperation(ctx context.Context, params gen.CancelOperationParams) (gen.CancelOperationRes, error) {
	tx, p, err := request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := get(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.Check(params.IfMatch, o.Version); err != nil {
		return nil, err
	}
	updated, err := cancel(ctx, tx, p, o)
	if err != nil {
		return nil, err
	}
	if e.Workflows != nil && o.WorkflowID != nil {
		if err := e.Workflows.Cancel(ctx, *o.WorkflowID); err != nil && e.Log != nil {
			e.Log.WarnContext(ctx, "operation cancelled in the database; the workflow engine did not take the cancellation", "operation", o.ID, "workflow", *o.WorkflowID, "error", err)
		}
	}
	if err := audit.Add(ctx, audit.Entry{Action: "platform.operation.cancel", ObjectType: "platform.operation", ObjectID: o.ID,
		Outcome: "succeeded", Detail: map[string]any{"kind": o.Kind, "from_status": o.Status}}); err != nil {
		return nil, err
	}
	return &gen.CancelOperationOKHeaders{ETag: gen.NewOptString(etag.Format(updated.Version)), Response: ToAPI(updated)}, nil
}

// ToAPI is the API representation of an operation, for contexts that answer
// 202 with the operation they started.
func ToAPI(o Operation) gen.Operation {
	out := gen.Operation{
		ID: o.ID, Kind: o.Kind, Status: o.Status, Version: o.Version,
		Progress:  gen.OperationProgress{Done: o.ProgressDone, Total: gen.NilInt64{Null: true}},
		Resource:  gen.NilString{Null: true},
		CreatedAt: o.CreatedAt, CreatedBy: o.CreatedBy, UpdatedAt: o.UpdatedAt, UpdatedBy: o.UpdatedBy,
	}
	if o.ProgressTotal != nil {
		out.Progress.Total.SetTo(*o.ProgressTotal)
	}
	if o.Resource != nil {
		out.Resource.SetTo(*o.Resource)
	}
	if o.Result != nil {
		var r gen.OperationResult
		if r.UnmarshalJSON(o.Result) == nil { // the result column holds an object written by the platform
			out.Result.SetTo(r)
		}
	}
	if o.Error != nil {
		var pr gen.Problem
		if pr.UnmarshalJSON(o.Error) == nil {
			out.Error.SetTo(pr)
		}
	}
	return out
}
