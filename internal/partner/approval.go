package partner

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// RequestApproval opens the approval request of a pending change and links
// it; a change the configuration needs no approval for is applied at once.
func RequestApproval(ctx context.Context, opener Opener, c Change) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	var number string
	if err := tx.QueryRow(ctx, `SELECT number FROM partner.business_partner WHERE id = $1`, c.PartnerID).Scan(&number); err != nil {
		return Change{}, err
	}
	id, needed, err := opener.Open(ctx, approvals.NewRequest{ObjectType: approvals.PartnerChange, ObjectID: c.ID,
		Title: "Partner change " + c.Number + " (" + c.Kind + ") for " + number})
	if err != nil {
		return Change{}, err
	}
	if !needed {
		return ApplyChange(ctx, c.ID)
	}
	return scanChange(tx.QueryRow(ctx, `UPDATE partner.partner_change SET workflow_request_id = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+changeColumns, c.ID, id, p.ID))
}

// Approvals moves partner changes on when their approval requests complete.
type Approvals struct{}

// Approved applies the change.
func (Approvals) Approved(ctx context.Context, change, _ uuid.UUID) error {
	_, err := ApplyChange(ctx, change)
	return err
}

// Rejected rejects the change.
func (Approvals) Rejected(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := RejectChange(ctx, change)
	return err
}

// Cancelled cancels the change: its requester withdrew it.
func (Approvals) Cancelled(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := CancelChange(ctx, change)
	return err
}
