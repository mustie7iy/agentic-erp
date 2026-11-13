// Package revenue is the revenue recognition context (ADR-0068;
// 02-architecture §2.2, 03-data-model §3.30): the revenue contract of a
// confirmed sales order, with one performance obligation per order item.
// A contract's transaction price is the order's net value, without tax,
// in the company's local currency; activating the contract allocates it
// to the obligations in proportion to their stand-alone selling prices
// (allocation.go).
//
// The person makes every judgement and the system only does the
// arithmetic (ADR-0068 §2): what is a distinct obligation, whether it is
// satisfied at a point in time or over time and what its stand-alone
// selling price is are a person's to decide. A contract starts from the
// order as it is, one obligation per item, satisfied at a point in time,
// priced at the item's net value, and nothing here judges whether that is
// right.
//
// Every confirmed sales order gets its contract by itself: Consumer reads
// the sales context's SalesOrderChanged from the event bus and creates
// it, once per order (ADR-0068 §4; ADR-0007). A person creates the same
// contract on request for a confirmed order that has none yet.
//
// An active contract's recognition schedule is computed on read from its
// obligations and its order's delivery confirmations (schedule.go;
// ADR-0068 §8).
//
// The context is a leaf (ADR-0068 §1; ADR-0051 §2): it reads the sales
// context's orders, the partner context's names, tenancy, the ledger's
// calendar and the configuration through the ports below, posts nothing
// yet, and no context imports it.
//
// What the context takes IFRS 15 to say is ADR-0068 §2's six statements,
// each from memory and not yet confirmed by the accounting specialist.
package revenue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Draft is the status of a revenue contract when it is created
// (ADR-0068 §3).
const Draft = "draft"

// PointInTime is the timing every performance obligation starts with: it
// is satisfied at a point in time until a person says otherwise
// (ADR-0068 §2).
const PointInTime = "point_in_time"

// SalesOrders reads a sales order with its items (sales context,
// ADR-0067 §5). Company access is the caller's to check.
type SalesOrders interface {
	Order(ctx context.Context, id uuid.UUID) (sales.Order, bool, error)
}

// LegalEntities is what revenue reads about legal entities (tenancy
// context): a company's code, name and local currency.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// Calendar gives a company's regular fiscal periods (ledger context):
// the one a day falls in and a fiscal year's, in order. A day or a year
// that is not set up is the ledger's 422 ledger.no_fiscal_period.
type Calendar interface {
	Period(ctx context.Context, legalEntity uuid.UUID, date time.Time) (ledger.FiscalPeriod, error)
	Periods(ctx context.Context, legalEntity uuid.UUID, year int16) ([]ledger.FiscalPeriod, error)
}

// Settings reads the active configuration: a company's time zone
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the revenue part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	SalesOrders   SalesOrders
	LegalEntities LegalEntities
	Partners      Partners
	Calendar      Calendar
	Settings      Settings
	// Now is the clock a schedule's day is told by, in the company's time
	// zone; nil: the wall clock.
	Now func() time.Time
}

// Obligation is a row of revenue.performance_obligation: what one item
// of the contract's sales order promises the customer. Net is the order
// item's net value, StandalonePrice the obligation's stand-alone selling
// price and Allocated its share of the contract's transaction price, nil
// until the contract is activated: minor units of the contract's
// currency. ServiceFrom and ServiceTo are the period an over-time
// obligation is satisfied over, nil for a point-in-time obligation.
// SatisfiedOn is the day a person says a point-in-time obligation is
// satisfied, nil when its order item's delivery confirmations say it
// (ADR-0068 §8).
type Obligation struct {
	No              int32        `json:"obligation_no"`
	OrderItemNo     int32        `json:"order_item_no"`
	ItemKey         *string      `json:"item_key"`
	Description     string       `json:"description"`
	Net             money.Minor  `json:"net_amount"`
	Timing          string       `json:"timing"`
	ServiceFrom     *time.Time   `json:"service_from"`
	ServiceTo       *time.Time   `json:"service_to"`
	SatisfiedOn     *time.Time   `json:"satisfied_on"`
	StandalonePrice money.Minor  `json:"standalone_selling_price"`
	Allocated       *money.Minor `json:"allocated_amount"`
}

// record adds the audit record of a contract's change and publishes
// RevenueContractChanged in the same transaction (ADR-0007, ADR-0011);
// before is its status before, nil when it had none, and source says what
// made the change: a person's request or the order's confirmation. The
// event names the customer by id and number, never by name (product
// owner, 2026-11-05), and carries the transaction price, not the
// obligations.
func record(ctx context.Context, action, source string, c Contract, le tenancy.Entity, customerNumber string, before *string) error {
	price := money.EventAmount(c.TransactionPrice, c.Currency, c.Exponent)
	payload := map[string]any{"revenue_contract_id": c.ID, "sales_order_id": c.SalesOrderID, "sales_order_number": c.SalesOrderNumber,
		"legal_entity_id": c.LegalEntityID, "legal_entity_code": le.Code, "customer_id": c.CustomerID, "customer_number": customerNumber,
		"transaction_price": price, "obligation_count": len(c.Obligations), "status": c.Status, "status_before": before, "version": c.Version}
	detail := map[string]any{"sales_order_id": c.SalesOrderID, "sales_order_number": c.SalesOrderNumber, "customer_id": c.CustomerID,
		"transaction_price": price, "obligations": len(c.Obligations), "status": c.Status, "status_before": before, "source": source}
	if err := audit.Record(ctx, action, "revenue.contract", c.ID, detail,
		outbox.Event{Context: "revenue", Name: "revenue_contract_changed", AggregateType: "revenue.contract", AggregateID: c.ID, Payload: payload}); err != nil {
		return fmt.Errorf("revenue: record %s of contract %s: %w", action, c.ID, err)
	}
	return nil
}
