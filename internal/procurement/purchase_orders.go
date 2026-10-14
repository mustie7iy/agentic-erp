package procurement

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// PurchaseOrder is a row of procurement.purchase_order with its live items
// and their received quantities.
type PurchaseOrder struct {
	ID              uuid.UUID
	DocumentNumber  string
	LegalEntityID   uuid.UUID
	PurchasingOrgID *uuid.UUID
	SupplierID      uuid.UUID
	Currency        string
	Exponent        int
	OrderDate       time.Time
	PaymentTermCode string
	RequisitionID   *uuid.UUID
	ContractID      *uuid.UUID
	Status          string
	Extensions      map[string]any
	Items           []Item
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

// Total is the value of the live items.
func (o PurchaseOrder) Total() money.Minor { return total(o.Items) }

const orderSelect = `SELECT o.id, o.document_number, o.legal_entity_id, o.purchasing_org_id, o.supplier_id, o.currency, c.exponent, o.order_date,
		o.payment_term_code, o.requisition_id, o.contract_id, o.status, o.extensions, o.version, o.created_at, o.created_by, o.updated_at, o.updated_by,
		coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'material_id', i.material_id, 'description', i.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code, 'unit_price', i.unit_price, 'price_unit', i.price_unit,
			'account_id', i.account_id, 'over_delivery_pct', trim_scale(i.over_delivery_pct)::text, 'tax_code', i.tax_code,
			'contract_item_no', i.contract_item_no,
			'received', trim_scale(coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g WHERE g.tenant_id = i.tenant_id
				AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text) ORDER BY i.item_no)
		 FROM procurement.purchase_order_item i WHERE i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND NOT i.is_removed), '[]')
	FROM procurement.purchase_order o JOIN ref.currency c ON c.code = o.currency`

func scanOrder(row pgx.Row) (PurchaseOrder, error) {
	var o PurchaseOrder
	if err := row.Scan(&o.ID, &o.DocumentNumber, &o.LegalEntityID, &o.PurchasingOrgID, &o.SupplierID, &o.Currency, &o.Exponent, &o.OrderDate,
		&o.PaymentTermCode, &o.RequisitionID, &o.ContractID, &o.Status, &o.Extensions, &o.Version, &o.CreatedAt, &o.CreatedBy, &o.UpdatedAt,
		&o.UpdatedBy, &o.Items); err != nil {
		return PurchaseOrder{}, fmt.Errorf("scan purchase order: %w", err)
	}
	return o, nil
}

func getOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (PurchaseOrder, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	q := orderSelect + ` WHERE o.id = $1`
	if lock {
		q += ` FOR UPDATE OF o`
	}
	o, err := scanOrder(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(o.LegalEntityID)) {
		return PurchaseOrder{}, notFound("purchase order")
	}
	if err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: get purchase order %s: %w", id, err)
	}
	return o, nil
}

// GetOrder returns a purchase order.
func GetOrder(ctx context.Context, id uuid.UUID) (PurchaseOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return getOrder(ctx, tx, id, false)
}

// usableContract requires a contract an order of the legal entity,
// supplier and currency can use on the order date.
func usableContract(ctx context.Context, tx pgx.Tx, id uuid.UUID, le, supplier uuid.UUID, currency string, on time.Time) (Contract, error) {
	c, err := getContract(ctx, tx, id, false)
	if err != nil {
		return Contract{}, reference(err, "procurement.invalid_contract", "contract_id", "no contract with this id that you can access")
	}
	switch {
	case c.Status != Active:
		return Contract{}, invalid("procurement.invalid_contract", "contract_id", "the contract is "+c.Status)
	case c.LegalEntityID != le || c.SupplierID != supplier || c.Currency != currency:
		return Contract{}, invalid("procurement.invalid_contract", "contract_id", "a contract of the legal entity with the order's supplier and currency")
	case on.Before(c.ValidFrom) || on.After(c.ValidTo):
		return Contract{}, invalid("procurement.invalid_contract", "contract_id",
			fmt.Sprintf("the contract is valid from %s to %s", c.ValidFrom.Format(time.DateOnly), c.ValidTo.Format(time.DateOnly)))
	}
	return c, nil
}

// orderHeader is what order items are checked against.
type orderHeader struct {
	le        tenancy.Entity
	currency  string
	exp       int
	orderDate time.Time
	contract  *Contract
	tolerance string // the company's default over-delivery percentage
}

// orderItems validates order items. Contract items take the contract's
// price; the over-delivery tolerance defaults to the company's.
func (e *Endpoints) orderItems(ctx context.Context, h orderHeader, in []ItemInput) ([]Item, error) {
	items := make([]Item, len(in))
	for i, it := range in {
		item, err := e.orderItem(ctx, h, i, it)
		if err != nil {
			return nil, err
		}
		items[i] = item
	}
	return items, checkTotal(items)
}

// orderItem validates item i of an order: number, quantity, price in the
// order currency, tolerance, contract item, account and tax code.
func (e *Endpoints) orderItem(ctx context.Context, h orderHeader, i int, it ItemInput) (Item, error) {
	field := fmt.Sprintf("items[%d]", i)
	if err := checkItemNo(it.No, i, field); err != nil {
		return Item{}, err
	}
	if _, err := quantity(it.Quantity, field+".quantity"); err != nil {
		return Item{}, err
	}
	price, err := amount(it.Price, it.Currency, h.currency, h.exp, field+".unit_price", false)
	if err != nil {
		return Item{}, err
	}
	item := Item{No: int32(i + 1), MaterialID: it.MaterialID, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
		UnitPrice: price, PriceUnit: max(it.PriceUnit, 1), AccountID: it.AccountID, OverDelivery: h.tolerance, TaxCode: it.TaxCode,
		ContractItemNo: it.ContractItemNo}
	if it.OverDelivery != nil {
		pct, ok := new(big.Rat).SetString(*it.OverDelivery)
		if !ok || pct.Sign() < 0 || pct.Cmp(big.NewRat(100, 1)) > 0 {
			return Item{}, invalid("procurement.invalid_tolerance", field+".over_delivery_pct", "a percentage from 0 to 100")
		}
		item.OverDelivery = *it.OverDelivery
	}
	if err := applyContractPrice(&item, h.contract, it, field); err != nil {
		return Item{}, err
	}
	if err := e.account(ctx, h.le.ID, it.AccountID, field+".account_id"); err != nil {
		return Item{}, err
	}
	if it.TaxCode != nil {
		_, ok, err := e.Accounts.TaxCode(ctx, h.le.ID, *it.TaxCode, h.orderDate)
		if err != nil {
			return Item{}, err
		}
		if !ok {
			return Item{}, invalid("procurement.unknown_tax_code", field+".tax_code", "a tax code of the legal entity valid on the order date")
		}
	}
	return item, nil
}

// applyContractPrice gives an item on a contract item the contract's
// price; the contract item must exist in the item's unit.
func applyContractPrice(item *Item, contract *Contract, it ItemInput, field string) error {
	if it.ContractItemNo == nil {
		return nil
	}
	if contract == nil {
		return invalid("procurement.invalid_contract_item", field+".contract_item_no", "contract items need the order's contract_id")
	}
	var ci *Item
	for j := range contract.Items {
		if contract.Items[j].No == *it.ContractItemNo {
			ci = &contract.Items[j]
		}
	}
	if ci == nil || ci.Unit != it.Unit {
		return invalid("procurement.invalid_contract_item", field+".contract_item_no", "an item of the contract in the same unit")
	}
	item.UnitPrice, item.PriceUnit = ci.UnitPrice, ci.PriceUnit
	return nil
}

func writeOrderItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, items []Item) error {
	n := len(items)
	nos, materials, descriptions, quantities, units := make([]int32, n), make([]*uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
	prices, priceUnits, accounts, tolerances := make([]money.Minor, n), make([]int32, n), make([]uuid.UUID, n), make([]string, n)
	taxes, contractItems := make([]*string, n), make([]*int32, n)
	for i, it := range items {
		nos[i], materials[i], descriptions[i], quantities[i], units[i] = it.No, it.MaterialID, it.Description, it.Quantity, it.Unit
		prices[i], priceUnits[i], accounts[i], tolerances[i] = it.UnitPrice, it.PriceUnit, it.AccountID, it.OverDelivery
		taxes[i], contractItems[i] = it.TaxCode, it.ContractItemNo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.purchase_order_item AS i (tenant_id, purchase_order_id, item_no, material_id, description, quantity,
			uom_code, unit_price, price_unit, account_id, over_delivery_pct, tax_code, contract_item_no)
		SELECT $1, $2, n, m, d, q::numeric, u, p, pu, a, t::numeric, tx, ci
		FROM unnest($3::int[], $4::uuid[], $5::text[], $6::text[], $7::text[], $8::bigint[], $9::int[], $10::uuid[], $11::text[], $12::text[], $13::int[])
			AS x(n, m, d, q, u, p, pu, a, t, tx, ci)
		ON CONFLICT (tenant_id, purchase_order_id, item_no) DO UPDATE SET material_id = EXCLUDED.material_id, description = EXCLUDED.description,
			quantity = EXCLUDED.quantity, uom_code = EXCLUDED.uom_code, unit_price = EXCLUDED.unit_price, price_unit = EXCLUDED.price_unit,
			account_id = EXCLUDED.account_id, over_delivery_pct = EXCLUDED.over_delivery_pct, tax_code = EXCLUDED.tax_code,
			contract_item_no = EXCLUDED.contract_item_no, is_removed = false`,
		tenant, id, nos, materials, descriptions, quantities, units, prices, priceUnits, accounts, tolerances, taxes, contractItems); err != nil {
		return fmt.Errorf("procurement: write items of purchase order %s: %w", id, err)
	}
	return removeItems(ctx, tx, "procurement.purchase_order_item", "purchase_order_id", id, n)
}

// header checks what an order's items depend on: currency, contract,
// purchasing organization and the company's default tolerance.
func (e *Endpoints) header(ctx context.Context, tx pgx.Tx, le tenancy.Entity, supplier uuid.UUID, currency string, orderDate time.Time,
	contract, purchasingOrg *uuid.UUID) (orderHeader, error) {
	h := orderHeader{le: le, currency: currency, orderDate: orderDate, tolerance: "0"}
	var err error
	if h.exp, err = exponent(ctx, tx, currency, "currency"); err != nil {
		return h, err
	}
	if err := e.supplier(ctx, supplier, le.ID); err != nil {
		return h, err
	}
	if err := e.orgUnit(ctx, purchasingOrg, le.ID, "purchasing_org", "purchasing_org_id"); err != nil {
		return h, err
	}
	if contract != nil {
		c, err := usableContract(ctx, tx, *contract, le.ID, supplier, currency, orderDate)
		if err != nil {
			return h, err
		}
		h.contract = &c
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return h, err
	}
	if co := doc.Company(le.Code); co != nil {
		h.tolerance = strconv.Itoa(co.OverDeliveryPercent)
	}
	return h, nil
}

// paymentTerm defaults the terms to the supplier's and requires them to exist.
func (e *Endpoints) paymentTerm(ctx context.Context, code *string, supplier, le uuid.UUID) (string, error) {
	if code == nil {
		s, _, err := e.Suppliers.Supplier(ctx, supplier, le) // the supplier was checked
		return s.PaymentTermCode, err
	}
	ok, err := e.Suppliers.PaymentTermExists(ctx, *code)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", invalid("procurement.unknown_payment_term", "payment_term_code", "payment terms from the configuration")
	}
	return *code, nil
}

// NewOrder is the input of createOrder.
type NewOrder struct {
	LegalEntityID   uuid.UUID
	PurchasingOrgID *uuid.UUID
	SupplierID      uuid.UUID
	Currency        string
	OrderDate       time.Time
	PaymentTermCode *string
	RequisitionID   *uuid.UUID
	ContractID      *uuid.UUID
	Items           []ItemInput
	Extensions      map[string]any
}

// createOrder creates a draft purchase order. An order from a requisition
// needs an approved requisition of the legal entity and currency, which
// becomes ordered.
func (e *Endpoints) createOrder(ctx context.Context, in NewOrder) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	if err := checkExtensions(in.Extensions); err != nil {
		return PurchaseOrder{}, err
	}
	req, err := orderedRequisition(ctx, tx, &in, le.ID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	h, err := e.header(ctx, tx, le, in.SupplierID, in.Currency, in.OrderDate, in.ContractID, in.PurchasingOrgID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	term, err := e.paymentTerm(ctx, in.PaymentTermCode, in.SupplierID, le.ID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	items, err := e.orderItems(ctx, h, in.Items)
	if err != nil {
		return PurchaseOrder{}, err
	}
	number, err := e.number(ctx, le.ID, "purchase_order", in.OrderDate)
	if err != nil {
		return PurchaseOrder{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, purchasing_org_id, supplier_id,
			currency, order_date, payment_term_code, requisition_id, contract_id, status, extensions, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'draft', $12, $13, $13)`,
		p.TenantID, id, number, le.ID, in.PurchasingOrgID, in.SupplierID, in.Currency, in.OrderDate, term, in.RequisitionID, in.ContractID,
		in.Extensions, p.ID); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: create purchase order %s: %w", number, err)
	}
	if err := writeOrderItems(ctx, tx, p.TenantID, id, items); err != nil {
		return PurchaseOrder{}, err
	}
	if in.RequisitionID != nil {
		if _, err := setStatus(ctx, tx, p, req, Ordered, req.WorkflowRequestID, "requisition_ordered", map[string]any{"purchase_order_id": id}); err != nil {
			return PurchaseOrder{}, err
		}
	}
	o, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return o, audit.Record(ctx, "procurement.purchase_order.create", "procurement.purchase_order", id, map[string]any{"document_number": number,
		"items": len(items), "requisition_id": in.RequisitionID})
}

// orderedRequisition locks the requisition an order is created from, if
// any: an approved requisition of the legal entity in the order currency,
// whose contract the order takes unless it names its own.
func orderedRequisition(ctx context.Context, tx pgx.Tx, in *NewOrder, le uuid.UUID) (Requisition, error) {
	if in.RequisitionID == nil {
		return Requisition{}, nil
	}
	req, err := getRequisition(ctx, tx, *in.RequisitionID, true)
	if err != nil {
		return Requisition{}, reference(err, "request.reference_not_found", "requisition_id", "no requisition with this id that you can access")
	}
	switch {
	case req.Status != Approved:
		return Requisition{}, statusConflict("requisition", req.Status, "ordering", Approved)
	case req.LegalEntityID != le || req.Currency != in.Currency:
		return Requisition{}, invalid("procurement.requisition_mismatch", "requisition_id", "a requisition of the legal entity in the order currency")
	}
	if in.ContractID == nil {
		in.ContractID = req.ContractID
	}
	return req, nil
}

// OrderChange is a merge patch of a draft purchase order.
type OrderChange struct {
	PurchasingOrg, Contract     bool // the field was sent (nil value clears it)
	PurchasingOrgID, ContractID *uuid.UUID
	SupplierID                  *uuid.UUID
	Currency                    *string
	OrderDate                   *time.Time
	PaymentTermCode             *string
	Items                       []ItemInput // nil: unchanged
	Extensions                  map[string]any
}

func loadOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error, action string, allowed ...string) (PurchaseOrder, error) {
	o, err := getOrder(ctx, tx, id, true)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if err := ifMatch(o.Version); err != nil {
		return PurchaseOrder{}, err
	}
	if !allows(o.Status, allowed...) {
		return PurchaseOrder{}, statusConflict("purchase order", o.Status, action, allowed...)
	}
	return o, nil
}

// updateOrder changes a draft order; header changes re-check the items.
func (e *Endpoints) updateOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch OrderChange) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	o, err := loadOrder(ctx, tx, id, ifMatch, "update", Draft)
	if err != nil {
		return PurchaseOrder{}, err
	}
	f, err := changedOrderHeader(o, ch)
	if err != nil {
		return PurchaseOrder{}, err
	}
	org, contract, supplier, currency, orderDate, term, ext := f.org, f.contract, f.supplier, f.currency, f.orderDate, o.PaymentTermCode, f.ext
	le, err := e.legalEntity(ctx, o.LegalEntityID)
	if err != nil {
		return PurchaseOrder{}, err
	}
	h, err := e.header(ctx, tx, le, supplier, currency, orderDate, contract, org)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if ch.PaymentTermCode != nil || supplier != o.SupplierID {
		if term, err = e.paymentTerm(ctx, ch.PaymentTermCode, supplier, le.ID); err != nil {
			return PurchaseOrder{}, err
		}
	}
	in := ch.Items
	if in == nil { // the current items are checked again against the new header
		in = currentItems(o)
	}
	items, err := e.orderItems(ctx, h, in)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if err := writeOrderItems(ctx, tx, p.TenantID, id, items); err != nil {
		return PurchaseOrder{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET purchasing_org_id = $2, supplier_id = $3, currency = $4, order_date = $5,
		payment_term_code = $6, contract_id = $7, extensions = $8, version = version + 1, updated_by = $9 WHERE id = $1`,
		id, org, supplier, currency, orderDate, term, contract, ext, p.ID); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: update purchase order %s: %w", id, err)
	}
	updated, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return updated, audit.Record(ctx, "procurement.purchase_order.update", "procurement.purchase_order", id, map[string]any{"items": len(updated.Items)})
}

// orderHeaderFields are the header fields of an order after a change.
type orderHeaderFields struct {
	org, contract *uuid.UUID
	supplier      uuid.UUID
	currency      string
	orderDate     time.Time
	ext           map[string]any
}

// changedOrderHeader applies a change to the order's header fields; a new
// currency needs new items, and the merged extensions must be valid.
func changedOrderHeader(o PurchaseOrder, ch OrderChange) (orderHeaderFields, error) {
	f := orderHeaderFields{org: o.PurchasingOrgID, contract: o.ContractID, supplier: o.SupplierID, currency: o.Currency, orderDate: o.OrderDate, ext: o.Extensions}
	if ch.PurchasingOrg {
		f.org = ch.PurchasingOrgID
	}
	if ch.Contract {
		f.contract = ch.ContractID
	}
	if ch.SupplierID != nil {
		f.supplier = *ch.SupplierID
	}
	if ch.Currency != nil {
		f.currency = *ch.Currency
	}
	if ch.OrderDate != nil {
		f.orderDate = *ch.OrderDate
	}
	if f.currency != o.Currency && ch.Items == nil {
		return orderHeaderFields{}, invalid("procurement.currency_mismatch", "items", "a new currency needs the items priced in it")
	}
	if ch.Extensions != nil {
		f.ext = mergepatch.Apply(f.ext, ch.Extensions)
		if err := checkExtensions(f.ext); err != nil {
			return orderHeaderFields{}, err
		}
	}
	return f, nil
}

// currentItems are an order's items as input, so that they can be checked
// again against a changed header.
func currentItems(o PurchaseOrder) []ItemInput {
	var in []ItemInput
	for _, it := range o.Items {
		price := it.UnitPrice
		in = append(in, ItemInput{MaterialID: it.MaterialID, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
			Price: money.Minor(price).Plain(o.Exponent), Currency: o.Currency, PriceUnit: it.PriceUnit, AccountID: it.AccountID,
			OverDelivery: &it.OverDelivery, TaxCode: it.TaxCode, ContractItemNo: it.ContractItemNo})
	}
	return in
}

func orderEvent(o PurchaseOrder, extra map[string]any) map[string]any {
	payload := map[string]any{"purchase_order_id": o.ID, "document_number": o.DocumentNumber, "legal_entity_id": o.LegalEntityID,
		"supplier_id": o.SupplierID}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// releaseOrder releases a draft order to the supplier. The supplier must
// still be usable, and a contract must still be usable and cover the order
// value, which the release consumes.
func (e *Endpoints) releaseOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	o, err := loadOrder(ctx, tx, id, ifMatch, "release", Draft)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if err := e.supplier(ctx, o.SupplierID, o.LegalEntityID); err != nil {
		return PurchaseOrder{}, err
	}
	value := o.Total()
	if o.ContractID != nil {
		if _, err := tx.Exec(ctx, `SELECT FROM procurement.contract WHERE id = $1 FOR UPDATE`, *o.ContractID); err != nil {
			return PurchaseOrder{}, fmt.Errorf("procurement: release purchase order %s: lock contract %s: %w", id, *o.ContractID, err)
		}
		c, err := usableContract(ctx, tx, *o.ContractID, o.LegalEntityID, o.SupplierID, o.Currency, o.OrderDate)
		if err != nil {
			return PurchaseOrder{}, err
		}
		// Against the remainder: consumed plus the order can be out of range.
		if value > c.TargetValue-c.ConsumedValue {
			return PurchaseOrder{}, problem.Conflict("procurement.contract_exceeded", fmt.Sprintf("The order (%s) exceeds what remains of contract %s (%s).",
				value.Plain(o.Exponent), c.DocumentNumber, (c.TargetValue-c.ConsumedValue).Plain(c.Exponent)))
		}
		if value != 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO procurement.contract_release (tenant_id, contract_id, purchase_order_id, value, released_at, released_by)
				VALUES ($1, $2, $3, $4, clock_timestamp(), $5)`, p.TenantID, c.ID, id, value, p.ID); err != nil {
				return PurchaseOrder{}, fmt.Errorf("procurement: release purchase order %s: consume contract %s: %w", id, c.ID, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: release purchase order %s: %w", id, err)
	}
	released, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	items := make([]map[string]any, len(o.Items))
	for i, it := range o.Items {
		items[i] = map[string]any{"item_no": it.No, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit},
			"unit_price": money.EventAmount(it.UnitPrice, o.Currency, o.Exponent), "price_unit": it.PriceUnit, "account_id": it.AccountID,
			"tax_code": it.TaxCode, "contract_item_no": it.ContractItemNo}
	}
	return released, audit.Record(ctx, "procurement.purchase_order.release", "procurement.purchase_order", id, map[string]any{"value": value},
		event("purchase_order_released", "purchase_order", id, orderEvent(o, map[string]any{"purchasing_org_id": o.PurchasingOrgID,
			"order_date": o.OrderDate.Format(time.DateOnly), "payment_term_code": o.PaymentTermCode, "requisition_id": o.RequisitionID,
			"contract_id": o.ContractID, "total_amount": money.EventAmount(value, o.Currency, o.Exponent), "items": items})))
}

// closeOrder closes a released order: no further receipts or invoices are
// expected (receipts and invoice matching refuse a closed order). Orders
// fully received and invoiced are closed by the buyer the same way; the
// contract's consumption stays.
func (e *Endpoints) closeOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	o, err := loadOrder(ctx, tx, id, ifMatch, "close", Released)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET status = 'closed', version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: close purchase order %s: %w", id, err)
	}
	closed, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return closed, audit.Record(ctx, "procurement.purchase_order.close", "procurement.purchase_order", id, map[string]any{"from_status": o.Status},
		event("purchase_order_closed", "purchase_order", id, orderEvent(o, map[string]any{"previous_status": o.Status, "status": Closed})))
}

// cancelOrder cancels an order with nothing received or invoiced against
// it. A released contract order gives its contract consumption back, and
// the requisition can be ordered again.
func (e *Endpoints) cancelOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	o, err := loadOrder(ctx, tx, id, ifMatch, "cancel", Draft, Released)
	if err != nil {
		return PurchaseOrder{}, err
	}
	var received bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM procurement.goods_movement_item WHERE purchase_order_id = $1)`, id).Scan(&received); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: cancel purchase order %s: check receipts: %w", id, err)
	}
	if received {
		return PurchaseOrder{}, problem.Conflict("procurement.order_received", "Goods were received against the order; it cannot be cancelled.")
	}
	invoiced := false
	if e.Invoices != nil { // callers other than the API may leave it out
		if invoiced, err = e.Invoices.HasInvoices(ctx, id); err != nil {
			return PurchaseOrder{}, err
		}
	}
	if invoiced {
		return PurchaseOrder{}, problem.Conflict("procurement.order_invoiced", "Invoices refer to the order; it cannot be cancelled.")
	}
	var releasedValue any
	if o.Status == Released && o.ContractID != nil && o.Total() != 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO procurement.contract_release (tenant_id, contract_id, purchase_order_id, value, released_at, released_by)
			VALUES ($1, $2, $3, $4, clock_timestamp(), $5)`, p.TenantID, *o.ContractID, id, -o.Total(), p.ID); err != nil {
			return PurchaseOrder{}, fmt.Errorf("procurement: cancel purchase order %s: return contract %s consumption: %w", id, *o.ContractID, err)
		}
		releasedValue = money.EventAmount(o.Total(), o.Currency, o.Exponent)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET status = 'cancelled', version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: cancel purchase order %s: %w", id, err)
	}
	if o.RequisitionID != nil {
		req, err := getRequisition(ctx, tx, *o.RequisitionID, true)
		if err != nil {
			return PurchaseOrder{}, err
		}
		if _, err := setStatus(ctx, tx, p, req, Approved, req.WorkflowRequestID, "", nil); err != nil {
			return PurchaseOrder{}, err
		}
	}
	cancelled, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return cancelled, audit.Record(ctx, "procurement.purchase_order.cancel", "procurement.purchase_order", id, map[string]any{"from_status": o.Status},
		event("purchase_order_cancelled", "purchase_order", id, orderEvent(o, map[string]any{"requisition_id": o.RequisitionID,
			"contract_id": o.ContractID, "released_value": releasedValue, "previous_status": o.Status, "status": Cancelled})))
}
