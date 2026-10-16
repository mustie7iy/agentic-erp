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
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
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
