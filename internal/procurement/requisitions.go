package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Item is a live item of a requisition, contract or purchase order.
// Quantities are decimal strings; prices are minor units of the document
// currency per PriceUnit.
type Item struct {
	No             int32       `json:"item_no"`
	MaterialID     *uuid.UUID  `json:"material_id"`
	MaterialKey    string      `json:"material_key"`
	Description    string      `json:"description"`
	Quantity       string      `json:"quantity"`
	Unit           string      `json:"unit"`
	UnitPrice      money.Minor `json:"unit_price"`
	PriceUnit      int32       `json:"price_unit"`
	AccountID      uuid.UUID   `json:"account_id"`
	OverDelivery   string      `json:"over_delivery_pct"`
	TaxCode        *string     `json:"tax_code"`
	ContractItemNo *int32      `json:"contract_item_no"`
	Received       string      `json:"received"`
}

// total is the items' total for display. Decisions use documentTotal,
// which fails closed.
func total(items []Item) money.Minor {
	sum, _ := itemsTotal(items) // display only: 0 for items stored out of range before checkTotal; decisions use documentTotal
	return sum
}

// ItemInput is an item as the API sends it.
type ItemInput struct {
	No             *int32
	MaterialID     *uuid.UUID
	MaterialKey    string
	Description    string
	Quantity, Unit string
	Price          string // decimal amount of Currency
	Currency       string
	PriceUnit      int32
	AccountID      uuid.UUID
	OverDelivery   *string
	TaxCode        *string
	ContractItemNo *int32
}

// Requisition is a row of procurement.requisition with its live items.
type Requisition struct {
	ID                uuid.UUID
	DocumentNumber    string
	LegalEntityID     uuid.UUID
	RequesterID       uuid.UUID
	SupplierID        *uuid.UUID
	ContractID        *uuid.UUID
	Currency          string
	Exponent          int
	NeededBy          *time.Time
	Status            string
	WorkflowRequestID *uuid.UUID
	Extensions        map[string]any
	Items             []Item
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

// Total is the estimated value of the live items.
func (r Requisition) Total() money.Minor { return total(r.Items) }

const requisitionSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.requester_id, r.supplier_id, r.contract_id, r.currency, c.exponent,
		r.needed_by, r.status, r.workflow_request_id, r.extensions, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by,
		coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'material_id', i.material_id, 'description', i.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code, 'unit_price', i.unit_price, 'price_unit', i.price_unit,
			'account_id', i.account_id) ORDER BY i.item_no)
		 FROM procurement.requisition_item i WHERE i.tenant_id = r.tenant_id AND i.requisition_id = r.id AND NOT i.is_removed), '[]')
	FROM procurement.requisition r JOIN ref.currency c ON c.code = r.currency`

func scanRequisition(row pgx.Row) (Requisition, error) {
	var r Requisition
	if err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.RequesterID, &r.SupplierID, &r.ContractID, &r.Currency, &r.Exponent,
		&r.NeededBy, &r.Status, &r.WorkflowRequestID, &r.Extensions, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy, &r.Items); err != nil {
		return Requisition{}, fmt.Errorf("scan requisition: %w", err)
	}
	return r, nil
}

// getRequisition loads a requisition of a legal entity the caller may act
// in; lock takes the row lock for a change.
func getRequisition(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Requisition, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Requisition{}, err
	}
	q := requisitionSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRequisition(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(r.LegalEntityID)) {
		return Requisition{}, notFound("requisition")
	}
	if err != nil {
		return Requisition{}, fmt.Errorf("procurement: get requisition %s: %w", id, err)
	}
	return r, nil
}

// GetRequisition returns a requisition.
func GetRequisition(ctx context.Context, id uuid.UUID) (Requisition, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Requisition{}, err
	}
	return getRequisition(ctx, tx, id, false)
}

// requisitionItems validates requisition items and returns them as rows.
func (e *Endpoints) requisitionItems(ctx context.Context, le uuid.UUID, currency string, exp int, in []ItemInput) ([]Item, error) {
	items := make([]Item, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		if err := checkItemNo(it.No, i, field); err != nil {
			return nil, err
		}
		if _, err := quantity(it.Quantity, field+".quantity"); err != nil {
			return nil, err
		}
		price, err := amount(it.Price, it.Currency, currency, exp, field+".unit_price", false)
		if err != nil {
			return nil, err
		}
		if err := e.account(ctx, le, it.AccountID, field+".account_id"); err != nil {
			return nil, err
		}
		items[i] = Item{No: int32(i + 1), MaterialID: it.MaterialID, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
			UnitPrice: price, PriceUnit: max(it.PriceUnit, 1), AccountID: it.AccountID}
	}
	return items, checkTotal(items)
}

// writeRequisitionItems stores the items as items 1..n and marks the rest removed.
func writeRequisitionItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, items []Item) error {
	n := len(items)
	nos, materials, descriptions, quantities, units := make([]int32, n), make([]*uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
	prices, priceUnits, accounts := make([]money.Minor, n), make([]int32, n), make([]uuid.UUID, n)
	for i, it := range items {
		nos[i], materials[i], descriptions[i], quantities[i], units[i] = it.No, it.MaterialID, it.Description, it.Quantity, it.Unit
		prices[i], priceUnits[i], accounts[i] = it.UnitPrice, it.PriceUnit, it.AccountID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.requisition_item AS i (tenant_id, requisition_id, item_no, material_id, description, quantity,
			uom_code, unit_price, price_unit, account_id)
		SELECT $1, $2, n, m, d, q::numeric, u, p, pu, a
		FROM unnest($3::int[], $4::uuid[], $5::text[], $6::text[], $7::text[], $8::bigint[], $9::int[], $10::uuid[]) AS x(n, m, d, q, u, p, pu, a)
		ON CONFLICT (tenant_id, requisition_id, item_no) DO UPDATE SET material_id = EXCLUDED.material_id, description = EXCLUDED.description,
			quantity = EXCLUDED.quantity, uom_code = EXCLUDED.uom_code, unit_price = EXCLUDED.unit_price, price_unit = EXCLUDED.price_unit,
			account_id = EXCLUDED.account_id, is_removed = false`,
		tenant, id, nos, materials, descriptions, quantities, units, prices, priceUnits, accounts); err != nil {
		return fmt.Errorf("procurement: write items of requisition %s: %w", id, err)
	}
	return removeItems(ctx, tx, "procurement.requisition_item", "requisition_id", id, n)
}

// checkRequisitionContract requires a contract the requisition can refer
// to: active, of the legal entity and currency, with the supplier if one is
// named. It returns the contract's supplier.
func checkRequisitionContract(ctx context.Context, tx pgx.Tx, contract *uuid.UUID, le uuid.UUID, currency string, supplier *uuid.UUID) (*uuid.UUID, error) {
	if contract == nil {
		return supplier, nil
	}
	c, err := getContract(ctx, tx, *contract, false)
	if err != nil {
		return nil, reference(err, "procurement.invalid_contract", "contract_id", "no contract with this id that you can access")
	}
	switch {
	case c.Status != Active:
		return nil, invalid("procurement.invalid_contract", "contract_id", "the contract is "+c.Status)
	case c.LegalEntityID != le || c.Currency != currency:
		return nil, invalid("procurement.invalid_contract", "contract_id", "a contract of the legal entity in the requisition currency")
	case supplier != nil && *supplier != c.SupplierID:
		return nil, invalid("procurement.invalid_contract", "contract_id", "a contract with the requisition's supplier")
	}
	return &c.SupplierID, nil
}

// NewRequisition is the input of createRequisition.
type NewRequisition struct {
	LegalEntityID uuid.UUID
	SupplierID    *uuid.UUID
	ContractID    *uuid.UUID
	Currency      string
	NeededBy      *time.Time
	Items         []ItemInput
	Extensions    map[string]any
}

// createRequisition creates a draft requisition of the caller.
func (e *Endpoints) createRequisition(ctx context.Context, in NewRequisition) (Requisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Requisition{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return Requisition{}, err
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	if err := checkExtensions(in.Extensions); err != nil {
		return Requisition{}, err
	}
	exp, err := exponent(ctx, tx, in.Currency, "currency")
	if err != nil {
		return Requisition{}, err
	}
	supplier, err := checkRequisitionContract(ctx, tx, in.ContractID, le.ID, in.Currency, in.SupplierID)
	if err != nil {
		return Requisition{}, err
	}
	if supplier != nil {
		if err := e.supplier(ctx, *supplier, le.ID); err != nil {
			return Requisition{}, err
		}
	}
	items, err := e.requisitionItems(ctx, le.ID, in.Currency, exp, in.Items)
	if err != nil {
		return Requisition{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Requisition{}, err
	}
	number, err := e.number(ctx, le.ID, "requisition", day)
	if err != nil {
		return Requisition{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, supplier_id, contract_id,
			currency, needed_by, status, extensions, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'draft', $10, $5, $5)`,
		p.TenantID, id, number, le.ID, p.ID, supplier, in.ContractID, in.Currency, in.NeededBy, in.Extensions); err != nil {
		return Requisition{}, fmt.Errorf("procurement: create requisition %s: %w", number, err)
	}
	if err := writeRequisitionItems(ctx, tx, p.TenantID, id, items); err != nil {
		return Requisition{}, err
	}
	r, err := getRequisition(ctx, tx, id, false)
	if err != nil {
		return Requisition{}, err
	}
	return r, audit.Record(ctx, "procurement.requisition.create", "procurement.requisition", id, map[string]any{"document_number": number, "items": len(items)})
}

// RequisitionChange is a merge patch of a draft requisition.
type RequisitionChange struct {
	Supplier, Contract, NeededBy bool // the field was sent (nil value clears it)
	SupplierID, ContractID       *uuid.UUID
	NeededByDate                 *time.Time
	Currency                     *string
	Items                        []ItemInput // nil: unchanged
	Extensions                   map[string]any
}

// loadDraft locks a requisition its requester may change.
func loadDraft(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error, action string, allowed ...string) (Requisition, error) {
	r, err := getRequisition(ctx, tx, id, true)
	if err != nil {
		return Requisition{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Requisition{}, err
	}
	if r.RequesterID != p.ID {
		return Requisition{}, problem.Forbidden("procurement.not_requester", "Only the requester may change, submit or cancel a requisition.")
	}
	if !allows(r.Status, allowed...) {
		return Requisition{}, statusConflict("requisition", r.Status, action, allowed...)
	}
	return r, nil
}

// updateRequisition changes a draft. Items are replaced as a whole; a new
// currency needs the items priced in it.
func (e *Endpoints) updateRequisition(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch RequisitionChange) (Requisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Requisition{}, err
	}
	r, err := loadDraft(ctx, tx, p, id, ifMatch, "update", Draft)
	if err != nil {
		return Requisition{}, err
	}
	f, err := changedRequisitionHeader(r, ch)
	if err != nil {
		return Requisition{}, err
	}
	supplier, contract, neededBy, currency, ext := f.supplier, f.contract, f.neededBy, f.currency, f.ext
	exp, err := exponent(ctx, tx, currency, "currency")
	if err != nil {
		return Requisition{}, err
	}
	if supplier, err = checkRequisitionContract(ctx, tx, contract, r.LegalEntityID, currency, supplier); err != nil {
		return Requisition{}, err
	}
	if supplier != nil {
		if err := e.supplier(ctx, *supplier, r.LegalEntityID); err != nil {
			return Requisition{}, err
		}
	}
	if ch.Items != nil {
		items, err := e.requisitionItems(ctx, r.LegalEntityID, currency, exp, ch.Items)
		if err != nil {
			return Requisition{}, err
		}
		if err := writeRequisitionItems(ctx, tx, p.TenantID, id, items); err != nil {
			return Requisition{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.requisition SET supplier_id = $2, contract_id = $3, needed_by = $4, currency = $5, extensions = $6,
		version = version + 1, updated_by = $7 WHERE id = $1`, id, supplier, contract, neededBy, currency, ext, p.ID); err != nil {
		return Requisition{}, fmt.Errorf("procurement: update requisition %s: %w", id, err)
	}
	updated, err := getRequisition(ctx, tx, id, false)
	if err != nil {
		return Requisition{}, err
	}
	return updated, audit.Record(ctx, "procurement.requisition.update", "procurement.requisition", id, map[string]any{"items": len(updated.Items)})
}

// requisitionHeaderFields are the header fields of a requisition after a
// change.
type requisitionHeaderFields struct {
	supplier, contract *uuid.UUID
	neededBy           *time.Time
	currency           string
	ext                map[string]any
}

// changedRequisitionHeader applies a change to the requisition's header
// fields; a new currency needs new items, and the merged extensions must
// be valid.
func changedRequisitionHeader(r Requisition, ch RequisitionChange) (requisitionHeaderFields, error) {
	f := requisitionHeaderFields{supplier: r.SupplierID, contract: r.ContractID, neededBy: r.NeededBy, currency: r.Currency, ext: r.Extensions}
	if ch.Supplier {
		f.supplier = ch.SupplierID
	}
	if ch.Contract {
		f.contract = ch.ContractID
	}
	if ch.NeededBy {
		f.neededBy = ch.NeededByDate
	}
	if ch.Currency != nil {
		f.currency = *ch.Currency
	}
	if f.currency != r.Currency && ch.Items == nil {
		return requisitionHeaderFields{}, invalid("procurement.currency_mismatch", "items", "a new currency needs the items priced in it")
	}
	if ch.Extensions != nil {
		f.ext = mergepatch.Apply(f.ext, ch.Extensions)
		if err := checkExtensions(f.ext); err != nil {
			return requisitionHeaderFields{}, err
		}
	}
	return f, nil
}

func requisitionEvent(name string, r Requisition, extra map[string]any) map[string]any {
	payload := map[string]any{"requisition_id": r.ID, "document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// setStatus moves a requisition on and publishes the event of the move.
func setStatus(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Requisition, status string, request *uuid.UUID, name string, extra map[string]any) (Requisition, error) {
	if _, err := tx.Exec(ctx, `UPDATE procurement.requisition SET status = $2, workflow_request_id = $3, version = version + 1, updated_by = $4 WHERE id = $1`,
		r.ID, status, request, p.ID); err != nil {
		return Requisition{}, fmt.Errorf("procurement: set requisition %s status %s: %w", r.ID, status, err)
	}
	updated, err := getRequisition(ctx, tx, r.ID, false)
	if err != nil {
		return Requisition{}, err
	}
	detail := map[string]any{"from_status": r.Status, "status": status}
	if name == "" {
		return updated, audit.Record(ctx, "procurement.requisition."+status, "procurement.requisition", r.ID, detail)
	}
	extra["status"] = status
	if name != "requisition_submitted" { // submission has no previous status in the event contract
		extra["previous_status"] = r.Status
	}
	return updated, audit.Record(ctx, "procurement.requisition."+status, "procurement.requisition", r.ID, detail,
		event(name, "requisition", r.ID, requisitionEvent(name, r, extra)))
}

// submitRequisition submits a draft for approval: the workflow context
// opens an approval request by amount tier, or approves it at once when the
// configuration requires no approval.
func (e *Endpoints) submitRequisition(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Requisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Requisition{}, err
	}
	r, err := loadDraft(ctx, tx, p, id, ifMatch, "submit", Draft)
	if err != nil {
		return Requisition{}, err
	}
	sum, err := documentTotal(r.Items)
	if err != nil {
		return Requisition{}, err
	}
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: id,
		Title: "Requisition " + r.DocumentNumber, Amount: &sum, Currency: &r.Currency})
	if err != nil {
		return Requisition{}, err
	}
	var link *uuid.UUID
	if needed {
		link = &request
	}
	submitted, err := setStatus(ctx, tx, p, r, PendingApproval, link, "requisition_submitted", map[string]any{"requester_id": r.RequesterID,
		"supplier_id": r.SupplierID, "contract_id": r.ContractID, "needed_by": date(r.NeededBy), "estimated_total": money.EventAmount(sum, r.Currency, r.Exponent),
		"item_count": len(r.Items)})
	if err != nil || needed {
		return submitted, err
	}
	return setStatus(ctx, tx, p, submitted, Approved, nil, "requisition_approved", map[string]any{"workflow_request_id": nil})
}

func date(d *time.Time) any {
	if d == nil {
		return nil
	}
	return d.Format(time.DateOnly)
}

// cancelRequisition cancels a requisition that is not yet ordered and
// withdraws its pending approval request.
func (e *Endpoints) cancelRequisition(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Requisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Requisition{}, err
	}
	r, err := loadDraft(ctx, tx, p, id, ifMatch, "cancel", Draft, PendingApproval, Approved)
	if err != nil {
		return Requisition{}, err
	}
	if r.Status == PendingApproval {
		if err := e.Approvals.Withdraw(ctx, approvals.Requisition, id, "The requisition was cancelled."); err != nil {
			return Requisition{}, err
		}
	}
	return setStatus(ctx, tx, p, r, Cancelled, r.WorkflowRequestID, "requisition_cancelled", map[string]any{"requester_id": r.RequesterID,
		"workflow_request_id": r.WorkflowRequestID})
}

// decide applies the outcome of a requisition's approval request.
func decide(ctx context.Context, id uuid.UUID, status, name string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := getRequisition(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if r.Status != PendingApproval {
		return statusConflict("requisition", r.Status, "an approval decision", PendingApproval)
	}
	request := r.WorkflowRequestID
	var extra map[string]any
	if name != "" {
		extra = map[string]any{"workflow_request_id": request}
	} else {
		request = nil
	}
	_, err = setStatus(ctx, tx, p, r, status, request, name, extra)
	return err
}
