package controlling

import (
	"bytes"
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Plan against actual (ADR-0050 §8): per cost center and account, the
// category's plan and the leading ledger's actual over a range of periods.
// The plan is controlling's, the actual the ledger's (through
// ledger.AssignmentBalances), so a page is merged from both: each side
// reads its first rows after the page's start in key order, one more than
// the page, and the page is the first keys of the two.

// paCursor is the key of the last row of a page.
type paCursor struct {
	Center  uuid.UUID `json:"c"`
	Account uuid.UUID `json:"a"`
}

// planActual is a row: a key with its plan and actual.
type planActual struct {
	ledger.CenterAccount
	plan, actual money.Minor
}

func compareKeys(a, b ledger.CenterAccount) int {
	if c := bytes.Compare(a.CostCenterID[:], b.CostCenterID[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.AccountID[:], b.AccountID[:])
}

// paQuery is a page's request, its filters resolved.
type paQuery struct {
	category             uuid.UUID
	actuals              ledger.ActualsQuery
	from, to             int16
	scope                string
	size                 int
	currency             string
	exp                  int
	centerFilter, noRows bool
}

// ListCostCenterPlanActual implements GET /v1/cost-center-plan-actual.
func (e *Endpoints) ListCostCenterPlanActual(ctx context.Context, params gen.ListCostCenterPlanActualParams) (gen.ListCostCenterPlanActualRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	q, err := e.planActualQuery(ctx, tx, params)
	if err != nil {
		return nil, err
	}
	page := &gen.CostCenterPlanActualPage{Items: []gen.CostCenterPlanActual{}}
	if q.noRows {
		return page, nil
	}
	rows, err := e.planActualRows(ctx, tx, q)
	if err != nil {
		return nil, err
	}
	if len(rows) > q.size {
		rows = rows[:q.size]
		last := rows[q.size-1]
		tok, _ := e.Pages.Encode(q.scope, paCursor{Center: last.CostCenterID, Account: last.AccountID}) // uuids always encode
		page.NextPageToken.SetTo(tok)
	}
	for _, r := range rows {
		item, err := q.toAPI(r)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

// planActualQuery resolves the request: the page, the periods, the legal
// entity (none when the caller cannot act in it), the category (none when
// it does not exist) and the cost centers of the center or node asked for.
func (e *Endpoints) planActualQuery(ctx context.Context, tx pgx.Tx, params gen.ListCostCenterPlanActualParams) (paQuery, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return paQuery{}, err
	}
	from, to := int16(params.FiscalPeriodFrom.Or(1)), int16(params.FiscalPeriodTo.Or(16))
	if from > to {
		return paQuery{}, problem.BadRequest("request.invalid_parameter", "fiscal_period_to is before fiscal_period_from.",
			problem.FieldError{Field: "fiscal_period_to", Code: "before_start", Message: "on or after fiscal_period_from"})
	}
	center, node := apitypes.Ptr[uuid.UUID](params.CostCenterID), apitypes.Ptr[uuid.UUID](params.HierarchyNodeID)
	q := paQuery{category: params.PlanCategoryID, from: from, to: to, size: size,
		scope:   fmt.Sprintf("listCostCenterPlanActual|%s|%s|%d|%d|%d|%v|%v", params.PlanCategoryID, params.LegalEntityID, params.FiscalYear, from, to, center, node),
		actuals: ledger.ActualsQuery{LegalEntityID: params.LegalEntityID, FiscalYear: int16(params.FiscalYear), PeriodFrom: from, PeriodTo: to, Limit: size + 1}}
	if params.PageToken.Set {
		var c paCursor
		if err := e.Pages.Decode(q.scope, params.PageToken.Value, &c); err != nil {
			return paQuery{}, err
		}
		q.actuals.After = &ledger.CenterAccount{CostCenterID: c.Center, AccountID: c.Account}
	}
	le, exp, ok, err := e.company(ctx, tx, params.LegalEntityID)
	if err != nil || !ok {
		return paQuery{noRows: true}, err
	}
	q.currency, q.exp = le.LocalCurrency, exp
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM controlling.plan_category WHERE id = $1)`, q.category).Scan(&known); err != nil {
		return paQuery{}, fmt.Errorf("controlling: plan category %s: %w", q.category, err)
	}
	if !known {
		return paQuery{noRows: true}, nil
	}
	q.actuals.CostCenters, ok, err = e.Ledger.CostCenters(ctx, center, node)
	q.centerFilter, q.noRows = q.actuals.CostCenters != nil, !ok
	return q, err
}

// planActualRows are the page's rows and one more, if there is one: the
// first plan rows and actual rows after its start, merged by key.
func (e *Endpoints) planActualRows(ctx context.Context, tx pgx.Tx, q paQuery) ([]planActual, error) {
	var afterCenter, afterAccount *uuid.UUID
	if a := q.actuals.After; a != nil {
		afterCenter, afterAccount = &a.CostCenterID, &a.AccountID
	}
	rows, _ := tx.Query(ctx, `SELECT cost_center_id, account_id, sum(amount)::bigint FROM controlling.plan_line
		WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND fiscal_period BETWEEN $4 AND $5 AND cost_center_id IS NOT NULL
			AND replaced_by_batch_id IS NULL AND (NOT $6 OR cost_center_id = ANY($7)) AND ($8::uuid IS NULL OR (cost_center_id, account_id) > ($8, $9::uuid))
		GROUP BY 1, 2 ORDER BY 1, 2 LIMIT $10`,
		q.category, q.actuals.LegalEntityID, q.actuals.FiscalYear, q.from, q.to, q.centerFilter, q.actuals.CostCenters, afterCenter, afterAccount, q.size+1)
	plans, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (planActual, error) {
		var p planActual
		return p, errs.Wrapf(r.Scan(&p.CostCenterID, &p.AccountID, &p.plan), "scan plan")
	})
	if err != nil {
		return nil, fmt.Errorf("controlling: plan of %s in %d: %w", q.actuals.LegalEntityID, q.actuals.FiscalYear, err)
	}
	actuals, err := e.Ledger.CostCenterActuals(ctx, q.actuals)
	if err != nil {
		return nil, err
	}
	return merge(plans, actuals, q.size+1), nil
}

// merge joins plan rows and actual rows, both in key order, into the
// first n keys of either.
func merge(plans []planActual, actuals []ledger.AssignmentActual, n int) []planActual {
	out := []planActual{}
	for len(out) < n && (len(plans) > 0 || len(actuals) > 0) {
		switch {
		case len(actuals) == 0 || (len(plans) > 0 && compareKeys(plans[0].CenterAccount, actuals[0].CenterAccount) < 0):
			out, plans = append(out, plans[0]), plans[1:]
		case len(plans) == 0 || compareKeys(plans[0].CenterAccount, actuals[0].CenterAccount) > 0:
			out, actuals = append(out, planActual{CenterAccount: actuals[0].CenterAccount, actual: actuals[0].Amount}), actuals[1:]
		default:
			p := plans[0]
			p.actual = actuals[0].Amount
			out, plans, actuals = append(out, p), plans[1:], actuals[1:]
		}
	}
	return out
}

// toAPI is a row with its variance and the variance in percent of the
// plan, computed exactly (no floats, ADR-0005) and rounded to two decimals
// half away from zero; none on a zero plan.
func (q paQuery) toAPI(r planActual) (gen.CostCenterPlanActual, error) {
	variance, err := money.Diff(r.actual, r.plan)
	if err != nil {
		return gen.CostCenterPlanActual{}, fmt.Errorf("controlling: variance of %s on %s: %w", r.CostCenterID, r.AccountID, err)
	}
	out := gen.CostCenterPlanActual{PlanCategoryID: q.category, LegalEntityID: q.actuals.LegalEntityID, FiscalYear: int32(q.actuals.FiscalYear),
		FiscalPeriodFrom: int32(q.from), FiscalPeriodTo: int32(q.to), CostCenterID: r.CostCenterID, AccountID: r.AccountID,
		Plan: apitypes.Money(r.plan, q.currency, q.exp), Actual: apitypes.Money(r.actual, q.currency, q.exp),
		Variance: apitypes.Money(variance, q.currency, q.exp)}
	if r.plan != 0 {
		pct := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(variance)), big.NewInt(100)), big.NewInt(int64(r.plan)))
		s := pct.FloatString(2)
		if s == "-0.00" { // a negative variance below half a hundredth of a percent rounds to zero, without a sign
			s = "0.00"
		}
		out.VariancePercent.SetTo(gen.Decimal(s))
	}
	return out, nil
}
