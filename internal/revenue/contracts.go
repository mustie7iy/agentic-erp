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
// currency, whose minor-unit digits are Exponent.
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
				'standalone_selling_price', o.standalone_selling_price, 'allocated_amount', o.allocated_amount) ORDER BY o.obligation_no)
			FROM revenue.performance_obligation o WHERE o.tenant_id = k.tenant_id AND o.contract_id = k.id), '[]'),
		k.version, k.created_at, k.created_by, k.updated_at, k.updated_by
	FROM revenue.contract k JOIN ref.currency c ON c.code = k.currency`

func scanContract(row pgx.Row) (Contract, error) {
	var c Contract
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.CustomerID, &c.SalesOrderID, &c.SalesOrderNumber, &c.Currency, &c.Exponent,
		&c.TransactionPrice, &c.Status, &c.Obligations, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
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

// basisOf reads the order a contract is asked for and its company, or
// says why no contract is made for it: an order that is not there or of a
// company allowed does not allow, one that is not confirmed
// (revenue.order_not_confirmed) and one in another currency than its
// company's local currency (revenue.foreign_currency).
func (e *Endpoints) basisOf(ctx context.Context, orderID uuid.UUID, allowed func(company uuid.UUID) bool) (basis, *declined, error) {
	o, ok, err := e.SalesOrders.Order(ctx, orderID)
	if err != nil {
		return basis{}, nil, fmt.Errorf("revenue: sales order %s of a contract: %w", orderID, err)
	}
	if !ok || !allowed(o.LegalEntityID) {
		return basis{}, &declined{detail: "no sales order with this id that you can access"}, nil
	}
	if o.Status != sales.Confirmed {
		return basis{}, &declined{code: notConfirmed,
			detail: "Sales order " + o.DocumentNumber + " is " + o.Status + "; a revenue contract is created for a confirmed order."}, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, o.LegalEntityID) // a stored order's legal entity exists
	if err != nil {
		return basis{}, nil, fmt.Errorf("revenue: company of sales order %s: %w", orderID, err)
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
	numbers, keys, descriptions, nets := make([]int32, len(o.Items)), make([]*string, len(o.Items)), make([]string, len(o.Items)), make([]int64, len(o.Items))
	for i, it := range o.Items {
		numbers[i], keys[i], descriptions[i], nets[i] = it.ItemNo, it.ItemKey, it.Description, int64(it.Net)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, item_key, description,
			net_amount, timing, standalone_selling_price)
		SELECT $1, $2, i.ordinality, i.item_no, i.item_key, i.description, i.net_amount, $7, i.net_amount
		FROM unnest($3::int[], $4::text[], $5::text[], $6::bigint[]) WITH ORDINALITY AS i (item_no, item_key, description, net_amount, ordinality)`,
		p.TenantID, id, numbers, keys, descriptions, nets, PointInTime); err != nil {
		return Contract{}, false, fmt.Errorf("revenue: insert the obligations of contract %s: %w", id, err)
	}
	c, err := load(ctx, tx, id)
	if err != nil {
		return Contract{}, false, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{c.CustomerID})
	if err != nil {
		return Contract{}, false, fmt.Errorf("revenue: customer of contract %s: %w", id, err)
	}
	return c, true, record(ctx, "revenue.contract.create", source, c, b.le, names[c.CustomerID].Number, nil)
}
