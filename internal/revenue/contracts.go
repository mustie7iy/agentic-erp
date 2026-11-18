package revenue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// What made a contract, as its audit record says it.
const (
	byRequest      = "request"
	byConfirmation = "sales_order_confirmed"
)

// contractExists refuses a second contract for a sales order, saying how
// the order's contract is found.
func contractExists(orderNumber string) *problem.Problem {
	return problem.Conflict("revenue.contract_exists",
		"Sales order "+orderNumber+" already has its revenue contract; an order has one. GET /v1/revenue-contracts?sales_order_id= finds it.")
}

// Contract is a row of revenue.contract with its obligations by number.
// TransactionPrice is minor units of Currency, the company's local
// currency, whose minor-unit digits are Exponent. Changes counts the
// follows of its sales order kept (T81e); EndedAt is when its order was
// found cancelled, nil while it stands.
type Contract struct {
	ID               uuid.UUID
	LegalEntityID    uuid.UUID
	CustomerID       uuid.UUID
	SalesOrderID     uuid.UUID
	SalesOrderNumber string
	Currency         string
	Exponent         int
	TransactionPrice money.Minor
	Status           string
	Obligations      []Obligation
	Changes          int32
	EndedAt          *time.Time
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

// contractSelect reads contracts with their obligations by number. The
// obligations come from a subquery, not a join: the keyset of the list
// orders by an unqualified id. A service date is written as an instant at
// midnight UTC, which is what a date is everywhere in the context.
const contractSelect = `SELECT k.id, k.legal_entity_id, k.customer_id, k.sales_order_id, k.sales_order_number, k.currency, c.exponent,
		k.transaction_price, k.status,
		coalesce((SELECT json_agg(json_build_object('obligation_no', o.obligation_no, 'order_item_no', o.order_item_no, 'item_key', o.item_key,
				'description', o.description, 'net_amount', o.net_amount, 'timing', o.timing,
				'service_from', to_char(o.service_from, 'YYYY-MM-DD"T00:00:00Z"'), 'service_to', to_char(o.service_to, 'YYYY-MM-DD"T00:00:00Z"'),
				'satisfied_on', to_char(o.satisfied_on, 'YYYY-MM-DD"T00:00:00Z"'),
				'standalone_selling_price', o.standalone_selling_price, 'allocated_amount', o.allocated_amount, 'ended_at', o.ended_at) ORDER BY o.obligation_no)
			FROM revenue.performance_obligation o WHERE o.tenant_id = k.tenant_id AND o.contract_id = k.id), '[]'),
		k.changes, k.ended_at, k.version, k.created_at, k.created_by, k.updated_at, k.updated_by
	FROM revenue.contract k JOIN ref.currency c ON c.code = k.currency`

func scanContract(row pgx.Row) (Contract, error) {
	var c Contract
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.CustomerID, &c.SalesOrderID, &c.SalesOrderNumber, &c.Currency, &c.Exponent,
		&c.TransactionPrice, &c.Status, &c.Obligations, &c.Changes, &c.EndedAt, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan revenue contract")
}

// load reads a contract whatever its company; GetContract applies company
// access.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Contract, error) {
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Contract{}, problem.NotFoundOf("revenue contract")
	}
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: read contract %s: %w", id, err)
	}
	return c, nil
}

// GetContract returns a revenue contract of a legal entity the caller may
// act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetContract(ctx context.Context, id uuid.UUID) (Contract, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: company access for contract %s: %w", id, err)
	}
	c, err := load(ctx, tx, id)
	if err == nil && !access.Allows(c.LegalEntityID) {
		return Contract{}, problem.NotFoundOf("revenue contract")
	}
	return c, err
}

// basis is what a contract is made of: its sales order and the order's
// company.
type basis struct {
	order sales.Order
	le    tenancy.Entity
}

// Why no contract is made for a sales order that is there (ADR-0068 §3).
const (
	notConfirmed    = "revenue.order_not_confirmed"
	foreignCurrency = "revenue.foreign_currency"
)

// declined is why no contract is made for a sales order: a refusal for
// the person who asked, and a line in the log for the consumer, which has
// nobody to answer. Without a code the order is not there, or not the
// caller's to read: a wrong reference.
type declined struct{ code, detail string }

// problem is the refusal as the API answers it, 422 naming
// sales_order_id.
func (d *declined) problem() *problem.Problem {
	switch d.code {
	case notConfirmed:
		return problem.Invalid("revenue.order_not_confirmed", "sales_order_id", d.detail)
	case foreignCurrency:
		return problem.Invalid("revenue.foreign_currency", "sales_order_id", d.detail)
	}
	return problem.ReferenceNotFound("sales_order_id", "sales order", d.detail)
}

// basisOf reads the order a contract is asked for, under a share lock
// (sales.Orders.Shared), and its company, or says why no contract is made
// for it (basis).
func (e *Endpoints) basisOf(ctx context.Context, orderID uuid.UUID, allowed func(company uuid.UUID) bool) (basis, *declined, error) {
	o, ok, err := e.SalesOrders.Shared(ctx, orderID)
	if err != nil {
		return basis{}, nil, fmt.Errorf("revenue: sales order %s of a contract: %w", orderID, err)
	}
	return e.basis(ctx, o, ok, allowed)
}

// basis is the order read (ok false: there is none) with its company, or
// why no contract is made for it: an order that is not there or of a
// company allowed does not allow, one that is not confirmed
// (revenue.order_not_confirmed) and one in another currency than its
// company's local currency (revenue.foreign_currency).
func (e *Endpoints) basis(ctx context.Context, o sales.Order, ok bool, allowed func(company uuid.UUID) bool) (basis, *declined, error) {
	if !ok || !allowed(o.LegalEntityID) {
		return basis{}, &declined{detail: "no sales order with this id that you can access"}, nil
	}
	if o.Status != sales.Confirmed {
		return basis{}, &declined{code: notConfirmed,
			detail: "Sales order " + o.DocumentNumber + " is " + o.Status + "; a revenue contract is created for a confirmed order."}, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return basis{}, nil, fmt.Errorf("revenue: company of sales order %s: %w", o.ID, err)
	}
	if o.Currency != le.LocalCurrency {
		return basis{}, &declined{code: foreignCurrency, detail: "Sales order " + o.DocumentNumber + " is in " + o.Currency +
			" and company " + le.Code + "'s local currency is " + le.LocalCurrency + "; revenue contracts in a foreign currency are not supported yet."}, nil
	}
	return basis{order: o, le: le}, nil, nil
}

// Create creates the revenue contract of a confirmed sales order of a
// company the caller may act in, on a person's request (ADR-0068 §3-§4).
// An order that has its contract, from its confirmation or an earlier
// request, is refused, 409 revenue.contract_exists.
func (e *Endpoints) Create(ctx context.Context, orderID uuid.UUID) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: company access for the contract of sales order %s: %w", orderID, err)
	}
	b, no, err := e.basisOf(ctx, orderID, access.Allows)
	if err != nil {
		return Contract{}, err
	}
	if no != nil {
		return Contract{}, no.problem()
	}
	c, created, err := e.make(ctx, tx, p, b, byRequest)
	if err == nil && !created {
		return Contract{}, contractExists(b.order.DocumentNumber)
	}
	return c, err
}

// make creates the contract of the basis's order unless the order has
// one, and says whether it did (ADR-0068 §3-§4): a draft with the order's
// net value as its transaction price and one obligation per order item,
// numbered in the items' order, each satisfied at a point in time with
// the item's net value as its stand-alone selling price. An item's net
// value is read from the order, never computed from its quantity and
// price. The insert leaves an order's existing contract as it is and
// waits for one being created by another transaction, so the order's
// confirmation and a person's request end with one contract. The
// contract's audit record and RevenueContractChanged are written in the
// same transaction, tx, the unit of work's, as p. Nothing is posted.
func (e *Endpoints) make(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b basis, source string) (Contract, bool, error) {
	o, id := b.order, uuid.Must(uuid.NewV7())
	tag, err := tx.Exec(ctx, `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
			transaction_price, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) ON CONFLICT ON CONSTRAINT contract_one_per_sales_order DO NOTHING`,
		p.TenantID, id, o.LegalEntityID, o.CustomerID, o.ID, o.DocumentNumber, o.Currency, o.Net, Draft, p.ID)
	if err != nil {
		return Contract{}, false, fmt.Errorf("revenue: insert the contract of sales order %s: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return Contract{}, false, nil
	}
	items := o.Standing() // a removed item (T78e) is not ordered, and is out of the transaction price
	obligations := make([]Obligation, len(items))
	for i, it := range items {
		obligations[i] = Obligation{No: int32(i + 1), OrderItemNo: it.ItemNo, ItemKey: it.ItemKey, Description: it.Description, Net: it.Net,
			Timing: PointInTime, StandalonePrice: it.Net}
	}
	if err := insertObligations(ctx, tx, p.TenantID, id, obligations); err != nil {
		return Contract{}, false, err
	}
	c, err := load(ctx, tx, id)
	if err != nil {
		return Contract{}, false, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{c.CustomerID})
	if err != nil {
		return Contract{}, false, fmt.Errorf("revenue: customer of contract %s: %w", id, err)
	}
	return c, true, record(ctx, "revenue.contract.create", source, c, b.le, names[c.CustomerID].Number, nil, nil)
}

// insertObligations writes obligations of the contract, each with its
// number, order item, what it promises, order value, timing, stand-alone
// selling price and, once the contract is active, allocated amount.
func insertObligations(ctx context.Context, tx pgx.Tx, tenant, contract uuid.UUID, obligations []Obligation) error {
	if len(obligations) == 0 {
		return nil
	}
	n := len(obligations)
	numbers, items, keys, descriptions := make([]int32, n), make([]int32, n), make([]*string, n), make([]string, n)
	nets, timings, prices, allocated := make([]int64, n), make([]string, n), make([]int64, n), make([]*int64, n)
	for i, o := range obligations {
		numbers[i], items[i], keys[i], descriptions[i], nets[i], timings[i], prices[i] = o.No, o.OrderItemNo, o.ItemKey, o.Description, int64(o.Net),
			o.Timing, int64(o.StandalonePrice)
		if o.Allocated != nil {
			a := int64(*o.Allocated)
			allocated[i] = &a
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, item_key, description,
			net_amount, timing, standalone_selling_price, allocated_amount)
		SELECT $1, $2, i.no, i.item_no, i.item_key, i.description, i.net_amount, i.timing, i.price, i.allocated
		FROM unnest($3::int[], $4::int[], $5::text[], $6::text[], $7::bigint[], $8::text[], $9::bigint[], $10::bigint[])
			AS i (no, item_no, item_key, description, net_amount, timing, price, allocated)`,
		tenant, contract, numbers, items, keys, descriptions, nets, timings, prices, allocated); err != nil {
		return fmt.Errorf("revenue: insert the obligations of contract %s: %w", contract, err)
	}
	return nil
}
