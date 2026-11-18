package inventory

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ItemInput is an item of a goods movement as a request gives it: a
// quantity of a material at a plant's storage location, the cost center
// of an issue or a scrapping, and where a transfer receives it.
type ItemInput struct {
	MaterialID, PlantID, StorageLocationID uuid.UUID
	Quantity, Unit                         string
	CostCenterID                           *uuid.UUID
	ToPlantID, ToStorageLocationID         *uuid.UUID
}

// Input is a goods movement as a request posts it.
type Input struct {
	MovementType string
	HeaderText   *string
	Items        []ItemInput
}

// item is an item checked: the company of its plants and its lines.
type item struct {
	company uuid.UUID
	lines   []Line
}

// Post posts a goods movement as a material document (ADR-0082 §7): the
// movement type one a person posts, each item checked against the
// material, the plants and storage locations it names and the company
// they are of, the document dated the company's date today and numbered
// from its material_document range, the stock locked and refused below
// zero, the movements written, and the audit record and
// MaterialDocumentPosted in the request's transaction.
func (e *Endpoints) Post(ctx context.Context, in Input) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	mt, ok := movementTypes[in.MovementType]
	if !ok || mt.reverses != "" {
		return Document{}, problem.Invalid("inventory.unknown_movement_type", "movement_type", fmt.Sprintf("%q is not a movement type that is posted; "+
			"they are 501, 561, 201, 551, 311 and 301, and a reversal is posted by reversing its document.", in.MovementType))
	}
	r, err := e.inputRefs(ctx, in.Items)
	if err != nil {
		return Document{}, err
	}
	company, lines, err := e.items(in.Items, mt, r)
	if err != nil {
		return Document{}, err
	}
	le, day, err := e.today(ctx, company)
	if err != nil {
		return Document{}, err
	}
	if err := e.costCenters(ctx, in.Items, r, company, day.Date); err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: le, movementType: in.MovementType, day: day.Date, headerText: in.HeaderText, lines: lines}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": d.HeaderText != nil})
}

// today is the company and its date today in its time zone, which a
// document is posted on.
func (e *Endpoints) today(ctx context.Context, company uuid.UUID) (tenancy.Entity, config.CompanyDay, error) {
	le, _, err := e.LegalEntities.Entity(ctx, company) // a plant's company exists
	if err != nil {
		return tenancy.Entity{}, config.CompanyDay{}, fmt.Errorf("inventory: company %s: %w", company, err)
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return tenancy.Entity{}, config.CompanyDay{}, fmt.Errorf("inventory: today in company %s: %w", le.Code, err)
	}
	return le, day, nil
}

// inputRefs reads what a request's items name: their materials, plants
// and storage locations, the companies the caller may act in and the cost
// centers.
func (e *Endpoints) inputRefs(ctx context.Context, items []ItemInput) (refs, error) {
	var ms, units, centers []uuid.UUID
	for _, it := range items {
		ms, units = append(ms, it.MaterialID), append(units, it.PlantID, it.StorageLocationID)
		for _, id := range []*uuid.UUID{it.ToPlantID, it.ToStorageLocationID} {
			if id != nil {
				units = append(units, *id)
			}
		}
		if it.CostCenterID != nil {
			centers = append(centers, *it.CostCenterID)
		}
	}
	return e.read(ctx, ms, units, centers, nil)
}

// items checks a request's items and makes their lines, numbered in
// order, and answers the one company their plants are of.
func (e *Endpoints) items(in []ItemInput, mt movementType, r refs) (uuid.UUID, []Line, error) {
	var company uuid.UUID
	var lines []Line
	for i, it := range in {
		c, err := checkItem(i, it, mt, r)
		if err != nil {
			return uuid.Nil, nil, err
		}
		if company != uuid.Nil && c.company != company {
			return uuid.Nil, nil, problem.Invalid("inventory.mixed_companies", fmt.Sprintf("items[%d].plant_id", i),
				"A material document moves the stock of one company; this plant is of another than the items before it.")
		}
		company = c.company
		for _, l := range c.lines {
			l.LineNo, l.ItemNo = int16(len(lines)+1), int16(i+1) // at most 100 items of two lines (the contract's)
			lines = append(lines, l)
		}
	}
	return company, lines, nil
}

// checkItem checks one item: its material, the stock it moves and where a
// transfer receives it, and makes its lines.
func checkItem(i int, it ItemInput, mt movementType, r refs) (item, error) {
	m, err := usable(i, it.MaterialID, r)
	if err != nil {
		return item{}, err
	}
	at := fmt.Sprintf("items[%d].", i)
	company, err := location(at+"plant_id", at+"storage_location_id", m, it.PlantID, it.StorageLocationID, r)
	if err != nil {
		return item{}, err
	}
	base, err := baseQuantity(at+"quantity", m, it.Quantity, it.Unit)
	if err != nil {
		return item{}, err
	}
	if err := assignment(at, it, mt); err != nil {
		return item{}, err
	}
	line := Line{MaterialID: m.ID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, EntryQuantity: it.Quantity, EntryUnit: it.Unit,
		CostCenterID: it.CostCenterID}
	issued := new(big.Rat).Mul(base, big.NewRat(int64(mt.sign), 1))
	line.Quantity = money.PlainQuantity(issued)
	if mt.transfer == noTransfer {
		return item{company: company, lines: []Line{line}}, nil
	}
	to, err := target(at, it, mt, m, company, r)
	if err != nil {
		return item{}, err
	}
	to.EntryQuantity, to.EntryUnit, to.Quantity = it.Quantity, it.Unit, money.PlainQuantity(base)
	return item{company: company, lines: []Line{line, to}}, nil
}

// usable is the item's material: an active material kept in stock.
func usable(i int, id uuid.UUID, r refs) (materials.Material, error) {
	field := fmt.Sprintf("items[%d].material_id", i)
	m, ok := r.materials[id]
	switch {
	case !ok:
		return materials.Material{}, problem.Invalid("inventory.material_invalid", field, "No material with this id.")
	case m.Status != materials.Active:
		return materials.Material{}, problem.Invalid("inventory.material_not_usable", field, "Material "+m.Number+" is "+m.Status+
			"; only an active material is moved.")
	case !m.Stocked():
		return materials.Material{}, problem.Invalid("inventory.material_not_stocked", field, "Material "+m.Number+" is of type "+m.Type+
			", which is kept in no stock.")
	}
	return m, nil
}

// location checks a plant and a storage location the material is moved
// at: an active plant of a company the caller may act in that the
// material is extended to, and an active storage location under it the
// material is kept at. It answers the plant's company.
func location(plantField, locationField string, m materials.Material, plant, storageLocation uuid.UUID, r refs) (uuid.UUID, error) {
	u, ok := r.units[plant]
	switch {
	case !ok || u.Kind != kindPlant || u.Status != tenancy.Active || !r.access.Allows(u.LegalEntityID):
		return uuid.Nil, problem.Invalid("inventory.plant_invalid", plantField, "No active plant with this id in a company you can access.")
	case !hasPlant(m, plant):
		return uuid.Nil, problem.Invalid("inventory.plant_not_extended", plantField, "Material "+m.Number+" is not extended to plant "+u.Code+".")
	}
	l, ok := r.units[storageLocation]
	switch {
	case !ok || l.Kind != kindStorageLocation || l.Status != tenancy.Active || l.ParentID == nil || *l.ParentID != plant:
		return uuid.Nil, problem.Invalid("inventory.storage_location_invalid", locationField, "No active storage location with this id under plant "+
			u.Code+".")
	case !m.KeptAt(plant, storageLocation):
		return uuid.Nil, problem.Invalid("inventory.storage_location_not_extended", locationField, "Material "+m.Number+" is not kept at storage location "+
			l.Code+" of plant "+u.Code+".")
	}
	return u.LegalEntityID, nil
}

func hasPlant(m materials.Material, plant uuid.UUID) bool {
	_, ok := m.Plant(plant)
	return ok
}

// baseQuantity is an item's quantity in the material's base unit: a
// positive plain decimal in one of its units, converted exactly.
func baseQuantity(field string, m materials.Material, quantity, unit string) (*big.Rat, error) {
	if money.PositiveQuantity(quantity) != nil {
		return nil, problem.Invalid("inventory.invalid_quantity", field+".value", fmt.Sprintf("a positive plain decimal quantity of at most %d "+
			"integer digits and %d decimals", money.QuantityIntDigits, money.QuantityDecimals))
	}
	base, err := m.Convert(money.ParseRat(quantity), unit)
	switch {
	case errors.Is(err, materials.ErrUnknownUnit):
		return nil, problem.Invalid("inventory.unknown_unit", field+".unit", unit+" is neither the base unit "+m.BaseUnit+" of material "+m.Number+
			" nor one of its alternative units.")
	case err != nil:
		return nil, problem.Invalid("inventory.inexact_quantity", field+".value", quantity+" "+unit+" of material "+m.Number+
			" is no exact quantity of its base unit "+m.BaseUnit+" in six decimals; it is not rounded.")
	}
	return base, nil
}

// assignment checks that an item names a cost center exactly when its
// movement type takes one, and a receiving location only for a transfer.
func assignment(at string, it ItemInput, mt movementType) error {
	switch {
	case mt.costCenter && it.CostCenterID == nil:
		return problem.Invalid("inventory.cost_center_required", at+"cost_center_id", "An issue to a cost center and a scrapping name the cost center.")
	case !mt.costCenter && it.CostCenterID != nil:
		return problem.Invalid("inventory.cost_center_not_allowed", at+"cost_center_id", "Only an issue to a cost center and a scrapping name a cost center.")
	case mt.transfer != betweenPlants && it.ToPlantID != nil:
		return problem.Invalid("inventory.transfer_not_allowed", at+"to_plant_id", "Only a transfer between plants names the plant received at.")
	case mt.transfer == noTransfer && it.ToStorageLocationID != nil:
		return problem.Invalid("inventory.transfer_not_allowed", at+"to_storage_location_id", "Only a transfer names the storage location received at.")
	case mt.transfer != noTransfer && it.ToStorageLocationID == nil:
		return problem.Invalid("inventory.transfer_target_required", at+"to_storage_location_id", "A transfer names the storage location received at.")
	case mt.transfer == betweenPlants && it.ToPlantID == nil:
		return problem.Invalid("inventory.transfer_target_required", at+"to_plant_id", "A transfer between plants names the plant received at.")
	}
	return nil
}

// target is the receiving line of a transfer: at another storage location
// of the plant, or at a storage location of another plant of the company,
// checked as the issuing one is.
func target(at string, it ItemInput, mt movementType, m materials.Material, company uuid.UUID, r refs) (Line, error) {
	plant := it.PlantID
	if mt.transfer == betweenPlants {
		plant = *it.ToPlantID
	}
	to, err := location(at+"to_plant_id", at+"to_storage_location_id", m, plant, *it.ToStorageLocationID, r)
	switch {
	case err != nil: // a 311's plant is the issuing one's, checked already
		return Line{}, err
	case to != company:
		return Line{}, problem.Invalid("inventory.mixed_companies", at+"to_plant_id", "A transfer between plants stays in one company; "+
			"this plant is of another.")
	case mt.transfer == betweenPlants && plant == it.PlantID:
		return Line{}, problem.Invalid("inventory.same_location", at+"to_plant_id", "A transfer between plants receives at another plant.")
	case *it.ToStorageLocationID == it.StorageLocationID:
		return Line{}, problem.Invalid("inventory.same_location", at+"to_storage_location_id", "A transfer receives at another storage location.")
	}
	return Line{MaterialID: m.ID, PlantID: plant, StorageLocationID: *it.ToStorageLocationID}, nil
}

// costCenters checks the cost centers items name: each an active cost
// center of the document's company in force on the posting date.
func (e *Endpoints) costCenters(ctx context.Context, items []ItemInput, r refs, company uuid.UUID, day time.Time) error {
	for i, it := range items {
		if it.CostCenterID == nil {
			continue
		}
		field := fmt.Sprintf("items[%d].cost_center_id", i)
		c, ok := r.centers[*it.CostCenterID]
		if !ok || !c.Active || c.LegalEntityID != company {
			return problem.Invalid("inventory.cost_center_invalid", field, "No active cost center with this id in the document's company.")
		}
		valid, err := e.CostCenters.CostCenterValidOn(ctx, *it.CostCenterID, day)
		if err != nil {
			return fmt.Errorf("inventory: cost center %s on %s: %w", c.Code, day.Format(time.DateOnly), err)
		}
		if !valid {
			return problem.Invalid("inventory.cost_center_invalid", field, "Cost center "+c.Code+" is not in force on "+day.Format(time.DateOnly)+".")
		}
	}
	return nil
}

// posting is a material document about to be written.
type posting struct {
	company      tenancy.Entity
	movementType string
	day          time.Time
	headerText   *string
	reversalOf   *uuid.UUID
	lines        []Line
}

// write numbers the document from its company's material_document range
// of its posting date's fiscal year, writes it, locks the stock its lines
// move and refuses it below zero, and writes the lines, which add to the
// stock (inventory.movement_stock).
func (e *Endpoints) write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, r refs) (Document, error) {
	year, err := e.Calendar.FiscalYear(ctx, in.company.ID, in.day)
	if err != nil {
		return Document{}, fmt.Errorf("inventory: fiscal year of %s in company %s: %w", in.day.Format(time.DateOnly), in.company.Code, err)
	}
	number, err := e.Numbers.Next(ctx, in.company.ID, NumberRange, year)
	if err != nil {
		return Document{}, fmt.Errorf("inventory: number a material document in company %s: %w", in.company.Code, err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type,
			posting_date, header_text, reversal_of_id, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		p.TenantID, id, in.company.ID, year, number, in.movementType, in.day, in.headerText, in.reversalOf, p.ID); err != nil {
		return Document{}, fmt.Errorf("inventory: insert material document %s: %w", number, err)
	}
	if err := takeStock(ctx, tx, p, in.company.ID, in.lines, r); err != nil {
		return Document{}, err
	}
	if err := writeLines(ctx, tx, p, id, in.lines); err != nil {
		return Document{}, err
	}
	return load(ctx, tx, id)
}

// writeLines writes a document's movements in line order.
func writeLines(ctx context.Context, tx pgx.Tx, p reqctx.Principal, document uuid.UUID, lines []Line) error {
	n := len(lines)
	lineNo, itemNo := make([]int16, n), make([]int16, n)
	ms, plants, locations := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n)
	qty, entry, units, centers := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, l := range lines {
		lineNo[i], itemNo[i], ms[i], plants[i], locations[i] = l.LineNo, l.ItemNo, l.MaterialID, l.PlantID, l.StorageLocationID
		qty[i], entry[i], units[i] = l.Quantity, l.EntryQuantity, l.EntryUnit
		if l.CostCenterID != nil {
			centers[i] = l.CostCenterID.String()
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id,
			storage_location_id, quantity, entry_quantity, entry_uom_code, cost_center_id)
		SELECT $1, $2, x.l, x.i, x.m, x.p, x.s, x.q::numeric, x.e::numeric, x.u, nullif(x.c, '')::uuid
		FROM unnest($3::smallint[], $4::smallint[], $5::uuid[], $6::uuid[], $7::uuid[], $8::text[], $9::text[], $10::text[], $11::text[])
			AS x (l, i, m, p, s, q, e, u, c) ORDER BY x.l`,
		p.TenantID, document, lineNo, itemNo, ms, plants, locations, qty, entry, units, centers); err != nil {
		return fmt.Errorf("inventory: movements of material document %s: %w", document, err)
	}
	return nil
}

// access is the companies the caller may act in, as authz answers them.
func access(ctx context.Context) (authz.Access, error) {
	a, err := authz.Companies(ctx)
	if err != nil {
		return authz.Access{}, fmt.Errorf("inventory: company access: %w", err)
	}
	return a, nil
}
