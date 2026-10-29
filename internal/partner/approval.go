package partner

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// RequestApproval opens the approval request of a pending change and links
// it; a change the configuration needs no approval for is applied at once.
func RequestApproval(ctx context.Context, opener Opener, schemas extensions.Schemas, c Change) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	var number string
	if err := tx.QueryRow(ctx, `SELECT number FROM partner.business_partner WHERE id = $1`, c.PartnerID).Scan(&number); err != nil {
		return Change{}, fmt.Errorf("partner: read the number of business partner %s: %w", c.PartnerID, err)
	}
	id, needed, err := opener.Open(ctx, approvals.NewRequest{ObjectType: approvals.PartnerChange, ObjectID: c.ID,
		Title: "Partner change " + c.Number + " (" + c.Kind + ") for " + number})
	if err != nil {
		return Change{}, err
	}
	if !needed {
		return ApplyChange(ctx, schemas, c.ID)
	}
	linked, err := scanChange(tx.QueryRow(ctx, `UPDATE partner.partner_change SET workflow_request_id = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+changeColumns, c.ID, id, p.ID))
	if err != nil {
		return linked, fmt.Errorf("partner: link partner change %s to approval request %s: %w", c.ID, id, err)
	}
	return linked, nil
}

// Approvals moves partner changes on when their approval requests complete.
type Approvals struct {
	Schemas extensions.Schemas // mask the custom fields of the events (ADR-0046)
}

// Approved applies the change.
func (a Approvals) Approved(ctx context.Context, change, _ uuid.UUID) error {
	_, err := ApplyChange(ctx, a.Schemas, change)
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

// Resources is where the API reads each partner change (approvals.Locator).
func (Approvals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/partner-changes/%s", ids), nil
}
