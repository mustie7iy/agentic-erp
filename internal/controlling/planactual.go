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

// Plan against actual (ADR-0050 §8): per cost center (T56g) or profit
// center (T56g3) and account, the category's plan and the leading ledger's
// actual over a range of periods. The plan is controlling's, the actual
// the ledger's (through ledger.AssignmentBalances), so a page is merged
// from both: each side reads its first rows after the page's start in key
// order, one more than the page, and the page is the first keys of the
// two. The two operations differ only in the kind of center (paKind).

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
	if c := bytes.Compare(a.CenterID[:], b.CenterID[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.AccountID[:], b.AccountID[:])
}

// paKind is a kind of center plan against actual is by: the operation (the
// page token's scope), plan_line's column naming it, and the ledger port's
// centers of a center or node and actuals by such a center.
type paKind struct {
	operation, column string
	centers           func(Ledger, context.Context, *uuid.UUID, *uuid.UUID) ([]uuid.UUID, bool, error)
	actuals           func(Ledger, context.Context, ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
}

var (
	byCostCenter   = paKind{"listCostCenterPlanActual", "cost_center_id", Ledger.CostCenters, Ledger.CostCenterActuals}
	byProfitCenter = paKind{"listProfitCenterPlanActual", "profit_center_id", Ledger.ProfitCenters, Ledger.ProfitCenterActuals}
)

// paParams are the parameters both operations take.
type paParams struct {
	category, legalEntity uuid.UUID
	year                  int32
	from, to, size        gen.OptInt32
	center, node          *uuid.UUID
	token                 gen.OptString
}

// paRow is a row as the API shows it, whichever its kind of center.
type paRow struct {
	center, account        uuid.UUID
	plan, actual, variance gen.Money
	percent                gen.OptDecimal
}

// paPage is a page: its query, its rows and the next page's token.
type paPage struct {
	q    paQuery
	rows []paRow
	next gen.OptString
}

// paQuery is a page's request, its filters resolved.
type paQuery struct {
	kind                 paKind
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
	p, err := e.planActual(ctx, byCostCenter, costCenterParams(params))
	if err != nil {
		return nil, err
	}
	return &gen.CostCenterPlanActualPage{Items: paItems(p, (*gen.CostCenterPlanActual).SetCostCenterID), NextPageToken: p.next}, nil
}

// ListProfitCenterPlanActual implements GET /v1/profit-center-plan-actual.
func (e *Endpoints) ListProfitCenterPlanActual(ctx context.Context, params gen.ListProfitCenterPlanActualParams) (gen.ListProfitCenterPlanActualRes, error) {
	p, err := e.planActual(ctx, byProfitCenter, profitCenterParams(params))
	if err != nil {
		return nil, err
	}
	return &gen.ProfitCenterPlanActualPage{Items: paItems(p, (*gen.ProfitCenterPlanActual).SetProfitCenterID), NextPageToken: p.next}, nil
}

func costCenterParams(p gen.ListCostCenterPlanActualParams) paParams {
	return paParams{category: p.PlanCategoryID, legalEntity: p.LegalEntityID, year: p.FiscalYear, from: p.FiscalPeriodFrom, to: p.FiscalPeriodTo,
		size: p.PageSize, token: p.PageToken, center: apitypes.Ptr[uuid.UUID](p.CostCenterID), node: apitypes.Ptr[uuid.UUID](p.HierarchyNodeID)}
}

func profitCenterParams(p gen.ListProfitCenterPlanActualParams) paParams {
	return paParams{category: p.PlanCategoryID, legalEntity: p.LegalEntityID, year: p.FiscalYear, from: p.FiscalPeriodFrom, to: p.FiscalPeriodTo,
		size: p.PageSize, token: p.PageToken, center: apitypes.Ptr[uuid.UUID](p.ProfitCenterID), node: apitypes.Ptr[uuid.UUID](p.HierarchyNodeID)}
}

// paItem is what both operations' rows have in common, as the generated
// code sets it; the center is set by each kind's own setter.
type paItem[T any] interface {
	*T
	SetPlanCategoryID(uuid.UUID)
	SetLegalEntityID(uuid.UUID)
	SetFiscalYear(int32)
	SetFiscalPeriodFrom(int32)
	SetFiscalPeriodTo(int32)
	SetAccountID(uuid.UUID)
	SetPlan(gen.Money)
	SetActual(gen.Money)
	SetVariance(gen.Money)
	SetVariancePercent(gen.OptDecimal)
}

// paItems are the page's rows as the operation's items.
func paItems[T any, P paItem[T]](p paPage, setCenter func(P, uuid.UUID)) []T {
	out := make([]T, len(p.rows))
	for i, r := range p.rows {
		item := P(&out[i])
		item.SetPlanCategoryID(p.q.category)
		item.SetLegalEntityID(p.q.actuals.LegalEntityID)
		item.SetFiscalYear(int32(p.q.actuals.FiscalYear))
		item.SetFiscalPeriodFrom(int32(p.q.from))
		item.SetFiscalPeriodTo(int32(p.q.to))
		setCenter(item, r.center)
		item.SetAccountID(r.account)
		item.SetPlan(r.plan)
		item.SetActual(r.actual)
		item.SetVariance(r.variance)
		item.SetVariancePercent(r.percent)
	}
	return out
}

// planActual is a page of plan against actual by the kind of center.
func (e *Endpoints) planActual(ctx context.Context, k paKind, params paParams) (paPage, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return paPage{}, err
	}
	q, err := e.planActualQuery(ctx, tx, k, params)
	if err != nil || q.noRows {
		return paPage{q: q}, err
	}
	rows, err := e.planActualRows(ctx, tx, q)
	if err != nil {
		return paPage{}, err
	}
	p := paPage{q: q, rows: make([]paRow, 0, len(rows))}
	if len(rows) > q.size {
		rows = rows[:q.size]
		last := rows[q.size-1]
		tok, _ := e.Pages.Encode(q.scope, paCursor{Center: last.CenterID, Account: last.AccountID}) // uuids always encode
		p.next.SetTo(tok)
	}
	for _, r := range rows {
		row, err := q.toAPI(r)
		if err != nil {
			return paPage{}, err
		}
		p.rows = append(p.rows, row)
	}
	return p, nil
}

// planActualQuery resolves the request: the page, the periods, the legal
// entity (none when the caller cannot act in it), the category (none when
// it does not exist) and the centers of the center or node asked for.
func (e *Endpoints) planActualQuery(ctx context.Context, tx pgx.Tx, k paKind, params paParams) (paQuery, error) {
	size, err := pagination.Size(params.size.Value, params.size.Set)
	if err != nil {
		return paQuery{}, err
	}
	from, to := int16(params.from.Or(1)), int16(params.to.Or(16))
	if from > to {
		return paQuery{}, problem.BadRequest("request.invalid_parameter", "fiscal_period_to is before fiscal_period_from.",
			problem.FieldError{Field: "fiscal_period_to", Code: "before_start", Message: "on or after fiscal_period_from"})
	}
	q := paQuery{kind: k, category: params.category, from: from, to: to, size: size,
		scope:   fmt.Sprintf("%s|%s|%s|%d|%d|%d|%v|%v", k.operation, params.category, params.legalEntity, params.year, from, to, params.center, params.node),
		actuals: ledger.ActualsQuery{LegalEntityID: params.legalEntity, FiscalYear: int16(params.year), PeriodFrom: from, PeriodTo: to, Limit: size + 1}}
	if params.token.Set {
		var c paCursor
		if err := e.Pages.Decode(q.scope, params.token.Value, &c); err != nil {
			return paQuery{}, err
		}
		q.actuals.After = &ledger.CenterAccount{CenterID: c.Center, AccountID: c.Account}
	}
	le, exp, ok, err := e.company(ctx, tx, params.legalEntity)
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
	q.actuals.Centers, ok, err = k.centers(e.Ledger, ctx, params.center, params.node)
	q.centerFilter, q.noRows = q.actuals.Centers != nil, !ok
	return q, err
}

// planActualRows are the page's rows and one more, if there is one: the
// first plan rows and actual rows after its start, merged by key.
func (e *Endpoints) planActualRows(ctx context.Context, tx pgx.Tx, q paQuery) ([]planActual, error) {
	var afterCenter, afterAccount *uuid.UUID
	if a := q.actuals.After; a != nil {
		afterCenter, afterAccount = &a.CenterID, &a.AccountID
	}
	center := pgx.Identifier{q.kind.column}.Sanitize()
	rows, _ := tx.Query(ctx, `SELECT `+center+`, account_id, sum(amount)::bigint FROM controlling.plan_line
		WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND fiscal_period BETWEEN $4 AND $5 AND `+center+` IS NOT NULL
			AND replaced_by_batch_id IS NULL AND (NOT $6 OR `+center+` = ANY($7)) AND ($8::uuid IS NULL OR (`+center+`, account_id) > ($8, $9::uuid))
		GROUP BY 1, 2 ORDER BY 1, 2 LIMIT $10`,
		q.category, q.actuals.LegalEntityID, q.actuals.FiscalYear, q.from, q.to, q.centerFilter, q.actuals.Centers, afterCenter, afterAccount, q.size+1)
	plans, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (planActual, error) {
		var p planActual
		return p, errs.Wrapf(r.Scan(&p.CenterID, &p.AccountID, &p.plan), "scan plan")
	})
	if err != nil {
		return nil, fmt.Errorf("controlling: plan of %s in %d: %w", q.actuals.LegalEntityID, q.actuals.FiscalYear, err)
	}
	actuals, err := q.kind.actuals(e.Ledger, ctx, q.actuals)
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
func (q paQuery) toAPI(r planActual) (paRow, error) {
	variance, err := money.Diff(r.actual, r.plan)
	if err != nil {
		return paRow{}, fmt.Errorf("controlling: variance of %s on %s: %w", r.CenterID, r.AccountID, err)
	}
	out := paRow{center: r.CenterID, account: r.AccountID, plan: apitypes.Money(r.plan, q.currency, q.exp),
		actual: apitypes.Money(r.actual, q.currency, q.exp), variance: apitypes.Money(variance, q.currency, q.exp)}
	if r.plan != 0 {
		pct := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(variance)), big.NewInt(100)), big.NewInt(int64(r.plan)))
		s := pct.FloatString(2)
		if s == "-0.00" { // a negative variance below half a hundredth of a percent rounds to zero, without a sign
			s = "0.00"
		}
		out.percent.SetTo(gen.Decimal(s))
	}
	return out, nil
}
