// Package procurement is the procurement context (03-data-model §3.8):
// purchase requisitions approved through the workflow context, purchasing
// contracts (renewed by a successor, alerted before they expire), purchase
// orders and goods movements (insert-only: receipts 101 and their reversals
// 102, limited by the ordered quantity plus the over-delivery tolerance;
// returns 122 and their reversals 123, limited by what was received).
// Orders are valued at release, directly or through GR/IR, each item at
// the valuation price fixed then; movements on GR/IR orders post WE entries
// through the ledger's Postings (T33). An order without an approved
// requisition is released by the release strategy, and a released order
// changes only through order changes, its versions (ADR-0034). A released
// or closed order's document for its supplier is rendered from the stored
// order as plain text on each read (ADR-0054).
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
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
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
	// PaymentTerm and OrderAddress fill the order document (ADR-0054).
	PaymentTerm(ctx context.Context, code string) (partner.PaymentTerm, bool, error)
	OrderAddress(ctx context.Context, partnerID uuid.UUID) (partner.Address, bool, error)
}

// SupplierNames reads suppliers' numbers and names (partner context), for
// the contract expiry job's events.
type SupplierNames interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// Accounts is what procurement asks the ledger about accounts, the account
// determination and tax codes.
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
	CheckAssignments(ctx context.Context, legalEntity uuid.UUID, date time.Time, items []ledger.ItemAssignment, defaults bool) error
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
	Schemas       extensions.Schemas    // the tenant's custom field schemas (configuration context)
	References    extensions.References // what custom fields refer to (partner context)
	Rules         rules.Source          // the tenant's validation rules (configuration context)
	Approvals     Opener
	Invoices      Invoices
	Postings      Postings
	// Now dates requisitions in their company's time zone (T72n5); nil
	// means the wall clock. Tests fix it.
	Now func() time.Time
}

// checkRules evaluates the tenant's validation rules on a purchase order's
// header as a write leaves it (ADR-0046); the write's audit names the
// object. Item lines are not inputs in Phase 2.
func (e *Endpoints) checkRules(ctx context.Context, action, objectType string, id uuid.UUID, o PurchaseOrder) error {
	return rules.Writes{Entity: "purchase_order", Source: e.Rules}.Check(ctx,
		rules.Write{Action: action, ObjectType: objectType, ID: id, Row: ruleRow(o), Extensions: o.Extensions})
}

// ruleRow is a purchase order's header as its rules read it.
func ruleRow(o PurchaseOrder) map[string]any {
	var term *string
	if o.PaymentTermCode != "" {
		term = &o.PaymentTermCode
	}
	return map[string]any{"legal_entity_id": o.LegalEntityID.String(), "purchasing_org_id": rules.ID(o.PurchasingOrgID),
		"supplier_id": o.SupplierID.String(), "currency": o.Currency, "order_date": o.OrderDate.Format(time.DateOnly),
		"payment_term_code": rules.Text(term), "requisition_id": rules.ID(o.RequisitionID), "contract_id": rules.ID(o.ContractID)}
}

// RuleSamples reads the recent purchase orders on which a simulation
// replays proposed rules (rules.Sampler, ADR-0046): those changed since
// the time given, newest first.
type RuleSamples struct{}

// Sample implements rules.Sampler.
func (RuleSamples) Sample(ctx context.Context, since time.Time, limit int) ([]rules.Sample, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, orderSelect+` WHERE o.updated_at >= $1 ORDER BY o.updated_at DESC, o.id LIMIT $2`, since, limit)
	os, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PurchaseOrder, error) { return scanOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("procurement: sample purchase orders for rules: %w", err)
	}
	out := make([]rules.Sample, len(os))
	for i, o := range os {
		historical := o.Status == Closed || o.Status == Cancelled // no update or change request reaches it
		out[i] = rules.Sample{ID: o.ID, Reference: o.DocumentNumber, Row: ruleRow(o), Extensions: o.Extensions, Historical: historical}
	}
	return out, nil
}

// extensions checks a purchase order's custom fields (ADR-0046).
func (e *Endpoints) extensions() extensions.Writes {
	return extensions.Writes{Entity: "purchase_order", Schemas: e.Schemas, References: e.References}
}

// Postings posts the WE entries of goods movements on orders valued
// through GR/IR (ledger context, T33).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
	// GRIROrder reads an order's GR/IR positions and the price difference
	// account a GR/IR clearing proposes from (T72e).
	GRIROrder(ctx context.Context, legalEntity, order uuid.UUID) (ledger.GRIROrder, error)
	// AssetClearingAccounts are those of the accounts that are asset
	// clearing accounts of the legal entity's company: an order item on
	// one is an asset's, and its GR/IR clearing clears against it
	// (ADR-0057 §6; product owner 2026-11-10).
	AssetClearingAccounts(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) (map[uuid.UUID]ledger.AccountRef, error)
}

// Invoices tells whether supplier invoices refer to a purchase order, and
// what posted documents invoiced of its items (payables context).
type Invoices interface {
	HasInvoices(ctx context.Context, purchaseOrder uuid.UUID) (bool, error)
	// Invoiced is the quantity of each of the order's items that posted
	// documents invoiced net of credit memos, with what the source system
	// invoiced before migration, as plain decimals; an item nothing was
	// invoiced of may be missing. A receipt's reversal reads it under the
	// order items' locks, which a document's posting and reversal take too.
	Invoiced(ctx context.Context, purchaseOrder uuid.UUID) (map[int32]string, error)
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
	if err := h.Endpoints.checkAssignments(ctx, o.LegalEntityID, o.OrderDate, assignments(o.Items, 0)); err != nil {
		return err
	}
	value, err := documentTotal(o.Items)
	if err != nil {
		return err
	}
	_, err = h.Endpoints.release(ctx, tx, p, o, value, "")
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

// Resources is where the API reads each requisition (approvals.Locator).
func (Approvals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/requisitions/%s", ids), nil
}

// Resources is where the API reads each purchase order (approvals.Locator).
func (OrderReleases) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/purchase-orders/%s", ids), nil
}
