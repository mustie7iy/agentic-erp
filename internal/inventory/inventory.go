// Package inventory is the inventory management context (ADR-0082 §1-§3,
// §7; 02-architecture §2.2, 03-data-model §3.35): goods movements of
// materials at the storage locations of plants, each posted as a material
// document numbered from its company's material_document range, their
// reversal by another document, and the stock per material, plant and
// storage location now or as at a date (S1c).
//
// Stock is the sum of its movements and never below zero: a movement
// adds to its stock row in the database (inventory.movement_stock), and a
// posting locks every stock row it moves, in one order, and checks the
// quantity left before it writes the movements. Nothing is posted to the
// ledger yet; valuation follows (S1d).
//
// The context reads the materials context's published API
// (materials.Materials), tenancy's companies and org units, the ledger's
// cost centers and fiscal calendar and the configuration's number ranges
// and company dates through the ports below, which the composition root
// wires; procurement will post its goods receipts through it (ADR-0082
// §2-§3, S1e).
package inventory

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
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
}

// movementTypes are the forward movement types a person posts and their
// reversals, which only reverseMaterialDocument posts.
var movementTypes = map[string]movementType{
	"501": {name: "Receipt without purchase order", sign: 1, reversal: "502"},
	"502": {name: "Reversal of a receipt without purchase order", sign: -1, reverses: "501"},
	"561": {name: "Initial entry of stock", sign: 1, reversal: "562"},
	"562": {name: "Reversal of an initial entry of stock", sign: -1, reverses: "561"},
	"201": {name: "Issue to a cost center", sign: -1, costCenter: true, reversal: "202"},
	"202": {name: "Reversal of an issue to a cost center", sign: 1, costCenter: true, reverses: "201"},
	"551": {name: "Scrapping", sign: -1, costCenter: true, reversal: "552"},
	"552": {name: "Reversal of a scrapping", sign: 1, costCenter: true, reverses: "551"},
	"311": {name: "Transfer between storage locations", sign: -1, transfer: betweenStorageLocations, reversal: "312"},
	"312": {name: "Reversal of a transfer between storage locations", sign: 1, transfer: betweenStorageLocations, reverses: "311"},
	"301": {name: "Transfer between plants", sign: -1, transfer: betweenPlants, reversal: "302"},
	"302": {name: "Reversal of a transfer between plants", sign: 1, transfer: betweenPlants, reverses: "301"},
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
}

// Calendar maps a company's date to its fiscal year (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's time zone
// (configuration context).
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
	Lines            []Line
}
