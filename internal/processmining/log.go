package processmining

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// maxPeriodDays is the most days a log's period ends after its first: the
// longest fiscal year, 53 weeks.
const maxPeriodDays = 370

// PurchaseToPay is the first process with an event log.
const PurchaseToPay = "purchase_to_pay"

// Kinds of case.
const (
	CasePurchaseOrder   = "purchase_order"
	CaseSupplierInvoice = "supplier_invoice"
)

// Object types of the trail a purchase-to-pay case reads.
const (
	requisitionType = "procurement.requisition"
	orderType       = "procurement.purchase_order"
	movementType    = "procurement.goods_movement"
	invoiceType     = "payables.supplier_invoice"
	runType         = "payments.payment_run"
)

// activities are purchase to pay's activities by the audit action each
// comes from (ADR-0080 §3).
var activities = map[string]string{
	"procurement.requisition.create":           "Create requisition",
	"procurement.requisition.pending_approval": "Submit requisition",
	"procurement.requisition.approved":         "Approve requisition",
	"procurement.requisition.rejected":         "Reject requisition",
	"procurement.purchase_order.create":        "Create purchase order",
	"procurement.purchase_order.submit":        "Submit purchase order",
	"procurement.purchase_order.release":       "Release purchase order",
	"procurement.purchase_order.close":         "Close purchase order",
	"procurement.purchase_order.cancel":        "Cancel purchase order",
	"procurement.goods_receipt.post":           "Receive goods",
	"procurement.goods_return.post":            "Return goods",
	"procurement.goods_receipt.reverse":        "Reverse goods receipt",
	"payables.supplier_invoice.create":         "Enter invoice",
	"payables.supplier_invoice.post":           "Post invoice",
	"payables.supplier_invoice.hold":           "Hold invoice",
	"payables.supplier_invoice.release":        "Release invoice",
	"payables.supplier_invoice.reverse":        "Reverse invoice",
	"payments.payment_run.complete":            "Pay invoice",
}

// actions are the activities' audit actions, in order.
func actions() []string {
	out := make([]string, 0, len(activities))
	for a := range activities {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// actorKind is the kind of actor an event log gives for an audit actor
// kind: a person, an agent, or the system (a service or the platform's
// own work).
func actorKind(kind string) string {
	switch kind {
	case "human":
		return "person"
	case "agent":
		return "agent"
	default:
		return "system"
	}
}

// Event is an activity of a case.
type Event struct {
	Activity, ActorKind string
	OccurredAt          time.Time
	Object              audit.ObjectRef
	DocumentNumber      string
	Seq                 int64
}

// Case is a case of the log with its events.
type Case struct {
	ID             uuid.UUID
	Kind           string
	DocumentNumber string
	DocumentDate   time.Time
	Events         []Event
	objects        []audit.ObjectRef         // the trail's objects of its events
	refs           []procurement.DocumentRef // an order's requisition and movements
	own            *payables.LoggedInvoice   // a document case's document
}

// Log is a page of an event log.
type Log struct {
	Process       string
	LegalEntity   uuid.UUID
	CompanyCode   string
	From, To      time.Time
	CaseCount     int
	Cases         []Case
	NextPageToken string
}

// Query asks for a page of a log.
type Query struct {
	Process     string
	LegalEntity uuid.UUID
	From, To    time.Time
	Size        int32
	SizeSet     bool
	Token       string
}

// cursor is the last case of a page: its date and id.
type cursor struct {
	Date time.Time `json:"d"`
	ID   uuid.UUID `json:"i"`
}

// Read reads a page of the company's log of the process for the period.
func (e *Endpoints) Read(ctx context.Context, q Query) (Log, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return Log{}, err
	}
	if q.Process != PurchaseToPay {
		return Log{}, problem.Unprocessable("request.validation_failed", "The request is not valid: the first process is purchase_to_pay.",
			problem.FieldError{Field: "process", Code: "invalid", Message: PurchaseToPay})
	}
	if q.To.Before(q.From) || q.To.Sub(q.From) > maxPeriodDays*24*time.Hour {
		return Log{}, problem.Unprocessable("request.validation_failed", "The request is not valid: the period's last day is not before its first and at most "+
			"370 days after it.", problem.FieldError{Field: "to", Code: "out_of_range", Message: "from <= to <= from + 370 days"})
	}
	size, err := pagination.Size(q.Size, q.SizeSet)
	if err != nil {
		return Log{}, err
	}
	l := Log{Process: q.Process, LegalEntity: q.LegalEntity, From: q.From, To: q.To}
	if l.CompanyCode, err = e.company(ctx, q.LegalEntity); err != nil {
		return Log{}, err
	}
	scope := fmt.Sprintf("getProcessEventLog|%s|%s|%s|%s", q.Process, q.LegalEntity, q.From.Format(time.DateOnly), q.To.Format(time.DateOnly))
	var after *cursor
	if q.Token != "" {
		after = &cursor{}
		if err := e.Pages.Decode(scope, q.Token, after); err != nil {
			return Log{}, err
		}
	}
	if l.CaseCount, err = e.count(ctx, q); err != nil {
		return Log{}, err
	}
	if l.Cases, err = e.cases(ctx, q, after, size+1); err != nil {
		return Log{}, err
	}
	if len(l.Cases) > size {
		l.Cases = l.Cases[:size]
		last := l.Cases[size-1]
		l.NextPageToken, _ = e.Pages.Encode(scope, cursor{Date: last.DocumentDate, ID: last.ID}) // a date and an id always encode
	}
	return l, e.events(ctx, l.Cases)
}

// company is the code of the company, which the caller must be able to
// act in; another company's is not found, so ids cannot be probed.
func (e *Endpoints) company(ctx context.Context, legalEntity uuid.UUID) (string, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return "", fmt.Errorf("processmining: company access for an event log: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return "", fmt.Errorf("processmining: company %s of an event log: %w", legalEntity, err)
	}
	if !ok || !access.Allows(legalEntity) {
		return "", problem.NotFoundOf("legal entity")
	}
	return le.Code, nil
}

// count is how many cases the whole log has.
func (e *Endpoints) count(ctx context.Context, q Query) (int, error) {
	orders, err := e.Orders.CountOrders(ctx, q.LegalEntity, q.From, q.To)
	if err != nil {
		return 0, fmt.Errorf("processmining: %w", err)
	}
	invoices, err := e.Invoices.CountWithoutOrder(ctx, q.LegalEntity, q.From, q.To)
	if err != nil {
		return 0, fmt.Errorf("processmining: %w", err)
	}
	return orders + invoices, nil
}

// cases are up to limit cases after the cursor, by date and id: the
// orders and the documents on no order, merged.
func (e *Endpoints) cases(ctx context.Context, q Query, after *cursor, limit int) ([]Case, error) {
	var date *time.Time
	var id uuid.UUID
	if after != nil {
		date, id = &after.Date, after.ID
	}
	orders, err := e.Orders.Orders(ctx, q.LegalEntity, q.From, q.To, date, id, limit)
	if err != nil {
		return nil, fmt.Errorf("processmining: %w", err)
	}
	alone, err := e.Invoices.WithoutOrder(ctx, q.LegalEntity, q.From, q.To, date, id, limit)
	if err != nil {
		return nil, fmt.Errorf("processmining: %w", err)
	}
	out := make([]Case, 0, len(orders)+len(alone))
	for _, o := range orders {
		out = append(out, orderCase(o))
	}
	for _, v := range alone {
		out = append(out, Case{ID: v.ID, Kind: CaseSupplierInvoice, DocumentNumber: v.DocumentNumber, DocumentDate: v.InvoiceDate, own: &v})
	}
	slices.SortFunc(out, func(a, b Case) int {
		return cmp.Or(a.DocumentDate.Compare(b.DocumentDate), slices.Compare(a.ID[:], b.ID[:]))
	})
	return out[:min(len(out), limit)], nil
}

// orderCase is an order as a case, with its requisition and movements
// among its objects.
func orderCase(o procurement.LoggedOrder) Case {
	c := Case{ID: o.ID, Kind: CasePurchaseOrder, DocumentNumber: o.DocumentNumber, DocumentDate: o.OrderDate,
		objects: []audit.ObjectRef{{Type: orderType, ID: o.ID}}}
	if o.Requisition != nil {
		c.objects = append(c.objects, audit.ObjectRef{Type: requisitionType, ID: o.Requisition.ID})
		c.refs = append(c.refs, *o.Requisition)
	}
	for _, m := range o.Movements {
		c.objects = append(c.objects, audit.ObjectRef{Type: movementType, ID: m.ID})
		c.refs = append(c.refs, m)
	}
	return c
}
