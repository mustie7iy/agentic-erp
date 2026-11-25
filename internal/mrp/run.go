package mrp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Planning runs (ADR-0084 §3): a run of one plant is written queued with
// its Operation by the request, and carried out by the workflows context
// (or its in-process runner) through PlanRun, FailRun and UnfinishedRuns.

// Run is a stored planning run with its lines and the proposals it
// replaced.
type Run struct {
	ID, LegalEntityID, PlantID, OperationID uuid.UUID
	Status                                  string
	RunDate                                 *time.Time
	Planned, Created, Replaced              *int32
	Error                                   *RunError
	Version                                 int32
	CreatedAt, UpdatedAt                    time.Time
	CreatedBy, UpdatedBy                    uuid.UUID
	Lines                                   []Line
	Replacements                            []Replacement
}

// RunError is the problem that failed a run.
type RunError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Line is what a run counted and proposed of a material, in its base
// unit (Unit), as plain decimals: the lot sizing procedure it sized the
// shortage by, what it proposed (zero without a shortage) and in how
// many lots, the items of the requisition proposed.
type Line struct {
	MaterialID                                      uuid.UUID
	Unit                                            string
	ReorderPoint, Demand, Stock, Receipts, Shortage string
	LotSize, Proposed                               string
	Lots                                            *int32
	RequisitionID                                   *uuid.UUID
	RequisitionNumber                               *string
}

// Replacement is an earlier run's proposal a run cancelled.
type Replacement struct {
	RequisitionID, MaterialID uuid.UUID
	RequisitionNumber         string
}

// runPath is a run's resource path.
func runPath(id uuid.UUID) string { return "/v1/mrp-runs/" + id.String() }

const runSelect = `SELECT id, legal_entity_id, plant_id, operation_id, status, run_date, materials_planned, proposals_created, proposals_replaced,
		error, version, created_at, updated_at, created_by, updated_by
	FROM mrp.run`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var failure []byte
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.PlantID, &r.OperationID, &r.Status, &r.RunDate, &r.Planned, &r.Created, &r.Replaced, &failure,
		&r.Version, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.UpdatedBy)
	if err == nil && failure != nil {
		r.Error = &RunError{}
		// FailRun wrote it from a RunError; a document that does not
		// decode shows as empty rather than failing the read.
		_ = json.Unmarshal(failure, r.Error)
	}
	return r, errs.Wrapf(err, "scan planning run")
}

// StartRun queues a run of the plant with its Operation: an active plant
// of a company the caller may act in, with no run of it queued (409
// mrp.run_in_progress naming it). Audited.
func (e *Endpoints) StartRun(ctx context.Context, plantID uuid.UUID) (gen.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return gen.Operation{}, err
	}
	plant, le, err := e.plant(ctx, plantID, "plant_id")
	if err != nil {
		return gen.Operation{}, err
	}
	var other uuid.UUID
	switch err := tx.QueryRow(ctx, `SELECT id FROM mrp.run WHERE plant_id = $1 AND status = 'queued'`, plant.ID).Scan(&other); {
	case err == nil:
		return gen.Operation{}, problem.Conflict("mrp.run_in_progress", fmt.Sprintf("Planning run %s of plant %s is queued; one run of a plant "+
			"is queued at a time.", runPath(other), plant.Code))
	case !errors.Is(err, pgx.ErrNoRows):
		return gen.Operation{}, fmt.Errorf("mrp: queued runs of plant %s: %w", plant.Code, err)
	}
	id := uuid.Must(uuid.NewV7())
	op, err := e.Runs.Start(ctx, runPath(id), map[string]any{"plant_id": plant.ID})
	if err != nil {
		return gen.Operation{}, fmt.Errorf("mrp: operation of planning run %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mrp.run (tenant_id, id, legal_entity_id, plant_id, operation_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, p.TenantID, id, le.ID, plant.ID, op.ID, p.ID); err != nil {
		return gen.Operation{}, fmt.Errorf("mrp: queue planning run of plant %s: %w", plant.Code, err)
	}
	return op, audit.Record(ctx, "mrp.run.start", "mrp.run", id, map[string]any{"plant_id": plant.ID, "plant_code": plant.Code,
		"company_code": le.Code, "operation_id": op.ID})
}

// readRun reads a run of a company the caller may act in with its lines
// and replacements; any other is not found.
func readRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Run, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1`, id))
	if err = authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("planning run"), "mrp: planning run %s", id); err != nil {
		return Run{}, err
	}
	runs := []Run{r}
	if err := withDetails(ctx, tx, runs); err != nil {
		return Run{}, err
	}
	return runs[0], nil
}

// withDetails reads the runs' lines, by material, and replacements, by
// requisition number, into them.
func withDetails(ctx context.Context, tx pgx.Tx, runs []Run) error {
	ids := make([]uuid.UUID, len(runs))
	at := make(map[uuid.UUID]*Run, len(runs))
	for i := range runs {
		ids[i], at[runs[i].ID] = runs[i].ID, &runs[i]
	}
	rows, _ := tx.Query(ctx, `SELECT run_id, material_id, uom_code, trim_scale(reorder_point)::text, trim_scale(demand)::text, trim_scale(stock)::text,
			trim_scale(receipts)::text, trim_scale(shortage)::text, requisition_id, requisition_number, lot_size,
			-- a line of a server before lot sizing proposed its shortage in one lot (000482)
			trim_scale(COALESCE(proposed, shortage))::text, CASE WHEN requisition_id IS NOT NULL THEN COALESCE(lots, 1) END
		FROM mrp.run_line WHERE run_id = ANY($1) ORDER BY run_id, material_id`, ids)
	var run uuid.UUID
	var l Line
	if _, err := pgx.ForEachRow(rows, []any{&run, &l.MaterialID, &l.Unit, &l.ReorderPoint, &l.Demand, &l.Stock, &l.Receipts, &l.Shortage,
		&l.RequisitionID, &l.RequisitionNumber, &l.LotSize, &l.Proposed, &l.Lots}, func() error {
		at[run].Lines = append(at[run].Lines, l)
		l.RequisitionID, l.RequisitionNumber, l.Lots = nil, nil, nil // pgx scans the next row's into the pointers the line just kept holds
		return nil
	}); err != nil {
		return fmt.Errorf("mrp: lines of %d planning runs: %w", len(runs), err)
	}
	rows, _ = tx.Query(ctx, `SELECT replaced_by_run_id, requisition_id, material_id, requisition_number FROM mrp.proposal
		WHERE replaced_by_run_id = ANY($1) ORDER BY replaced_by_run_id, requisition_number`, ids)
	var x Replacement
	_, err := pgx.ForEachRow(rows, []any{&run, &x.RequisitionID, &x.MaterialID, &x.RequisitionNumber}, func() error {
		at[run].Replacements = append(at[run].Replacements, x)
		return nil
	})
	return errs.Wrapf(err, "mrp: proposals replaced by %d planning runs", len(runs))
}
