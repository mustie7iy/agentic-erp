package approvals

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the workflow part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	Decider
}

func ifMatch(header string) func(int32) error {
	return func(v int32) error { return etag.Check(header, v) }
}

func toAPI(r Request) gen.ApprovalRequest {
	out := gen.ApprovalRequest{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		ObjectType: r.ObjectType, ObjectID: r.ObjectID, Title: r.Title, RequesterID: r.RequesterID, Status: r.Status,
		CurrentStep: int32(r.CurrentStep), Steps: make([]gen.ApprovalStep, len(r.Steps)), Decisions: make([]gen.Decision, len(r.Decisions))}
	if r.Amount != nil {
		out.Amount.SetTo(gen.Money{Amount: money.Minor(*r.Amount).Plain(r.Exponent), Currency: *r.Currency})
	}
	if r.CompletedAt != nil {
		out.CompletedAt.SetTo(*r.CompletedAt)
	}
	out.Escalations.SetTo(int32(r.Escalations))
	if r.EscalatedAt != nil {
		out.EscalatedAt.SetTo(*r.EscalatedAt)
	}
	for i, s := range r.Steps {
		out.Steps[i] = gen.ApprovalStep{StepNo: int32(s.No), RoleID: s.RoleID, Required: int32(s.Required), Approvals: int32(s.Approvals)}
	}
	for i, d := range r.Decisions {
		dec := gen.Decision{ID: d.ID, StepNo: int32(d.StepNo), ActorID: d.ActorID, Decision: d.Decision, DecidedAt: d.DecidedAt}
		if d.Comment != nil {
			dec.Comment.SetTo(*d.Comment)
		}
		out.Decisions[i] = dec
	}
	return out
}

func ok(r Request) *gen.ApproveApprovalRequestOKHeaders {
	return &gen.ApproveApprovalRequestOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: toAPI(r)}
}

// GetApprovalRequest implements GET /v1/approval-requests/{id}.
func (e *Endpoints) GetApprovalRequest(ctx context.Context, params gen.GetApprovalRequestParams) (gen.GetApprovalRequestRes, error) {
	r, err := GetRequest(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.ApprovalRequestHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: toAPI(r)}, nil
}

var requestSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "r.created_at", Type: "timestamptz"},
}}

// ListApprovalRequests implements GET /v1/approval-requests.
func (e *Endpoints) ListApprovalRequests(ctx context.Context, params gen.ListApprovalRequestsParams) (gen.ListApprovalRequestsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var object *uuid.UUID
	if params.ObjectID.Set {
		object = &params.ObjectID.Value
	}
	scope := fmt.Sprintf("listApprovalRequests|%s|%s|%s", params.Status.Value, params.ObjectType.Value, params.ObjectID.Value)
	ks, err := e.Pages.Keyset(requestSort, scope, pagination.Request{OrderBy: params.OrderBy.Value, PageSize: params.PageSize.Value,
		PageSizeSet: params.PageSize.Set, PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set})
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(requestSelect+` WHERE ($1 = '' OR r.status = $1) AND ($2 = '' OR r.object_type = $2) AND ($3::uuid IS NULL OR r.object_id = $3)`,
		[]any{params.Status.Value, params.ObjectType.Value, object})
	rows, _ := tx.Query(ctx, q, args...)
	requests, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) { return scanRequest(r) })
	if err != nil {
		return nil, err
	}
	requests, next := pagination.Page(ks, requests, func(r Request) (string, uuid.UUID) {
		return r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), r.ID
	})
	page := gen.ApprovalRequestPage{Items: make([]gen.ApprovalRequest, len(requests))}
	for i, r := range requests {
		page.Items[i] = toAPI(r)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ApprovalRequestPageHeaders{Response: page}, nil
}

// ApproveApprovalRequest implements POST /v1/approval-requests/{id}:approve.
func (e *Endpoints) ApproveApprovalRequest(ctx context.Context, req gen.OptDecisionInput, params gen.ApproveApprovalRequestParams) (gen.ApproveApprovalRequestRes, error) {
	r, err := e.Approve(ctx, params.ID, ifMatch(params.IfMatch), req.Value.Comment.Value)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}

// RejectApprovalRequest implements POST /v1/approval-requests/{id}:reject.
func (e *Endpoints) RejectApprovalRequest(ctx context.Context, req *gen.CommentRequiredInput, params gen.RejectApprovalRequestParams) (gen.RejectApprovalRequestRes, error) {
	r, err := e.Reject(ctx, params.ID, ifMatch(params.IfMatch), req.Comment)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}

// CancelApprovalRequest implements POST /v1/approval-requests/{id}:cancel.
func (e *Endpoints) CancelApprovalRequest(ctx context.Context, req *gen.CommentRequiredInput, params gen.CancelApprovalRequestParams) (gen.CancelApprovalRequestRes, error) {
	r, err := e.Cancel(ctx, params.ID, ifMatch(params.IfMatch), req.Comment)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}
