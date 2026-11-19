package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// refs is what documents and stock name by id, as a request checks them
// and their consumer reads them (REVIEW.md, API-consumer classes): their
// materials by number and description, their plants and storage
// locations by code and name, their companies and cost centers by code,
// and the companies the caller may act in.
type refs struct {
	materials map[uuid.UUID]materials.Material
	units     map[uuid.UUID]tenancy.Unit
	companies map[uuid.UUID]tenancy.Entity
	centers   map[uuid.UUID]ledger.CostCenterRef
	access    authz.Access
}

// read reads the materials, org units, cost centers and companies, each
// once.
func (e *Endpoints) read(ctx context.Context, ms, units, centers, companies []uuid.UUID) (refs, error) {
	a, err := access(ctx)
	if err != nil {
		return refs{}, err
	}
	r := refs{access: a, companies: map[uuid.UUID]tenancy.Entity{}}
	if r.materials, err = e.Materials.Materials(ctx, ms); err != nil {
		return refs{}, fmt.Errorf("inventory: %d materials: %w", len(ms), err)
	}
	if r.units, err = e.LegalEntities.OrgUnits(ctx, units); err != nil {
		return refs{}, fmt.Errorf("inventory: %d plants and storage locations: %w", len(units), err)
	}
	if r.centers, err = e.CostCenters.CostCenterRefs(ctx, centers); err != nil {
		return refs{}, fmt.Errorf("inventory: %d cost centers: %w", len(centers), err)
	}
	for _, id := range companies {
		if _, ok := r.companies[id]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, id) // a stored document's or stock's company exists
		if err != nil {
			return refs{}, fmt.Errorf("inventory: company %s: %w", id, err)
		}
		r.companies[id] = le
	}
	return r, nil
}

// documentRefs reads what the documents name.
func (e *Endpoints) documentRefs(ctx context.Context, ds []Document) (refs, error) {
	var ms, units, centers, companies []uuid.UUID
	for _, d := range ds {
		companies = append(companies, d.LegalEntityID)
		for _, l := range d.Lines {
			ms, units = append(ms, l.MaterialID), append(units, l.PlantID, l.StorageLocationID)
			if l.CostCenterID != nil {
				centers = append(centers, *l.CostCenterID)
			}
		}
	}
	return e.read(ctx, ms, units, centers, companies)
}

// record adds the audit record of a posted document and publishes
// MaterialDocumentPosted in the same transaction (ADR-0007, ADR-0011),
// both naming the lines' materials, plants, storage locations and cost
// centers by id and number or code. Neither carries the header text, a
// person's free text (ADR-0079): the audit says whether there is one.
func (e *Endpoints) record(ctx context.Context, action string, d Document, r refs, detail map[string]any) error {
	le, _, err := e.LegalEntities.Entity(ctx, d.LegalEntityID) // a document's company exists
	if err != nil {
		return fmt.Errorf("inventory: company of material document %s: %w", d.Number, err)
	}
	lines := make([]map[string]any, len(d.Lines))
	for i, l := range d.Lines {
		m := r.materials[l.MaterialID]
		line := map[string]any{"line_no": l.LineNo, "item_no": l.ItemNo, "material_id": l.MaterialID, "material_number": m.Number,
			"plant_id": l.PlantID, "plant_code": r.units[l.PlantID].Code, "storage_location_id": l.StorageLocationID,
			"storage_location_code": r.units[l.StorageLocationID].Code, "quantity": l.Quantity, "unit": m.BaseUnit, "cost_center_id": l.CostCenterID,
			"cost_center_code": nil}
		if l.CostCenterID != nil {
			line["cost_center_code"] = r.centers[*l.CostCenterID].Code
		}
		if d.Currency != "" {
			line["amount"], line["price_difference"] = money.EventAmount(l.Amount, d.Currency, d.Exponent), money.EventAmount(l.Difference, d.Currency, d.Exponent)
		}
		lines[i] = line
	}
	payload := map[string]any{"material_document_id": d.ID, "number": d.Number, "legal_entity_id": d.LegalEntityID, "legal_entity_code": le.Code,
		"fiscal_year": d.FiscalYear, "movement_type": d.MovementType, "posting_date": d.PostingDate.Format(time.DateOnly),
		"reversal_of_id": d.ReversalOfID, "reversal_of_number": d.ReversalOfNumber, "journal_entry_id": d.EntryID, "journal_entry_year": d.EntryYear,
		"journal_entry_number": d.EntryNumber, "reference": d.Reference, "lines": lines}
	detail["number"], detail["movement_type"], detail["posting_date"], detail["lines"] = d.Number, d.MovementType, payload["posting_date"], lines
	detail["journal_entry_id"] = d.EntryID
	if err := audit.Record(ctx, action, "inventory.material_document", d.ID, detail,
		outbox.Event{Context: "inventory", Name: "material_document_posted", AggregateType: "inventory.material_document", AggregateID: d.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("inventory: record %s of material document %s: %w", action, d.Number, err)
	}
	return nil
}
