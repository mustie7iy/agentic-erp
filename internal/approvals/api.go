package approvals

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the workflow part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	Decider
}

func toAPI(r Request) gen.ApprovalRequest {
	out := gen.ApprovalRequest{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		ObjectType: r.ObjectType, ObjectID: r.ObjectID, Title: r.Title, RequesterID: r.RequesterID, Status: r.Status,
		CurrentStep: int32(r.CurrentStep), Steps: make([]gen.ApprovalStep, len(r.Steps)), Decisions: make([]gen.Decision, len(r.Decisions))}
	if r.Amount != nil {
		out.Amount.SetTo(apitypes.Money(*r.Amount, *r.Currency, r.Exponent))
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

// ListApprovalRequests implements GET /v1/approval-requests, and with
// decidable_by=me the caller's inbox (ADR-0052 G1).
func (e *Endpoints) ListApprovalRequests(ctx context.Context, params gen.ListApprovalRequestsParams) (gen.ListApprovalRequestsRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var object *uuid.UUID
	if params.ObjectID.Set {
		object = &params.ObjectID.Value
	}
	scope := fmt.Sprintf("listApprovalRequests|%s|%s|%s|%s", params.Status.Value, params.ObjectType.Value, params.ObjectID.Value, params.DecidableBy.Value)
	ks, err := e.Pages.Keyset(requestSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	in, err := e.inboxOf(ctx, p, params.DecidableBy.Value)
	if err != nil {
		return nil, err
	}
	if in.none {
		return &gen.ApprovalRequestPageHeaders{Response: gen.ApprovalRequestPage{Items: []gen.ApprovalRequest{}}}, nil
	}
	q, args := ks.SQL(requestSelect+` WHERE ($1 = '' OR r.status = $1) AND ($2 = '' OR r.object_type = $2) AND ($3::uuid IS NULL OR r.object_id = $3)`+in.where,
		append([]any{params.Status.Value, params.ObjectType.Value, object}, in.args...))
	rows, _ := tx.Query(ctx, q, args...)
	requests, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) { return scanRequest(r) })
	if err != nil {
		return nil, fmt.Errorf("approvals: list approval requests: %w", err)
	}
	// The page and its token first: a request left out below must not
	// pull the next page's first one in, nor move the token.
	page, next := pagination.Page(ks, requests, func(r Request) (string, uuid.UUID) {
		return r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), r.ID
	})
	if in.on {
		if page, err = e.unbarred(ctx, p.ID, page); err != nil {
			return nil, err
		}
	}
	out := gen.ApprovalRequestPage{Items: make([]gen.ApprovalRequest, len(page))}
	for i, r := range page {
		out.Items[i] = toAPI(r)
	}
	if next != "" {
		out.NextPageToken.SetTo(next)
	}
	return &gen.ApprovalRequestPageHeaders{Response: out}, nil
}

// ApproveApprovalRequest implements POST /v1/approval-requests/{id}:approve.
func (e *Endpoints) ApproveApprovalRequest(ctx context.Context, req gen.OptDecisionInput, params gen.ApproveApprovalRequestParams) (gen.ApproveApprovalRequestRes, error) {
	r, err := e.Approve(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Value.Comment.Value)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}

// RejectApprovalRequest implements POST /v1/approval-requests/{id}:reject.
func (e *Endpoints) RejectApprovalRequest(ctx context.Context, req *gen.CommentRequiredInput, params gen.RejectApprovalRequestParams) (gen.RejectApprovalRequestRes, error) {
	r, err := e.Reject(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Comment)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}

// CancelApprovalRequest implements POST /v1/approval-requests/{id}:cancel.
func (e *Endpoints) CancelApprovalRequest(ctx context.Context, req *gen.CommentRequiredInput, params gen.CancelApprovalRequestParams) (gen.CancelApprovalRequestRes, error) {
	r, err := e.Cancel(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Comment)
	if err != nil {
		return nil, err
	}
	return ok(r), nil
}
