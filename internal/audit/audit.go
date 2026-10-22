// Package audit is the audit context (03-data-model §3.12, ADR-0011): it
// reads the tenant's tamper-evident audit trail and verifies its hash
// chain. Entries are written by every other context through the
// platform's audit writer (internal/platform/audit) in the transaction of
// the change they record; this context never writes them.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the audit part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	// Anchors is the object storage the anchors were written to; nil checks
	// anchors against the chain only.
	Anchors blobstore.Store
}

// Event is a row of audit.audit_event without its hashes.
type Event struct {
	Seq        int64
	OccurredAt time.Time
	ActorID    uuid.UUID
	ActorKind  string
	Action     string
	ObjectType string
	ObjectID   *uuid.UUID
	Outcome    string
	Detail     map[string]json.RawMessage
	TraceID    *string
	// ClientAddress is where the request came from (ADR-0011 as amended
	// by T73c); nil for work no request made, and after an erasure.
	ClientAddress *netip.Addr
}

type cursor struct {
	Seq int64 `json:"s"`
}

// ListAuditEvents implements GET /v1/audit-events, newest first unless
// order_by asks otherwise. Sequence numbers follow the time of the events,
// so the trail pages by sequence number.
func (e *Endpoints) ListAuditEvents(ctx context.Context, params gen.ListAuditEventsParams) (gen.ListAuditEventsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	cmp, dir, err := auditOrder(params.OrderBy.Value)
	if err != nil {
		return nil, err
	}
	var objectID *uuid.UUID
	if id, ok := params.ObjectID.Get(); ok {
		objectID = &id
	}
	var from, to *time.Time
	if t, ok := params.OccurredFrom.Get(); ok {
		from = &t
	}
	if t, ok := params.OccurredTo.Get(); ok {
		to = &t
	}
	scope := strings.Join([]string{"listAuditEvents", params.ObjectType.Value, params.ObjectID.Value.String(), params.OccurredFrom.Value.String(),
		params.OccurredTo.Value.String(), dir}, "|")
	var after *int64
	if params.PageToken.Set {
		var c cursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		after = &c.Seq
	}
	// A masked reader's addresses never leave the database (people only,
	// product owner 2026-10-22).
	masked := reqctx.Masked(ctx)
	rows, _ := tx.Query(ctx, `SELECT seq, occurred_at, actor_id, actor_kind, action, object_type, object_id, outcome, detail, trace_id,
			CASE WHEN $7 THEN NULL ELSE client_address END
		FROM audit.audit_event WHERE ($1 = '' OR object_type = $1) AND ($2::uuid IS NULL OR object_id = $2)
			AND ($3::timestamptz IS NULL OR occurred_at >= $3) AND ($4::timestamptz IS NULL OR occurred_at <= $4)
			AND ($5::bigint IS NULL OR seq `+cmp+` $5)
		ORDER BY seq `+dir+` LIMIT $6`, pagination.PlanPerCall, params.ObjectType.Value, objectID, from, to, after, size+1, masked)
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var v Event
		err := r.Scan(&v.Seq, &v.OccurredAt, &v.ActorID, &v.ActorKind, &v.Action, &v.ObjectType, &v.ObjectID, &v.Outcome, &v.Detail, &v.TraceID, &v.ClientAddress)
		return v, errs.Wrapf(err, "scan audit event")
	})
	if err != nil {
		return nil, fmt.Errorf("audit: list audit events: %w", err)
	}
	page := gen.AuditEventPage{Items: []gen.AuditEvent{}}
	if len(events) > size {
		events = events[:size]
		tok, _ := e.Pages.Encode(scope, cursor{Seq: events[size-1].Seq}) // a number always encodes
		page.NextPageToken.SetTo(tok)
	}
	for _, v := range events {
		page.Items = append(page.Items, auditEventItem(v, masked))
	}
	return &gen.AuditEventPageHeaders{Response: page}, nil
}

// auditOrder is the comparison and direction that order_by asks for:
// newest first by default.
func auditOrder(orderBy string) (cmp, dir string, err error) {
	cmp, dir = "<", "DESC"
	switch strings.Join(strings.Fields(orderBy), " ") {
	case "", "occurred_at desc":
	case "occurred_at", "occurred_at asc":
		cmp, dir = ">", "ASC"
	default:
		return "", "", problem.BadRequest("request.invalid_order_by", "order_by accepts occurred_at asc or occurred_at desc.",
			problem.FieldError{Field: "order_by", Code: "unsupported", Message: "occurred_at asc|desc"})
	}
	return cmp, dir, nil
}

// auditEventItem is an event as the API answers it. Masked (any reader but
// a person) omits the client address: it is personal data only people
// holding audit.event.read see (product owner 2026-10-22). Omitted, not
// null, so it is not mistaken for work no request made.
func auditEventItem(v Event, masked bool) gen.AuditEvent {
	item := gen.AuditEvent{Seq: v.Seq, OccurredAt: v.OccurredAt, ActorID: v.ActorID, ActorKind: v.ActorKind, Action: v.Action,
		ObjectType: v.ObjectType, Outcome: v.Outcome, Detail: gen.AuditEventDetail{}}
	if v.ObjectID != nil {
		item.ObjectID.SetTo(*v.ObjectID)
	}
	if v.TraceID != nil {
		item.TraceID.SetTo(*v.TraceID)
	}
	switch {
	case masked:
	case v.ClientAddress != nil:
		item.ClientAddress.SetTo(v.ClientAddress.String())
	default:
		item.ClientAddress.SetToNull()
	}
	for k, raw := range v.Detail {
		item.Detail[k] = jx.Raw(raw)
	}
	return item
}

// VerifyChain recomputes the tenant's hash chain from from (default the
// last anchored head, or the first event while no anchor exists) to to
// (default the head), as an operation that completes within the request,
// and compares the anchors in the range with the chain and, with a store,
// with the anchor objects.
func VerifyChain(ctx context.Context, store blobstore.Store, from, to *int64) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	var head int64
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT seq FROM audit.chain_head WHERE tenant_id = $1), 0)`, p.TenantID).Scan(&head); err != nil {
		return operations.Operation{}, fmt.Errorf("audit: read chain head of tenant %s: %w", p.TenantID, err)
	}
	first, last := int64(1), int64(0)
	if from != nil {
		first = *from
	} else if anchored, err := lastAnchor(ctx, tx, p.TenantID); err != nil {
		return operations.Operation{}, err
	} else if anchored > 0 {
		first = anchored
	}
	if to != nil {
		last = *to
	}
	if first > head+1 || last > head || (last != 0 && last < first) {
		return operations.Operation{}, problem.Unprocessable("audit.invalid_range",
			"The range must lie within the chain, which ends at sequence number "+jsonNumber(head)+".",
			problem.FieldError{Field: "from_seq", Code: "out_of_range", Message: "from_seq <= to_seq <= the head"})
	}
	op, err := operations.Start(ctx, "audit.verify", map[string]any{"from_seq": first, "to_seq": to})
	if err != nil {
		return operations.Operation{}, err
	}
	res, err := trail.VerifyRange(ctx, tx, p.TenantID, first, last)
	if err != nil {
		return operations.Operation{}, err
	}
	anchors, brokenAnchor, err := checkAnchors(ctx, tx, store, p.TenantID, first, last)
	if err != nil {
		return operations.Operation{}, err
	}
	if brokenAnchor != 0 && (res.BrokenAt == 0 || brokenAnchor < res.BrokenAt) {
		res.BrokenAt = brokenAnchor
	}
	result := map[string]any{"verified": res.BrokenAt == 0, "last_seq": res.LastSeq, "checked": res.Checked, "anchors_checked": anchors}
	if res.BrokenAt != 0 {
		result["first_broken_seq"] = res.BrokenAt
	}
	done, err := operations.Succeed(ctx, op.ID, "/v1/operations/"+op.ID.String(), result)
	if err != nil {
		return operations.Operation{}, err
	}
	return done, trail.Add(ctx, trail.Entry{Action: "audit.chain.verify", ObjectType: "platform.operation", ObjectID: op.ID, Outcome: "succeeded",
		Detail: result})
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n) // an int64 always encodes
	return string(b)
}

// VerifyAuditChain implements POST /v1/audit-events:verify.
func (e *Endpoints) VerifyAuditChain(ctx context.Context, req *gen.AuditVerify, _ gen.VerifyAuditChainParams) (gen.VerifyAuditChainRes, error) {
	var from, to *int64
	if v, ok := req.FromSeq.Get(); ok {
		from = &v
	}
	if v, ok := req.ToSeq.Get(); ok {
		to = &v
	}
	op, err := VerifyChain(ctx, e.Anchors, from, to)
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}
