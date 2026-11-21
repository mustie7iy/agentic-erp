package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Serial is a serial number of a serial-managed material
// (inventory.serial, S2g): where it is in stock now, nil when in none,
// and the company of the goods movement that last moved it.
type Serial struct {
	ID                uuid.UUID
	LegalEntityID     uuid.UUID
	MaterialID        uuid.UUID
	Number            string
	PlantID           *uuid.UUID
	StorageLocationID *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// serialMove is a goods movement of a serial number: the material
// document line that received or issued it.
type serialMove struct {
	DocumentID        uuid.UUID
	DocumentNumber    string
	FiscalYear        int16
	MovementType      string
	PostingDate       time.Time
	LineNo            int16
	Received          bool
	PlantID           uuid.UUID
	StorageLocationID uuid.UUID
}

const serialSelect = `SELECT id, legal_entity_id, material_id, number, plant_id, storage_location_id, created_at, updated_at FROM inventory.serial`

func scanSerial(row pgx.Row) (Serial, error) {
	var s Serial
	err := row.Scan(&s.ID, &s.LegalEntityID, &s.MaterialID, &s.Number, &s.PlantID, &s.StorageLocationID, &s.CreatedAt, &s.UpdatedAt)
	return s, errs.Wrapf(err, "scan serial number")
}

// serialRefs reads what serial numbers and their movements name.
func (e *Endpoints) serialRefs(ctx context.Context, ss []Serial, moves []serialMove) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, s := range ss {
		ms, companies = append(ms, s.MaterialID), append(companies, s.LegalEntityID)
		if s.PlantID != nil {
			units = append(units, *s.PlantID, *s.StorageLocationID)
		}
	}
	for _, m := range moves {
		units = append(units, m.PlantID, m.StorageLocationID)
	}
	return e.read(ctx, ms, units, nil, companies)
}

// serialAPI is a serial number as the API gives it, naming its material,
// where it is and its company.
func serialAPI(s Serial, r refs) gen.SerialNumber {
	m := r.materials[s.MaterialID]
	out := gen.SerialNumber{ID: s.ID, MaterialID: s.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description, Number: s.Number,
		InStock: s.PlantID != nil, LegalEntityID: s.LegalEntityID, CompanyCode: r.companies[s.LegalEntityID].Code, CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt}
	if s.PlantID != nil {
		plant, loc := r.units[*s.PlantID], r.units[*s.StorageLocationID]
		out.PlantID, out.PlantCode, out.PlantName = gen.NewOptUUID(*s.PlantID), gen.NewOptString(plant.Code), gen.NewOptString(plant.Name)
		out.StorageLocationID, out.StorageLocationCode = gen.NewOptUUID(*s.StorageLocationID), gen.NewOptString(loc.Code)
		out.StorageLocationName = gen.NewOptString(loc.Name)
	}
	return out
}

// GetSerialNumber implements GET /v1/serial-numbers/{id}: a serial number
// whose last movement was in a company the caller may act in, with its
// movements in those companies, in the order they were posted.
func (e *Endpoints) GetSerialNumber(ctx context.Context, params gen.GetSerialNumberParams) (gen.GetSerialNumberRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	s, err := scanSerial(tx.QueryRow(ctx, serialSelect+` WHERE id = $1`, params.ID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !a.Allows(s.LegalEntityID)) {
		return nil, problem.NotFoundOf("serial number")
	}
	if err != nil {
		return nil, fmt.Errorf("inventory: read serial number %s: %w", params.ID, err)
	}
	rows, _ := tx.Query(ctx, `SELECT d.id, d.number, d.fiscal_year, d.movement_type, d.posting_date, m.line_no, m.quantity > 0, m.plant_id,
			m.storage_location_id
		FROM inventory.movement_serial ms
		JOIN inventory.movement m ON m.tenant_id = ms.tenant_id AND m.material_document_id = ms.material_document_id AND m.line_no = ms.line_no
		JOIN inventory.material_document d ON d.tenant_id = ms.tenant_id AND d.id = ms.material_document_id
		WHERE ms.serial_id = $1 AND ($2 OR d.legal_entity_id = ANY($3)) ORDER BY ms.created_at, d.id, m.line_no`, s.ID, a.All, a.IDs)
	moves, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (serialMove, error) {
		var m serialMove
		return m, row.Scan(&m.DocumentID, &m.DocumentNumber, &m.FiscalYear, &m.MovementType, &m.PostingDate, &m.LineNo, &m.Received, &m.PlantID,
			&m.StorageLocationID)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: history of serial number %s: %w", s.Number, err)
	}
	r, err := e.serialRefs(ctx, []Serial{s}, moves)
	if err != nil {
		return nil, err
	}
	out := serialAPI(s, r)
	out.History = make([]gen.SerialNumberMovement, len(moves))
	for i, m := range moves {
		direction := "issued"
		if m.Received {
			direction = "received"
		}
		out.History[i] = gen.SerialNumberMovement{MaterialDocumentID: m.DocumentID, MaterialDocumentNumber: m.DocumentNumber, FiscalYear: int32(m.FiscalYear),
			MovementType: m.MovementType, MovementTypeName: movementTypes[m.MovementType].name, PostingDate: m.PostingDate, LineNo: int32(m.LineNo),
			Direction: direction, PlantID: m.PlantID, PlantCode: r.units[m.PlantID].Code, StorageLocationID: m.StorageLocationID,
			StorageLocationCode: r.units[m.StorageLocationID].Code}
	}
	return &gen.SerialNumberHeaders{ETag: gen.NewOptString(etag.Of(&out)), Response: out}, nil
}

var serialSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListSerialNumbers implements GET /v1/serial-numbers: the serial numbers
// whose last movement was in a company the caller may act in, in the
// order they were first received, filtered by material, number, where
// they are in stock and whether they are.
func (e *Endpoints) ListSerialNumbers(ctx context.Context, params gen.ListSerialNumbersParams) (gen.ListSerialNumbersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	material, plant, loc := apitypes.Ptr[uuid.UUID](params.MaterialID), apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.StorageLocationID)
	inStock := apitypes.Ptr[bool](params.InStock)
	scope := fmt.Sprintf("listSerialNumbers|%s|%s|%s|%s|%v", material, params.Number.Value, plant, loc, params.InStock)
	ks, err := e.Pages.Keyset(serialSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("inventory: page of serial numbers: %w", err)
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	query, args := ks.SQL(serialSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR material_id = $3) AND ($4 = '' OR number = $4)
		AND ($5::uuid IS NULL OR plant_id = $5) AND ($6::uuid IS NULL OR storage_location_id = $6) AND ($7::boolean IS NULL OR (plant_id IS NOT NULL) = $7)`,
		[]any{a.All, a.IDs, material, params.Number.Value, plant, loc, inStock})
	rows, _ := tx.Query(ctx, query, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Serial, error) { return scanSerial(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: list serial numbers: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ss, func(s Serial) (string, uuid.UUID) { return pagination.Instant(s.CreatedAt), s.ID },
		func(page []Serial) (refs, error) { return e.serialRefs(ctx, page, nil) }, serialAPI)
	if err != nil {
		return nil, err
	}
	return &gen.SerialNumberPage{Items: items, NextPageToken: next}, nil
}
