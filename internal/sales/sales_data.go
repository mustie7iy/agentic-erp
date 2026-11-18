package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	// SetUpSalesData inserts without reading first; the unique key is the
	// rule, and two set-ups racing end with one and this answer.
	problem.RegisterConstraint("customer_sales_data_once", func() *problem.Problem {
		return problem.Conflict("sales.customer_sales_data_exists",
			"The customer already has sales data in this company; change that one (updateCustomerSalesData).")
	})
}

// SalesData is a row of sales.customer_sales_data (ADR-0067 §13): a
// customer's sales data in a company, its customer price group of the
// configuration, nil for none. CustomerNumber is the customer's number
// when it was set up, which the list sorts by.
type SalesData struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	PriceGroup     *string
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const salesDataSelect = `SELECT d.id, d.legal_entity_id, d.customer_id, d.customer_number, d.price_group, d.version, d.created_at, d.created_by,
		d.updated_at, d.updated_by
	FROM sales.customer_sales_data d`

func scanSalesData(row pgx.Row) (SalesData, error) {
	var d SalesData
	err := row.Scan(&d.ID, &d.LegalEntityID, &d.CustomerID, &d.CustomerNumber, &d.PriceGroup, &d.Version, &d.CreatedAt, &d.CreatedBy,
		&d.UpdatedAt, &d.UpdatedBy)
	return d, errs.Wrapf(err, "scan customer sales data")
}

// salesDataStore reads a customer's sales data by id for a caller
// (stored.read).
var salesDataStore = stored[SalesData]{what: "customer sales data", table: "customer_sales_data", query: salesDataSelect + ` WHERE d.id = $1`,
	scan: scanSalesData, company: func(d SalesData) uuid.UUID { return d.LegalEntityID }}

// GetSalesData returns a customer's sales data in a company the caller
// may act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetSalesData(ctx context.Context, id uuid.UUID) (SalesData, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return SalesData{}, err
	}
	return salesDataStore.read(ctx, tx, id, false)
}

// SetUpSalesData sets up a customer's sales data in a company (ADR-0067
// §13): the customer is checked as a price record's is (customer), the
// price group, when given, is one of the configuration's (priceGroup).
// A customer's second sales data in a company is refused by the database,
// 409 sales.customer_sales_data_exists. It writes the audit record and
// CustomerSalesDataChanged.
func (e *Endpoints) SetUpSalesData(ctx context.Context, legalEntity, customer uuid.UUID, group *string) (SalesData, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SalesData{}, err
	}
	le, err := e.company(ctx, legalEntity)
	if err != nil {
		return SalesData{}, err
	}
	named, err := e.customer(ctx, le, customer)
	if err != nil {
		return SalesData{}, err
	}
	if err := e.priceGroup(ctx, group); err != nil {
		return SalesData{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.customer_sales_data (tenant_id, id, legal_entity_id, customer_id, customer_number, price_group,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, p.TenantID, id, le.ID, customer, named.Number, group, p.ID); err != nil {
		return SalesData{}, fmt.Errorf("sales: insert the sales data of customer %s in company %s: %w", named.Number, le.Code, err)
	}
	d, err := salesDataStore.read(ctx, tx, id, false)
	if err != nil {
		return SalesData{}, err
	}
	return d, recordSalesData(ctx, "sales.customer_sales_data.create", d, le, nil)
}

// ChangeSalesData changes a customer's price group with the caller's
// version of its sales data (ADR-0008): the row is locked, the version
// compared, and the change recorded with the group before and after. A
// group is one of the configuration's or nil for none; the one the sales
// data has is refused (422 sales.nothing_to_change).
func (e *Endpoints) ChangeSalesData(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, group *string) (SalesData, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SalesData{}, err
	}
	d, err := salesDataStore.read(ctx, tx, id, true)
	if err != nil {
		return SalesData{}, err
	}
	if err := ifMatch(d.Version); err != nil {
		return SalesData{}, err
	}
	if err := e.priceGroup(ctx, group); err != nil {
		return SalesData{}, err
	}
	if apitypes.SamePtr(d.PriceGroup, group) {
		return SalesData{}, problem.Unprocessable("sales.nothing_to_change", "The change would leave the customer's sales data as it is.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, d.LegalEntityID) // a stored sales data's legal entity exists
	if err != nil {
		return SalesData{}, fmt.Errorf("sales: company of customer sales data %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.customer_sales_data SET price_group = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, group, p.ID); err != nil {
		return SalesData{}, fmt.Errorf("sales: change customer sales data %s: %w", id, err)
	}
	after, err := salesDataStore.read(ctx, tx, id, false)
	if err != nil {
		return SalesData{}, err
	}
	return after, recordSalesData(ctx, "sales.customer_sales_data.change", after, le, d.PriceGroup)
}

// priceGroup refuses a price group the active configuration does not
// have (422 sales.unknown_price_group naming price_group); nil is none.
func (e *Endpoints) priceGroup(ctx context.Context, group *string) error {
	if group == nil {
		return nil
	}
	cfg, _, err := e.Settings.Document(ctx)
	if err != nil {
		return fmt.Errorf("sales: price group %s: %w", *group, err)
	}
	if _, ok := cfg.Pricing.PriceGroup(*group); !ok {
		return problem.Invalid("sales.unknown_price_group", "price_group", "Price group "+*group+" is not one of the configuration's (pricing.price_groups).")
	}
	return nil
}

// customerGroup is the price group of the customer's sales data in the
// company, nil when it has no sales data there or no group (ADR-0067
// §13): the group whose records its items take before the general ones.
func customerGroup(ctx context.Context, tx pgx.Tx, le, customer uuid.UUID) (*string, error) {
	var group *string
	err := tx.QueryRow(ctx, `SELECT price_group FROM sales.customer_sales_data WHERE legal_entity_id = $1 AND customer_id = $2`, le, customer).Scan(&group)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sales: price group of customer %s: %w", customer, err)
	}
	return group, nil
}

// recordSalesData adds the audit record of sales data set up or changed
// and publishes CustomerSalesDataChanged in the same transaction
// (ADR-0007, ADR-0011), both with the price group after and before;
// before is nil for sales data just set up. The event names the customer
// by id and number, never by name (product owner, 2026-11-05).
func recordSalesData(ctx context.Context, action string, d SalesData, le tenancy.Entity, before *string) error {
	detail := map[string]any{"legal_entity_id": d.LegalEntityID, "customer_id": d.CustomerID, "customer_number": d.CustomerNumber,
		"price_group": d.PriceGroup, "price_group_before": before}
	payload := map[string]any{"customer_sales_data_id": d.ID, "legal_entity_id": d.LegalEntityID, "legal_entity_code": le.Code,
		"customer_id": d.CustomerID, "customer_number": d.CustomerNumber, "price_group": d.PriceGroup, "price_group_before": before, "version": d.Version}
	return record(ctx, action, "customer_sales_data", "customer_sales_data_changed", d.ID, detail, payload)
}
