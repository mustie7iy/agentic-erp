// Package procurement is the procurement context (03-data-model §3.8):
// purchase requisitions approved through the workflow context, purchasing
// contracts, purchase orders and goods receipts (insert-only movements 101
// and 102, limited by the ordered quantity plus the over-delivery
// tolerance). Orders are valued at release, directly or through GR/IR;
// movements on GR/IR orders post WE entries through the ledger's Postings
// (T33, 03-data-model §3.8).
package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LegalEntities is what procurement reads about legal entities and org
// units (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	OrgUnit(ctx context.Context, id uuid.UUID) (tenancy.Unit, bool, error)
}

// Suppliers is what procurement reads about suppliers (partner context).
type Suppliers interface {
	Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Supplier, bool, error)
	PaymentTermExists(ctx context.Context, code string) (bool, error)
}

// Accounts is what procurement asks the ledger about accounts, the account
// determination and tax codes.
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// Endpoints implements the procurement part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Suppliers     Suppliers
	Accounts      Accounts
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Approvals     Opener
	Invoices      Invoices
	Postings      Postings
}

// Postings posts the WE entries of goods movements on orders valued
// through GR/IR (ledger context, T33).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
}

// Invoices tells whether supplier invoices refer to a purchase order
// (payables context).
type Invoices interface {
	HasInvoices(ctx context.Context, purchaseOrder uuid.UUID) (bool, error)
}

// Approvals moves requisitions on when their approval requests complete.
type Approvals struct{}

// Approved approves the requisition.
func (Approvals) Approved(ctx context.Context, requisition, _ uuid.UUID) error {
	return decide(ctx, requisition, Approved, "requisition_approved")
}

// Rejected rejects the requisition.
func (Approvals) Rejected(ctx context.Context, requisition, _ uuid.UUID, _ string) error {
	return decide(ctx, requisition, Rejected, "requisition_rejected")
}

// Cancelled returns the requisition to draft: its requester withdrew the
// approval request to change it.
func (Approvals) Cancelled(ctx context.Context, requisition, _ uuid.UUID, _ string) error {
	return decide(ctx, requisition, Draft, "")
}

// OrderReleases moves a purchase order on when its purchase_order approval
// request completes (approvals.Handler, T47d), in the deciding
// transaction. Each acts only while the order waits for that request, so
// a request the order no longer names changes nothing and fails the
// decision. Endpoints is procurement as the API wires it.
type OrderReleases struct {
	Endpoints *Endpoints
}

// Approved releases the order, checked again as a release is: a supplier
// blocked or a contract used up meanwhile refuses the decision, and the
// request stays pending.
func (h OrderReleases) Approved(ctx context.Context, order, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	o, err := waitingOrder(ctx, tx, order, request)
	if err != nil {
		return err
	}
	if err := h.Endpoints.supplier(ctx, o.SupplierID, o.LegalEntityID); err != nil {
		return err
	}
	value, err := documentTotal(o.Items)
	if err != nil {
		return err
	}
	_, err = h.Endpoints.release(ctx, tx, p, o, value, false)
	return err
}

// Rejected returns the order to draft, to be changed or cancelled.
func (OrderReleases) Rejected(ctx context.Context, order, request uuid.UUID, comment string) error {
	return endRelease(ctx, order, request, "reject_release", comment)
}

// Cancelled returns the order to draft: its requester withdrew the
// request.
func (OrderReleases) Cancelled(ctx context.Context, order, request uuid.UUID, comment string) error {
	return endRelease(ctx, order, request, "withdraw_release", comment)
}

func endRelease(ctx context.Context, order, request uuid.UUID, action, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if _, err := waitingOrder(ctx, tx, order, request); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET status = 'draft', release_request_id = NULL, version = version + 1, updated_by = $3
		WHERE id = $1 AND release_request_id = $2`, order, request, p.ID); err != nil {
		return fmt.Errorf("procurement: return purchase order %s to draft: %w", order, err)
	}
	return audit.Record(ctx, "procurement.purchase_order."+action, "procurement.purchase_order", order,
		map[string]any{"release_request_id": request, "comment": comment})
}

// waitingOrder locks the order and returns it if it waits for the
// request. The decider may not see the order's company: the request is
// what authorises the change.
func waitingOrder(ctx context.Context, tx pgx.Tx, order, request uuid.UUID) (PurchaseOrder, error) {
	o, err := readOrder(ctx, tx, order, true)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if o.Status != PendingApproval || o.ReleaseRequestID == nil || *o.ReleaseRequestID != request {
		return PurchaseOrder{}, fmt.Errorf("procurement: purchase order %s (%s) does not wait for request %s", order, o.Status, request)
	}
	return o, nil
}
