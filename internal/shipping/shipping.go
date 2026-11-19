// Package shipping is the shipping context (ADR-0083; 02-architecture
// §2.2, 03-data-model §3.36): outbound deliveries of sales orders (SAP
// LIKP and LIPS), each of one confirmed order's items of materials kept in
// stock from one plant's storage location, numbered from its company's
// outbound_delivery range (create.go, S2a); their picking, up to what the
// location holds (pick.go, S2a); and their goods issue (S2b1).
//
// The context is a leaf (ADR-0083 §1). It reads the sales order through
// sales' published read (sales.Orders) and has sales hold the quantities
// a delivery takes on the order (sales' Ship), so an order change is
// judged on them; it reads materials through materials' published API,
// the stock a pick checks and the goods issue through inventory's
// (inventory.Endpoints.Held, IssueForDelivery),
// tenancy's companies and org units, the ledger's fiscal calendar and the
// configuration's number ranges and company dates, through the ports
// below, which the composition root wires.
package shipping

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// NumberRange is the document type outbound deliveries draw their
// numbers from (configuration number_ranges, ADR-0004).
const NumberRange = "outbound_delivery"

// Statuses of a delivery.
const (
	Created      = "created"
	GoodsIssued  = "goods_issued"
	NotPicked    = "not_picked"
	PartlyPicked = "partly_picked"
	Picked       = "picked"
)

// Org unit kinds a delivery ships from (tenancy.OrgUnitKinds).
const (
	kindPlant           = "plant"
	kindStorageLocation = "storage_location"
)

// Orders reads a sales order with its items (sales.Orders).
type Orders interface {
	Order(ctx context.Context, id uuid.UUID) (sales.Order, bool, error)
}

// Shipments has sales hold what a delivery takes of an order and record
// its goods issue on the order (sales' Endpoints.Ship and Issue,
// ADR-0083 §4-§5).
type Shipments interface {
	Ship(ctx context.Context, in sales.ShipInput) (sales.Shipped, error)
	Issue(ctx context.Context, in sales.GoodsIssue) (sales.Delivery, error)
}

// Stock reads what a storage location holds of a material and posts a
// delivery's goods issue (inventory's Endpoints.Held and
// IssueForDelivery).
type Stock interface {
	Held(ctx context.Context, material, plant, storageLocation uuid.UUID) (string, error)
	IssueForDelivery(ctx context.Context, reference string, in []inventory.DeliveryItem) (inventory.Document, error)
}

// Materials reads materials by id (the materials context's published API).
type Materials interface {
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
}

// Partners names customers (partner.Payees): a delivery shows its
// customer's name as it is now, as a sales document does.
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// LegalEntities reads tenancy's companies and org units.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps a company's date to its fiscal year (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's time zone.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the shipping part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	Orders        Orders
	Shipments     Shipments
	Stock         Stock
	Materials     Materials
	Partners      Partners
	LegalEntities LegalEntities
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	// Now is the clock a delivery is dated by, in its company's time
	// zone; nil: the wall clock.
	Now func() time.Time
}

// Item is an item of a delivery: a quantity of the order item OrderItemNo
// of a material, in the order item's unit, and what is picked of it.
type Item struct {
	ItemNo      int32     `json:"item_no"`
	OrderItemNo int32     `json:"order_item_no"`
	MaterialID  uuid.UUID `json:"material_id"`
	Description string    `json:"description"`
	Quantity    string    `json:"quantity"`
	Unit        string    `json:"unit"`
	Picked      string    `json:"picked_quantity"`
	// Cost is what its goods issue took from the stock, minor units of
	// the delivery's Currency; nil before.
	Cost *money.Minor `json:"cost_amount"`
}

// Delivery is a row of shipping.outbound_delivery with its items by item
// number. Once its goods issue is posted it names the day and the
// material document.
type Delivery struct {
	ID                     uuid.UUID
	LegalEntityID          uuid.UUID
	FiscalYear             int16
	Number                 string
	OrderID                uuid.UUID
	OrderNumber            string
	CustomerID             uuid.UUID
	CustomerNumber         string
	PlantID                uuid.UUID
	StorageLocationID      uuid.UUID
	Status                 string
	GoodsIssueDate         *time.Time
	MaterialDocumentID     *uuid.UUID
	MaterialDocumentNumber *string
	// Currency and Exponent are its items' costs', the company's; ""
	// before its goods issue.
	Currency  string
	Exponent  int
	Items     []Item
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}
