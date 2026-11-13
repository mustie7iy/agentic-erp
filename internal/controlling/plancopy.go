package controlling

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A planning version (ADR-0050 §8, As built T93a): a category's current
// plan of a company and fiscal year copied into another category, each
// amount scaled by a percentage. The copy is one plan batch of the
// target, so everything a batch is held to holds for it: the target is
// held FOR UPDATE and must be open, the lines must name what the ledger
// allows and change no cost center's plan in a period locked for
// planning, and the batch is audited and published. What differs: its
// lines come from the source's plan instead of the request, it replaces
// the target's whole plan of the company and year (only when asked to),
// and the batch names its origin.

// maxCopyLines bounds a copy, which reads and writes its lines in one
// request: ten uploads' worth.
const maxCopyLines = 50_000

// maxCopyPercent is the largest percentage a copy scales by.
const maxCopyPercent = 1000

// planCopy is a copy's request, resolved.
type planCopy struct {
	source, target Category
	year           int16
	percentage     string
	replace        bool
}

func copyInvalid(field, code, message string) error {
	return problem.Unprocessable("controlling.plan_copy_invalid", "The plan cannot be copied as asked; nothing was written.",
		problem.FieldError{Field: field, Code: code, Message: message})
}

// CopyPlanCategory implements POST /v1/plan-categories/{id}:copy.
func (e *Endpoints) CopyPlanCategory(ctx context.Context, req *gen.PlanCategoryCopy, params gen.CopyPlanCategoryParams) (gen.CopyPlanCategoryRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, req.LegalEntityID, "legal_entity_id")
	if err != nil {
		return nil, err
	}
	c, err := resolveCopy(ctx, tx, params.ID, req)
	if err != nil {
		return nil, err
	}
	// The caller copies the category as it read it: a lock or an unlock
	// since moves its version.
	if err := etag.IfMatch(params.IfMatch)(c.source.Version); err != nil {
		return nil, err
	}
	lines, err := c.lines(ctx, tx, le.ID)
	if err != nil {
		return nil, err
	}
	of, err := c.replaced(ctx, tx, le.ID, lines)
	if err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	b, err := e.replacePlan(ctx, tx, p, Batch{ID: id, CategoryID: c.target.ID, LegalEntityID: le.ID, FiscalYear: c.year, Key: "copy-" + id.String(),
		Digest: digestOf(lines), Lines: int32(len(lines)), CopiedFrom: &c.source.ID, CopyPercentage: &c.percentage}, lines, of,
		func(i int) string { return fmt.Sprintf("source_lines[%s]", lines[i].source) })
	if err != nil {
		return nil, err
	}
	return &gen.PlanBatchHeaders{Response: batchToAPI(b, false)}, nil
}

// copyPercentage reports whether s is a percentage a copy scales by:
// above 0 and at most maxCopyPercent, with up to four decimals, as the
// batch keeps it (plan_batch.copy_percentage).
func copyPercentage(s string) bool {
	r, err := money.ParsePlain(s)
	if err != nil || r.Sign() <= 0 || r.Cmp(big.NewRat(maxCopyPercent, 1)) > 0 {
		return false
	}
	return new(big.Rat).Mul(r, big.NewRat(10_000, 1)).IsInt()
}

// resolveCopy checks the request and reads its categories: the source as
// it is (404 when there is none), the target held as a batch into it
// holds it (getCategory), which must be another category and open. The
// source is not held: its lines are read in one statement (lines), and
// the batch's foreign key to it takes it FOR KEY SHARE only, which no
// holder of a category blocks, so a copy does not wait for a batch into
// its source or for the source's lock, and two copies in opposite
// directions do not wait for each other.
func resolveCopy(ctx context.Context, tx pgx.Tx, source uuid.UUID, req *gen.PlanCategoryCopy) (planCopy, error) {
	c := planCopy{year: int16(req.FiscalYear), percentage: req.Percentage, replace: req.Replace.Or(false)}
	if !copyPercentage(req.Percentage) {
		return planCopy{}, copyInvalid("percentage", "invalid", fmt.Sprintf("a percentage above 0 and at most %d with up to four decimals", maxCopyPercent))
	}
	if req.TargetPlanCategoryID == source {
		return planCopy{}, copyInvalid("target_plan_category_id", "same_category", "another plan category than the one copied")
	}
	var err error
	if c.source, err = getCategory(ctx, tx, source, false); err != nil {
		return planCopy{}, err
	}
	c.target, err = getCategory(ctx, tx, req.TargetPlanCategoryID, true)
	if missing := new(problem.Problem); errors.As(err, &missing) && missing.Status == 404 {
		return planCopy{}, problem.ReferenceNotFound("target_plan_category_id", "plan category", "no plan category with this id")
	}
	if err != nil {
		return planCopy{}, err
	}
	if c.target.Locked {
		return planCopy{}, categoryLocked()
	}
	return c, nil
}

// lines are the source's current lines of the company and year, scaled:
// each on its center, account and period with its quantity, the amount
// multiplied by the percentage and rounded half away from zero
// (money.Scale). They come in the plan's key order, so the batch's
// centers are in one order whatever the table's.
func (c planCopy) lines(ctx context.Context, tx pgx.Tx, le uuid.UUID) ([]planLine, error) {
	rows, _ := tx.Query(ctx, `SELECT id, cost_center_id, profit_center_id, account_id, fiscal_period, amount, quantity::text, uom_code
		FROM controlling.plan_line WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND replaced_by_batch_id IS NULL
		ORDER BY cost_center_id, profit_center_id, account_id, fiscal_period LIMIT $4`, c.source.ID, le, c.year, maxCopyLines+1)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (planLine, error) {
		var l planLine
		return l, errs.Wrapf(r.Scan(&l.source, &l.costCenter, &l.profitCenter, &l.account, &l.period, &l.amount, &l.quantity, &l.unit), "scan plan line")
	})
	switch {
	case err != nil:
		return nil, fmt.Errorf("controlling: plan of category %s to copy: %w", c.source.ID, err)
	case len(lines) == 0:
		return nil, copyInvalid("fiscal_year", "no_plan", "a fiscal year the category has plan lines of in the legal entity")
	case len(lines) > maxCopyLines:
		return nil, copyInvalid("fiscal_year", "too_many_lines", fmt.Sprintf("a plan of at most %d lines; copy a larger one by uploading batches", maxCopyLines))
	}
	for i := range lines {
		if lines[i].amount, err = money.Scale(lines[i].amount, c.percentage, 100); err != nil {
			return nil, copyInvalid("percentage", "overflow", "a percentage after which every amount still fits an amount")
		}
	}
	return lines, nil
}

// replaced are the centers whose plan the copy replaces: those of its
// lines and those of the target's current plan of the company and year,
// all of which the copy replaces. A target that has a plan is refused
// unless the request says to replace it.
func (c planCopy) replaced(ctx context.Context, tx pgx.Tx, le uuid.UUID, lines []planLine) (planCenters, error) {
	of := planOf(lines)
	rows, _ := tx.Query(ctx, `SELECT DISTINCT cost_center_id, profit_center_id FROM controlling.plan_line
		WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND replaced_by_batch_id IS NULL
		ORDER BY cost_center_id, profit_center_id`, c.target.ID, le, c.year)
	var costCenter, profitCenter *uuid.UUID
	n, err := pgx.ForEachRow(rows, []any{&costCenter, &profitCenter}, func() error {
		of.add(costCenter, profitCenter)
		return nil
	})
	if err != nil {
		return planCenters{}, fmt.Errorf("controlling: current plan of category %s: %w", c.target.ID, err)
	}
	if n.RowsAffected() > 0 && !c.replace {
		return planCenters{}, problem.Conflict("controlling.plan_copy_target_planned", "The target category already has a plan of the legal entity "+
			"and fiscal year; copy with replace to replace all of it, or copy into a new category.")
	}
	return of, nil
}
