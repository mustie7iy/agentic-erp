package closing

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CreateYearClose implements POST /v1/year-closes (ADR-0060 §8): the
// close of a company's fiscal year, covering the ledgers named (every one
// the company keeps by default, the leading one always), its tasks the
// active configuration's year tasks for the company, copied with their due
// dates, counted from the year's last day, and the configuration version.
func (e *Endpoints) CreateYearClose(ctx context.Context, req *gen.YearCloseCreate, _ gen.CreateYearCloseParams) (gen.CreateYearCloseRes, error) {
	year := int16(req.FiscalYear) // the contract bounds it to 1900-2999
	tx, c, err := e.openClose(ctx, yearCloses, req.LegalEntityID, opening{
		checklist: func(ctx context.Context, doc *config.Config, co Company) ([]LedgerPeriod, []config.CloseTask, string, error) {
			covered, err := e.yearLedgers(ctx, co.ID, year, req.LedgerIds)
			return covered, doc.YearCloseTasksOf(co.Code), "year tasks (close.year_tasks) give", err
		},
		insert: func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, end time.Time, version int32, covered []LedgerPeriod) error {
			return insertYearClose(ctx, tx, p, id, req.LegalEntityID, year, end, version, covered)
		},
	})
	if err != nil {
		return nil, err
	}
	out, err := e.yearCloseOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.YearCloseHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(yearCloses.path + c.ID.String()),
		Response: out}, nil
}

// insertYearClose writes a year close and the ledgers it covers, refusing
// a second close of the company's year.
func insertYearClose(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id, legalEntity uuid.UUID, year int16, end time.Time, version int32,
	covered []LedgerPeriod) error {
	first, ok, err := existing(ctx, tx, yearCloses, legalEntity, year, 0)
	if err != nil {
		return err
	}
	if ok {
		return exists(yearCloses, first, problem.Conflict("closing.year_close_exists",
			fmt.Sprintf("The company has year close %s of fiscal year %d already.", first, year)))
	}
	ledgers := make([]uuid.UUID, len(covered))
	for i, l := range covered {
		ledgers[i] = l.LedgerID
	}
	_, err = tx.Exec(ctx, `WITH c AS (INSERT INTO closing.year_close (tenant_id, id, legal_entity_id, fiscal_year, year_end, config_version,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $7))
		INSERT INTO closing.year_close_ledger (tenant_id, close_id, ledger_id) SELECT $1, $2, l FROM unnest($8::uuid[]) AS x(l)`,
		p.TenantID, id, legalEntity, year, end, version, p.ID, ledgers)
	return errs.Wrapf(err, "closing: insert year close of %s, %d", legalEntity, year)
}

// yearLedgers are the ledgers a year close covers, the leading one first,
// then by code (pick), each with its last regular period of the year,
// whose last day is the year's and the tasks' due dates count from; a
// ledger without periods of the year has none.
func (e *Endpoints) yearLedgers(ctx context.Context, legalEntity uuid.UUID, year int16, ids []uuid.UUID) ([]LedgerPeriod, error) {
	years, err := e.Ledger.YearPeriods(ctx, legalEntity, year)
	if err != nil {
		return nil, fmt.Errorf("closing: ledgers of %s in %d: %w", legalEntity, year, err)
	}
	kept := make([]LedgerPeriod, len(years))
	for i, l := range years {
		kept[i] = LedgerPeriod{LedgerID: l.LedgerID, Code: l.Code, Name: l.Name, Leading: l.Leading}
		for _, p := range l.Periods { // by period: the last regular one stays
			if !p.Special {
				kept[i].Period = &p
			}
		}
	}
	return pick(kept, ids, fmt.Sprintf("periods of %d", year), "fiscal_year")
}

// yearCovered are the ledgers a year close covers, the leading one first,
// then by code, each with its periods of the year as the ledger holds them
// now.
func (e *Endpoints) yearCovered(ctx context.Context, tx pgx.Tx, c closeRow) ([]LedgerYear, error) {
	covered, err := coveredIDs(ctx, tx, yearCloses, c.ID)
	if err != nil {
		return nil, err
	}
	years, err := e.Ledger.YearPeriods(ctx, c.LegalEntityID, c.FiscalYear)
	if err != nil {
		return nil, fmt.Errorf("closing: periods of year close %s: %w", c.ID, err)
	}
	var out []LedgerYear
	for _, l := range years {
		if slices.Contains(covered, l.LedgerID) && len(l.Periods) > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

// yearCloseOut is a year close as a single read shows it: with its
// company, its ledgers' periods of the year with their live status, and
// its tasks.
func (e *Endpoints) yearCloseOut(ctx context.Context, tx pgx.Tx, c closeRow) (gen.YearClose, error) {
	tasks, err := tasksOf(ctx, tx, yearCloses, c.ID)
	if err != nil {
		return gen.YearClose{}, err
	}
	v, err := e.view(ctx, []uuid.UUID{c.LegalEntityID}, tasks)
	if err != nil {
		return gen.YearClose{}, err
	}
	covered, err := e.yearCovered(ctx, tx, c)
	if err != nil {
		return gen.YearClose{}, err
	}
	return v.yearCloseToAPI(c, covered, tasks), nil
}

// GetYearClose implements GET /v1/year-closes/{id}.
func (e *Endpoints) GetYearClose(ctx context.Context, params gen.GetYearCloseParams) (gen.GetYearCloseRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := readClose(ctx, tx, yearCloses, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.yearCloseOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetYearCloseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// ListYearCloses implements GET /v1/year-closes: the year closes of the
// legal entities the caller may act in, without their tasks.
func (e *Endpoints) ListYearCloses(ctx context.Context, params gen.ListYearClosesParams) (gen.ListYearClosesRes, error) {
	items, next, err := listCloses(ctx, e, yearCloses, "listYearCloses", yearCloseSort, closeFilter{legalEntity: params.LegalEntityID,
		fiscalYear: params.FiscalYear, status: params.Status, page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)}, view.yearItemToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.YearClosePage{Items: items, NextPageToken: next}, nil
}

// yearTask answers a year close's task action: the task with the close's
// new ETag.
func yearTask(t task, v view, err error) (*gen.YearCloseTaskHeaders, error) {
	if err != nil {
		return nil, err
	}
	return &gen.YearCloseTaskHeaders{ETag: gen.NewOptString(etag.Format(t.CloseVersion)), Response: v.yearTaskToAPI(t)}, nil
}

// UpdateYearCloseTask implements PATCH /v1/year-closes/{id}/tasks/{code}:
// the task's assignee, as a period close's (UpdateCloseTask).
func (e *Endpoints) UpdateYearCloseTask(ctx context.Context, req *gen.CloseTaskUpdate, params gen.UpdateYearCloseTaskParams) (gen.UpdateYearCloseTaskRes, error) {
	return yearTask(e.taskAction(ctx, yearCloses, params.ID, params.Code, params.IfMatch, e.assign(req)))
}

// CompleteYearCloseTask implements POST
// /v1/year-closes/{id}/tasks/{code}:complete, as a period close's
// (CompleteCloseTask).
func (e *Endpoints) CompleteYearCloseTask(ctx context.Context, req gen.OptCloseTaskComplete,
	params gen.CompleteYearCloseTaskParams) (gen.CompleteYearCloseTaskRes, error) {
	return yearTask(e.taskAction(ctx, yearCloses, params.ID, params.Code, params.IfMatch, complete(req)))
}

// ReopenYearCloseTask implements POST
// /v1/year-closes/{id}/tasks/{code}:reopen, as a period close's
// (ReopenCloseTask).
func (e *Endpoints) ReopenYearCloseTask(ctx context.Context, req *gen.ReasonInput, params gen.ReopenYearCloseTaskParams) (gen.ReopenYearCloseTaskRes, error) {
	return yearTask(e.taskAction(ctx, yearCloses, params.ID, params.Code, params.IfMatch, reopenTask(req)))
}
