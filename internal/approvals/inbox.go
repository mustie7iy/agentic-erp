package approvals

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// DecidePermission is approveApprovalRequest's x-permission, which Authorize
// checks, segregation of duties included, before Approve runs.
const DecidePermission = "workflow.request.decide"

// inbox is listApprovalRequests' decidable_by=me (ADR-0052 G1) as a
// filter: the pending requests the caller may approve now, as decide and
// Approve check them before the owning context's bar (Barrer, which
// unbarred applies to the page). Its placeholders follow the list's
// first three. on is decidable_by=me; none means nothing can be
// decidable: the caller is not a person, may not use DecidePermission,
// or holds no approval permission or no role.
type inbox struct {
	on, none bool
	where    string
	args     []any
}

// inboxOf reads what the caller may decide. by is the parameter's value;
// "" is no inbox, anything but "me" is refused.
func (d Decider) inboxOf(ctx context.Context, p reqctx.Principal, by string) (inbox, error) {
	switch by {
	case "":
		return inbox{}, nil
	case "me":
	default:
		return inbox{}, problem.BadRequest("request.invalid_parameter", "Parameter decidable_by is me or absent.",
			problem.FieldError{Field: "decidable_by", Code: "invalid", Message: "me"})
	}
	if p.Kind != reqctx.KindHuman {
		return inbox{on: true, none: true}, nil
	}
	usable, barred, err := authz.Effective(ctx)
	if err != nil {
		return inbox{}, err
	}
	if !slices.Contains(usable, DecidePermission) {
		return inbox{on: true, none: true}, nil
	}
	held := slices.Concat(usable, barred) // the object type's permission is required, not checked for SoD (decide)
	var types []string
	for _, objectType := range slices.Sorted(maps.Keys(ObjectPermissions)) {
		if slices.Contains(held, ObjectPermissions[objectType]) {
			types = append(types, objectType)
		}
	}
	roles, err := d.Roles.RolesHeld(ctx, p.ID)
	if err != nil {
		return inbox{}, err
	}
	if len(types) == 0 || len(roles) == 0 {
		return inbox{on: true, none: true}, nil
	}
	return inbox{on: true, where: ` AND r.status = 'pending' AND r.requester_id <> $4 AND r.object_type = ANY($5::text[])
		AND EXISTS (SELECT FROM workflow.approval_step s WHERE s.tenant_id = r.tenant_id AND s.request_id = r.id
			AND s.step_no = r.current_step AND s.role_id = ANY($6::uuid[]))
		AND NOT EXISTS (SELECT FROM workflow.decision d WHERE d.tenant_id = r.tenant_id AND d.request_id = r.id
			AND d.actor_id = $4 AND d.decision = 'approve')`, args: []any{p.ID, types, roles}}, nil
}

// unbarred leaves out of a page the requests whose owning context bars the
// caller from approving them (Barrer), asked as Approve asks it.
func (d Decider) unbarred(ctx context.Context, actor uuid.UUID, page []Request) ([]Request, error) {
	out := page[:0:0]
	for _, r := range page {
		if b, ok := d.Handlers[r.ObjectType].(Barrer); ok {
			barred, err := b.Barred(ctx, r.ObjectID, actor)
			if err != nil {
				return nil, fmt.Errorf("approvals: is %s barred from request %s: %w", actor, r.ID, err)
			}
			if barred {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}
