package controlling

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Plans is controlling's published port for a financial close's plan
// basis (ADR-0058 §2, §9, T64h): a plan category by its code, and its plan
// of a company's period per account, each run in the request's
// transaction under row-level security.
type Plans struct{}

// PlanCategory is a plan category as a close names it.
type PlanCategory struct {
	ID         uuid.UUID
	Code, Name string
}

// Category returns the tenant's plan category with the code, ok false
// when there is none.
func (Plans) Category(ctx context.Context, code string) (PlanCategory, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PlanCategory{}, false, err
	}
	c := PlanCategory{Code: code}
	err = tx.QueryRow(ctx, `SELECT id, name FROM controlling.plan_category WHERE code = $1`, code).Scan(&c.ID, &c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanCategory{}, false, nil
	}
	if err != nil {
		return PlanCategory{}, false, fmt.Errorf("controlling: plan category %s: %w", code, err)
	}
	return c, true, nil
}

// Amounts returns the category's current plan of the legal entity's
// fiscal year and period per account, in local currency, debit positive:
// the sum of its lines on profit centers when profit is true, else on cost
// centers, never both, since a category may plan both and one amount would
// count twice (ADR-0058 §9). A legal entity the caller may not act in has
// none.
func (Plans) Amounts(ctx context.Context, legalEntity, category uuid.UUID, fiscalYear, period int16, profit bool) (map[uuid.UUID]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]money.Minor{}
	if !access.Allows(legalEntity) {
		return out, nil
	}
	center := byCostCenter.column
	if profit {
		center = byProfitCenter.column
	}
	center = pgx.Identifier{center}.Sanitize()
	rows, _ := tx.Query(ctx, `SELECT account_id, sum(amount)::bigint FROM controlling.plan_line
		WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND fiscal_period = $4 AND `+center+` IS NOT NULL
			AND replaced_by_batch_id IS NULL
		GROUP BY account_id`, category, legalEntity, fiscalYear, period)
	var account uuid.UUID
	var amount money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&account, &amount}, func() error { out[account] = amount; return nil }); err != nil {
		return nil, errs.Wrapf(err, "controlling: plan %s of %s in %d/%d", category, legalEntity, fiscalYear, period)
	}
	return out, nil
}
