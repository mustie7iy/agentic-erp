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
	// Amount is what a receipt is worth, in the company's currency; nil:
	// valued at the material's price (S1d).
	Amount *Amount
}

// Amount is an amount as a request gives it: a decimal and its currency.
type Amount struct{ Value, Currency string }

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
	if !ok || mt.reverses != "" || mt.order || mt.count {
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
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return Document{}, err
	}
	if err := e.costCenters(ctx, in.Items, r, company, c.day.Date); err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: c.le, movementType: in.MovementType, day: c.day.Date, headerText: in.HeaderText, lines: lines,
		items: in.Items}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": d.HeaderText != nil})
}

// companyOf is a company, its settings in the active configuration (nil
// when the configuration holds none of it: then it has no valuation
// classes) and its date today in its time zone, which documents and price
// changes are posted on.
type companyOf struct {
	le  tenancy.Entity
	co  *config.Company
	day config.CompanyDay
}

func (e *Endpoints) companyOf(ctx context.Context, id uuid.UUID) (companyOf, error) {
	le, _, err := e.LegalEntities.Entity(ctx, id) // a plant's, document's or valuation's company exists
	if err != nil {
		return companyOf{}, fmt.Errorf("inventory: company %s: %w", id, err)
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return companyOf{}, fmt.Errorf("inventory: today in company %s: %w", le.Code, err)
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return companyOf{}, fmt.Errorf("inventory: configuration of company %s: %w", le.Code, err)
	}
	return companyOf{le: le, co: doc.Company(le.Code), day: day}, nil
}

// unknownClass refuses a valuation class the company lacks.
func unknownClass(company, class string) error {
	return problem.Invalid("inventory.unknown_valuation_class", "valuation_class", "Company "+company+" has no valuation class "+class+
		" (companies[].account_determination.inventory).")
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
// at: the plant as plantAt checks it, and an active storage location
// under it the material is kept at. It answers the plant's company.
func location(plantField, locationField string, m materials.Material, plant, storageLocation uuid.UUID, r refs) (uuid.UUID, error) {
	u, err := plantAt(plantField, m, plant, r)
	if err != nil {
		return uuid.Nil, err
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

// plantAt checks a plant a material is moved or valued at: an active
// plant of a company the caller may act in that the material is extended
// to.
func plantAt(field string, m materials.Material, plant uuid.UUID, r refs) (tenancy.Unit, error) {
	u, ok := r.units[plant]
	switch {
	case !ok || u.Kind != kindPlant || u.Status != tenancy.Active || !r.access.Allows(u.LegalEntityID):
		return tenancy.Unit{}, problem.Invalid("inventory.plant_invalid", field, "No active plant with this id in a company you can access.")
	case !hasPlant(m, plant):
		return tenancy.Unit{}, problem.Invalid("inventory.plant_not_extended", field, "Material "+m.Number+" is not extended to plant "+u.Code+".")
	}
	return u, nil
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
	return converted(field, m, money.ParseRat(quantity), quantity, unit)
}

// converted is a quantity q of a material, entered as quantity in unit,
// converted exactly to its base unit (ADR-0082 §5). Zero, a count of
// nothing (S2c), is zero in every unit of the material; Convert takes
// positive quantities only.
func converted(field string, m materials.Material, q *big.Rat, quantity, unit string) (*big.Rat, error) {
	base, err := m.Convert(q, unit)
	if q.Sign() == 0 {
		base, err = q, nil
		if _, ferr := m.Factor(unit); ferr != nil {
			err = ferr
		}
	}
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

// posting is a material document about to be written: amounts are what
// its receipts are worth by item, when the request names them.
type posting struct {
	company      tenancy.Entity
	movementType string
	day          time.Time
	headerText   *string
	reversalOf   *uuid.UUID
	reference    *string // a purchase order's goods receipt's number (101, 102)
	lines        []Line
	items        []ItemInput
}

// write locks the valuations and the stock the lines move, refuses them
// below zero, values them, numbers the document from its company's
// material_document range of its posting date's fiscal year, posts its
// value through the ledger and writes it with its lines, which add to
// the stock and the valuations (inventory.movement_stock). The locks come
// first, in one order, and every check follows them (ADR-0008).
func (e *Endpoints) write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, r refs) (Document, error) {
	vals, err := lockValuations(ctx, tx, in.lines, r)
	if err != nil {
		return Document{}, err
	}
	mt := movementTypes[in.movementType]
	amounts, err := itemAmounts(in.items, mt, in.movementType, vals)
	if err != nil {
		return Document{}, err
	}
	if err := takeStock(ctx, tx, p, in.company.ID, in.lines, r); err != nil {
		return Document{}, err
	}
	if err := valueLines(in.lines, mt, vals, amounts, r); err != nil {
		return Document{}, err
	}
	n, err := e.numbered(ctx, in)
	if err != nil {
		return Document{}, err
	}
	entry, err := e.post(ctx, in, n.id, n.number, vals, r)
	if err != nil {
		return Document{}, err
	}
	return insertDocument(ctx, tx, p, in, n, entry)
}

// lock locks the valuations and then the stock the lines move, refusing
// a line without a valuation or below zero.
func (e *Endpoints) lock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, r refs) (map[valuationKey]*Valuation, error) {
	vals, err := lockValuations(ctx, tx, in.lines, r)
	if err != nil {
		return nil, err
	}
	return vals, takeStock(ctx, tx, p, in.company.ID, in.lines, r)
}

// numbering is a document's id, fiscal year and number.
type numbering struct {
	id     uuid.UUID
	year   int16
	number string
}

// numbered numbers a document from its company's material_document range
// of its posting date's fiscal year.
func (e *Endpoints) numbered(ctx context.Context, in posting) (numbering, error) {
	year, err := e.Calendar.FiscalYear(ctx, in.company.ID, in.day)
	if err != nil {
		return numbering{}, fmt.Errorf("inventory: fiscal year of %s in company %s: %w", in.day.Format(time.DateOnly), in.company.Code, err)
	}
	number, err := e.Numbers.Next(ctx, in.company.ID, NumberRange, year)
	if err != nil {
		return numbering{}, fmt.Errorf("inventory: number a material document in company %s: %w", in.company.Code, err)
	}
	return numbering{id: uuid.Must(uuid.NewV7()), year: year, number: number}, nil
}

// insertDocument writes a valued document with its lines and the entry
// that posted it, and reads it back.
func insertDocument(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, n numbering, entry entryRef) (Document, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type,
			posting_date, header_text, reversal_of_id, currency, journal_entry_id, journal_entry_year, journal_entry_number, reference, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		p.TenantID, n.id, in.company.ID, n.year, n.number, in.movementType, in.day, in.headerText, in.reversalOf, in.company.LocalCurrency, entry.id,
		entry.year, entry.number, in.reference, p.ID); err != nil {
		return Document{}, fmt.Errorf("inventory: insert material document %s: %w", n.number, err)
	}
	if err := writeLines(ctx, tx, p, n.id, in.lines); err != nil {
		return Document{}, err
	}
	return load(ctx, tx, n.id)
}

// writeLines writes a document's movements in line order, the order
// takeStock checked them in: movement_stock applies each line on its own
// and stock_not_negative refuses any line that would take a location
// below zero.
func writeLines(ctx context.Context, tx pgx.Tx, p reqctx.Principal, document uuid.UUID, lines []Line) error {
	n := len(lines)
	lineNo, itemNo := make([]int16, n), make([]int16, n)
	ms, plants, locations := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n)
	qty, entry, units, centers := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	amounts, differences := make([]money.Minor, n), make([]money.Minor, n)
	for i, l := range lines {
		lineNo[i], itemNo[i], ms[i], plants[i], locations[i] = l.LineNo, l.ItemNo, l.MaterialID, l.PlantID, l.StorageLocationID
		qty[i], entry[i], units[i], amounts[i], differences[i] = l.Quantity, l.EntryQuantity, l.EntryUnit, l.Amount, l.Difference
		if l.CostCenterID != nil {
			centers[i] = l.CostCenterID.String()
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id,
			storage_location_id, quantity, entry_quantity, entry_uom_code, cost_center_id, amount, difference)
		SELECT $1, $2, x.l, x.i, x.m, x.p, x.s, x.q::numeric, x.e::numeric, x.u, nullif(x.c, '')::uuid, x.a, x.d
		FROM unnest($3::smallint[], $4::smallint[], $5::uuid[], $6::uuid[], $7::uuid[], $8::text[], $9::text[], $10::text[], $11::text[],
			$12::bigint[], $13::bigint[]) AS x (l, i, m, p, s, q, e, u, c, a, d) ORDER BY x.l`,
		p.TenantID, document, lineNo, itemNo, ms, plants, locations, qty, entry, units, centers, amounts, differences); err != nil {
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
