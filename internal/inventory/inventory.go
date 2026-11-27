// Package inventory is the inventory management context (ADR-0082 §1-§3,
// §7-§8; 02-architecture §2.2, 03-data-model §3.35): goods movements of
// materials at the storage locations of plants, each posted as a material
// document numbered from its company's material_document range, their
// reversal by another document, and the stock per material, plant and
// storage location now or as at a date (S1c); the valuation of materials
// per plant at a standard or moving price under a valuation class, every
// movement's value posted through the ledger's posting port by the
// accounts of its class, and price changes that revalue the stock (S1d).
//
// Stock is the sum of its movements and never below zero, and a
// valuation's quantity and value the sum of its movements and price
// changes: a movement adds to its stock row and its valuation in the
// database (inventory.movement_stock). A posting locks the valuations and
// stock rows it moves, each in one order, checks and values its lines in
// the order the database applies them, then posts one journal entry
// (journal.go) and writes the lines.
//
// The context reads the materials context's published API
// (materials.Materials), tenancy's companies and org units, the ledger's
// cost centers, fiscal calendar and account determination and the
// configuration's number ranges, company dates and valuation classes, and
// posts through the ledger's posting port, through the ports below, which
// the composition root wires. Procurement posts its goods receipts of
// stock items and their reversals through it (ReceiveForOrder,
// ReverseForOrder), their value on the GR/IR entry procurement posts, and
// payables the price differences of their invoices on a moving-priced
// stock (InvoiceDifferences; ADR-0082 §2-§3 and As built S1e). Sales'
// return receipts post a customer's return into stock through it
// (ReceiveForReturn; ADR-0083 As built S2b4). Stock transport orders
// move stock between a company's plants through it, the goods issue into
// stock in transit at the receiving plant and the goods receipt out of it
// (IssueForTransport, ReceiveForTransport; As built S2h).
package inventory

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// NumberRange is the document type material documents draw their numbers
// from (configuration number_ranges, ADR-0004).
const NumberRange = "material_document"

// Transfers: what a transfer posting moves the stock between.
const (
	noTransfer = iota
	betweenStorageLocations
	betweenPlants
)

// movementType is one of SAP's movement types the context posts (ADR-0082
// §7, from memory): its name, the sign of the stock it moves (a transfer
// issues from one location and receives at another), whether an item
// names a cost center, and its reversal's type, or the type it reverses.
type movementType struct {
	name       string
	sign       int
	transfer   int
	costCenter bool
	reversal   string
	reverses   string
	// order: a purchase order's goods receipt or its reversal, which
	// procurement's goods movement posts (S1e), never a person here.
	order bool
	// count: a physical inventory count's difference, which posting the
	// count's differences posts (S2d), never a person here, and which is
	// not reversed: a new count corrects it.
	count bool
	// delivery: an outbound delivery's goods issue, which shipping posts
	// (IssueForDelivery, ADR-0083 §5), never a person here.
	delivery bool
	// transport: a stock transport order's goods issue into stock in
	// transit, which procurement posts (IssueForTransport, S2h), never a
	// person here, and which is not reversed yet.
	transport bool
	// intercompany: an intercompany stock transport order's goods issue
	// out of the supplying company's stock, which procurement posts
	// (IssueForIntercompany, S2i1), never a person here, and which is not
	// reversed yet.
	intercompany bool
	// customerReturn: a customer's return received into stock, which a
	// return receipt of sales posts (ReceiveForReturn, S2b4), never a
	// person here, and which is not reversed yet.
	customerReturn bool
	// tracked: an item of a batch-managed material names its batch
	// (S2f), and one of a serial-managed material the serial number of
	// each unit (S2g); a purchase order's goods receipt and its reversal
	// carry both since S2f1 and S2g1, a delivery's goods issue and its
	// reversal since S2f2 and S2g2, a customer's return since S2b4; the
	// other movement types carry neither yet, and refuse such a material.
	tracked bool
}

// movementTypes are the forward movement types a person posts and their
// reversals, which only reverseMaterialDocument posts, a purchase
// order's goods receipt and its reversal, which procurement posts
// (ReceiveForOrder, ReverseForOrder), a count's differences, which
// PostDifferences posts (S2d), and an outbound delivery's goods issue,
// which shipping posts (IssueForDelivery; its reversal, 602, through
// ReverseForDelivery, S2b3), and a
// stock transport order's goods issue, which procurement posts
// (IssueForTransport, S2h; its receipt is a 101 of its own kind,
// transportReceipt), and an intercompany stock transport order's goods
// issue, which procurement posts (IssueForIntercompany, S2i1; the
// receiving company receives it as a purchase order's 101), and a
// customer's return received into stock, which sales' return receipt
// posts (ReceiveForReturn, S2b4).
var movementTypes = map[string]movementType{
	"101": {name: "Goods receipt for a purchase order", sign: 1, reversal: "102", order: true, tracked: true},
	"102": {name: "Reversal of a goods receipt for a purchase order", sign: -1, reverses: "101", order: true, tracked: true},
	"501": {name: "Receipt without purchase order", sign: 1, reversal: "502", tracked: true},
	"502": {name: "Reversal of a receipt without purchase order", sign: -1, reverses: "501", tracked: true},
	"561": {name: "Initial entry of stock", sign: 1, reversal: "562", tracked: true},
	"562": {name: "Reversal of an initial entry of stock", sign: -1, reverses: "561", tracked: true},
	"201": {name: "Issue to a cost center", sign: -1, costCenter: true, reversal: "202", tracked: true},
	"202": {name: "Reversal of an issue to a cost center", sign: 1, costCenter: true, reverses: "201", tracked: true},
	"551": {name: "Scrapping", sign: -1, costCenter: true, reversal: "552", tracked: true},
	"552": {name: "Reversal of a scrapping", sign: 1, costCenter: true, reverses: "551", tracked: true},
	"311": {name: "Transfer between storage locations", sign: -1, transfer: betweenStorageLocations, reversal: "312", tracked: true},
	"312": {name: "Reversal of a transfer between storage locations", sign: 1, transfer: betweenStorageLocations, reverses: "311", tracked: true},
	"301": {name: "Transfer between plants", sign: -1, transfer: betweenPlants, reversal: "302", tracked: true},
	"302": {name: "Reversal of a transfer between plants", sign: 1, transfer: betweenPlants, reverses: "301", tracked: true},
	"351": {name: "Goods issue for a stock transport order", sign: -1, transfer: betweenPlants, transport: true},
	"601": {name: "Goods issue for a delivery", sign: -1, reversal: "602", delivery: true, tracked: true},
	"602": {name: "Reversal of a goods issue for a delivery", sign: 1, reverses: "601", delivery: true, tracked: true},
	"643": {name: "Goods issue for an intercompany stock transport order", sign: -1, intercompany: true},
	"653": {name: "Customer return into stock", sign: 1, customerReturn: true, tracked: true},
	"701": {name: "Physical inventory surplus", sign: 1, count: true},
	"702": {name: "Physical inventory shortage", sign: -1, count: true},
}

func init() {
	// takeStock refuses a shortage first, naming it; this is the
	// database's answer should one pass it.
	problem.RegisterConstraint("stock_not_negative", func() *problem.Problem {
		return problem.Conflict("inventory.insufficient_stock", "A goods movement would take a location's stock below zero.")
	})
}

// Org unit kinds goods move between (tenancy.OrgUnitKinds).
const (
	kindPlant           = "plant"
	kindStorageLocation = "storage_location"
)

// Materials reads materials by id (the materials context's published API).
type Materials interface {
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
}

// LegalEntities reads tenancy's legal entities and org units: a company's
// code, and a plant's or storage location's kind, company, parent, code,
// name and status.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
}

// CostCenters reads the ledger's cost centers: their companies, codes and
// whether they are active, and whether one is in force on a day.
type CostCenters interface {
	CostCenterRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CostCenterRef, error)
	CostCenterValidOn(ctx context.Context, id uuid.UUID, day time.Time) (bool, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
	Lock(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) error
}

// Calendar maps a company's date to its fiscal year (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Postings posts journal entries and says whether a company can post on
// a day (ledger context).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
	Postable(ctx context.Context, legalEntity uuid.UUID, date time.Time) error
}

// Accounts reads the account determination (ledger context): the
// accounts of valuation classes by config.InventoryKey.
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
}

// People names principals by id (the identity context, through the app's
// adapter): a count names who entered it and who sent it for a recount,
// as they were named then (S2d2). An id of none is left out.
type People interface {
	PersonNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Settings reads the active configuration: a company's time zone and
// valuation classes (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the inventory part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	Materials     Materials
	LegalEntities LegalEntities
	CostCenters   CostCenters
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Postings      Postings
	Accounts      Accounts
	// Requests opens the approval requests of count differences above a
	// company's tolerance (S2d).
	Requests Opener
	// People names who counts and who sends an item for a recount (S2d2).
	People People
	// Now is the clock a document is dated by, in its company's time
	// zone; nil: the wall clock.
	Now func() time.Time
}

// Line is a movement of a material document: one stock effect, in the
// material's base unit and signed (a receipt positive, an issue
// negative), with the quantity and unit its item was entered in.
type Line struct {
	LineNo            int16      `json:"line_no"`
	ItemNo            int16      `json:"item_no"`
	MaterialID        uuid.UUID  `json:"material_id"`
	PlantID           uuid.UUID  `json:"plant_id"`
	StorageLocationID uuid.UUID  `json:"storage_location_id"`
	Quantity          string     `json:"quantity"`
	EntryQuantity     string     `json:"entry_quantity"`
	EntryUnit         string     `json:"entry_unit"`
	CostCenterID      *uuid.UUID `json:"cost_center_id"`
	// BatchID is the batch the line moves, of a batch-managed material
	// (S2f), and BatchNumber its number; nil on another material's line.
	BatchID     *uuid.UUID `json:"batch_id"`
	BatchNumber *string    `json:"batch_number"`
	// SerialNumbers are the serial numbers of the units the line moves, of
	// a serial-managed material (S2g), by number; nil on another
	// material's line.
	SerialNumbers []string `json:"serial_numbers"`
	// InTransit says the line moves stock in transit to its storage
	// location of its plant, the receiving one of a stock transport order
	// (S2h), rather than the stock at that location.
	InTransit bool `json:"in_transit"`
	// Amount is what the line moves in value, in the valuation's
	// currency, signed as its quantity is, and Difference what it is
	// valued at beyond that: its price difference (S1d).
	Amount     money.Minor `json:"amount"`
	Difference money.Minor `json:"difference"`
	// target is what the line is to be valued at, when that is given: a
	// reversal's line, its original's value negated.
	target *money.Minor
}

// Document is a material document with its movements in line order, and
// the documents it reverses and that reverse it, by id and number.
type Document struct {
	ID               uuid.UUID
	LegalEntityID    uuid.UUID
	FiscalYear       int16
	Number           string
	MovementType     string
	PostingDate      time.Time
	HeaderText       *string
	ReversalOfID     *uuid.UUID
	ReversalOfNumber *string
	ReversedByID     *uuid.UUID
	ReversedByNumber *string
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	// Reference is the goods movement's number of a purchase order's
	// goods receipt or its reversal (101, 102), nil on another document.
	Reference *string
	// Currency and Exponent are what its lines are valued in, the
	// company's; empty on a document posted before valuation (S1c). The
	// entry is the journal entry that posted its value, nil when none.
	Currency    string
	Exponent    int
	EntryID     *uuid.UUID
	EntryYear   *int16
	EntryNumber *string
	Lines       []Line
}
