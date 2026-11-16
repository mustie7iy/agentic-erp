// Package processmining is the process mining context (ADR-0080; T103a):
// event logs of business processes, computed on read from the documents'
// links and the audit trail. The first process is purchase to pay: a
// case is a purchase order, or a supplier document on no order, and its
// events are the activities on the order, its requisition, its goods
// movements, the documents on it and the payment runs that paid them, as
// the audit trail recorded them, each with its time and the kind of its
// actor, never who.
//
// The context is a leaf (ADR-0080 §1): it reads tenancy's legal
// entities, procurement's orders, payables' documents, payments' runs and
// the audit trail through ports the composition root wires, by the types
// their published APIs give; it stores nothing and no context imports
// it.
package processmining

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LegalEntities reads the tenant's legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Orders reads a company's purchase orders of a period with their
// requisitions and goods movements (procurement.ProcessLog).
type Orders interface {
	Orders(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, afterDate *time.Time, afterID uuid.UUID, limit int) ([]procurement.LoggedOrder, error)
	CountOrders(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (int, error)
}

// Invoices reads the supplier documents on orders and a company's
// documents on no order (payables.ProcessLog).
type Invoices interface {
	OfOrders(ctx context.Context, orders []uuid.UUID) ([]payables.LoggedInvoice, error)
	WithoutOrder(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, afterDate *time.Time, afterID uuid.UUID, limit int) ([]payables.LoggedInvoice, error)
	CountWithoutOrder(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (int, error)
}

// Payments reads the released runs that paid journal entries
// (payments.ProcessLog).
type Payments interface {
	Paying(ctx context.Context, years []int16, entries []uuid.UUID) ([]payments.LoggedPayment, error)
}

// Trail reads the audit events about objects (audit.Trail).
type Trail interface {
	About(ctx context.Context, objects []audit.ObjectRef, actions []string) ([]audit.ObjectEvent, error)
}

// Endpoints implements the process mining part of the generated API.
type Endpoints struct {
	LegalEntities LegalEntities
	Orders        Orders
	Invoices      Invoices
	Payments      Payments
	Trail         Trail
	Pages         *pagination.Codec
}
