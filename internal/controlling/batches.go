package controlling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A batch (ADR-0050 §8) is the plan of a category, company and fiscal year
// on the centers its lines name. Every line is checked before anything is
// written; one wrong line refuses the batch, naming each wrong field. Its
// key, once applied with its lines (their digest), makes the same batch
// again change and publish nothing, even after later batches: a replay
// that arrives late does not undo them.

// maxLineErrors bounds the field errors of a refused batch.
const maxLineErrors = 100

// planLine is a batch's line, parsed.
type planLine struct {
	costCenter, profitCenter *uuid.UUID
	account                  uuid.UUID
	period                   int16
	amount                   money.Minor
	quantity, unit           *string   // quantity in its canonical form, six decimals
	source                   uuid.UUID // the plan line a copy's line was copied from (T93a)
}

// key is the line's center, account and period, once in a batch.
func (l planLine) key() string {
	if l.costCenter != nil {
		return fmt.Sprintf("cc|%s|%s|%d", l.costCenter, l.account, l.period)
	}
	return fmt.Sprintf("pc|%s|%s|%d", l.profitCenter, l.account, l.period)
}

// Batch is a batch as applied. A batch written by a copy of another
// category's plan (T93a) names that category, with its code, and the
// percentage its amounts were scaled by.
type Batch struct {
	ID, CategoryID, LegalEntityID  uuid.UUID
	FiscalYear                     int16
	Key, Digest                    string
	Lines, Replaced                int32
	CreatedAt                      time.Time
	CreatedBy                      uuid.UUID
	CopiedFrom                     *uuid.UUID
	CopiedFromCode, CopyPercentage *string
}

const batchColumns = `id, category_id, legal_entity_id, fiscal_year, batch_key, digest, line_count, replaced_count, created_at, created_by,
	copied_from_category_id, (SELECT c.code FROM controlling.plan_category c WHERE c.id = copied_from_category_id), trim_scale(copy_percentage)::text`

func scanBatch(r pgx.Row) (Batch, error) {
	var b Batch
	err := r.Scan(&b.ID, &b.CategoryID, &b.LegalEntityID, &b.FiscalYear, &b.Key, &b.Digest, &b.Lines, &b.Replaced, &b.CreatedAt, &b.CreatedBy,
		&b.CopiedFrom, &b.CopiedFromCode, &b.CopyPercentage)
	return b, errs.Wrapf(err, "controlling: scan plan batch")
}

// batchToAPI is a batch as the API shows it.
func batchToAPI(b Batch, replayed bool) gen.PlanBatch {
	return gen.PlanBatch{ID: b.ID, PlanCategoryID: b.CategoryID, LegalEntityID: b.LegalEntityID, FiscalYear: int32(b.FiscalYear), BatchKey: b.Key,
		LineCount: b.Lines, ReplacedCount: b.Replaced, Replayed: replayed, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy,
		CopiedFromPlanCategoryID: apitypes.OptUUID(b.CopiedFrom), CopiedFromPlanCategoryCode: apitypes.OptString(b.CopiedFromCode),
		CopyPercentage: apitypes.OptString(b.CopyPercentage)}
}

// lineRef names a batch's line in a refusal: lines[N] for an uploaded
// batch's, source_lines[<plan line id>] for a copy's.
type lineRef func(i int) string

// uploaded names the Nth line of an uploaded batch.
func uploaded(i int) string { return fmt.Sprintf("lines[%d]", i) }

func batchInvalid(fields ...problem.FieldError) error {
	return problem.Unprocessable("controlling.plan_batch_invalid", "The batch's lines are not a plan; nothing was written.",
		fields[:min(len(fields), maxLineErrors)]...)
}

func lineError(i int, field, message string) problem.FieldError {
	return refError(uploaded(i), field, message)
}

// refError is a wrong field of the line a refusal names by ref.
func refError(ref, field, message string) problem.FieldError {
	return problem.FieldError{Field: ref + "." + field, Code: "invalid", Message: message}
}

// ApplyPlanBatch implements POST /v1/plan-batches.
func (e *Endpoints) ApplyPlanBatch(ctx context.Context, req *gen.PlanBatchCreate, _ gen.ApplyPlanBatchParams) (gen.ApplyPlanBatchRes, error) {
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
	cat, err := getCategory(ctx, tx, req.PlanCategoryID, true)
	if missing := new(problem.Problem); errors.As(err, &missing) && missing.Status == 404 {
		return nil, problem.ReferenceNotFound("plan_category_id", "plan category", "no plan category with this id")
	}
	if err != nil {
		return nil, err
	}
	lines, err := parseLines(ctx, tx, req.Lines, le.LocalCurrency)
	if err != nil {
		return nil, err
	}
	b, replayed, err := e.apply(ctx, tx, p, cat, le, int16(req.FiscalYear), req.BatchKey, lines)
	if err != nil {
		return nil, err
	}
	return &gen.PlanBatchHeaders{Response: batchToAPI(b, replayed)}, nil
}

// apply applies the batch to the category, held FOR UPDATE: a batch of
// the key already applied is returned as it was (replayed), or refused
// with other lines; otherwise the category must be open, the lines name
// what the ledger allows, and they replace the plan of their centers.
func (e *Endpoints) apply(ctx context.Context, tx pgx.Tx, p reqctx.Principal, cat Category, le tenancy.Entity, year int16, key string, lines []planLine) (Batch, bool, error) {
	digest := digestOf(lines)
	b, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchColumns+` FROM controlling.plan_batch
		WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND batch_key = $4`, cat.ID, le.ID, year, key))
	switch {
	case err == nil && b.Digest == digest:
		return b, true, nil
	case err == nil:
		return Batch{}, false, problem.Conflict("controlling.plan_batch_reused",
			"The batch key was already applied to the category, legal entity and year with other lines; send a new key for a new plan.")
	case !errors.Is(err, pgx.ErrNoRows):
		return Batch{}, false, fmt.Errorf("controlling: read plan batch %s: %w", key, err)
	case cat.Locked:
		return Batch{}, false, categoryLocked()
	}
	b, err = e.replacePlan(ctx, tx, p, Batch{ID: uuid.Must(uuid.NewV7()), CategoryID: cat.ID, LegalEntityID: le.ID, FiscalYear: year, Key: key,
		Digest: digest, Lines: int32(len(lines))}, lines, planOf(lines), uploaded)
	return b, false, err
}

// replacePlan writes the batch: its lines must name what the ledger
// allows and change no cost center's plan in a period the center is
// locked for planning in; they then replace the plan of the centers given
// (the lines' own for an uploaded batch; for a copy also those of the
// target's current plan), and the batch is audited and published. The
// caller holds the batch's category FOR UPDATE.
func (e *Endpoints) replacePlan(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b Batch, lines []planLine, of planCenters, ref lineRef) (Batch, error) {
	locked, err := e.checkObjects(ctx, b.LegalEntityID, b.FiscalYear, lines, of, ref)
	if err != nil {
		return Batch{}, err
	}
	if err := checkPlanLocks(ctx, tx, b, lines, of.cost, locked, ref); err != nil {
		return Batch{}, err
	}
	b, err = write(ctx, tx, p, b, lines, of)
	if err != nil {
		return Batch{}, err
	}
	return b, recordBatch(ctx, b, of)
}

// parseLines reads the lines: one center each, the amount in the legal
// entity's currency with its digits, a positive quantity of a known unit,
// and each center, account and period once.
func parseLines(ctx context.Context, tx pgx.Tx, in []gen.PlanBatchLine, currencyLC string) ([]planLine, error) {
	exp, err := currency.Exponent(ctx, tx, currencyLC, nil)
	if err != nil {
		return nil, err
	}
	units, err := knownUnits(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	out := make([]planLine, len(in))
	var bad []problem.FieldError
	seen := map[string]int{}
	for i, l := range in {
		var lineBad []problem.FieldError
		out[i], lineBad = parseLine(i, l, currencyLC, exp, units)
		if first, ok := seen[out[i].key()]; ok && len(lineBad) == 0 {
			lineBad = append(lineBad, lineError(i, "account_id", fmt.Sprintf("the center, account and period once in the batch; line %d has them", first)))
		}
		seen[out[i].key()] = i
		bad = append(bad, lineBad...)
	}
	if len(bad) > 0 {
		return nil, batchInvalid(bad...)
	}
	return out, nil
}

// parseLine reads one line, reporting each wrong field.
func parseLine(i int, l gen.PlanBatchLine, currencyLC string, exp int, units map[string]bool) (planLine, []problem.FieldError) {
	out := planLine{costCenter: apitypes.Ptr[uuid.UUID](l.CostCenterID), profitCenter: apitypes.Ptr[uuid.UUID](l.ProfitCenterID), account: l.AccountID,
		period: int16(l.FiscalPeriod)}
	var bad []problem.FieldError
	if (out.costCenter == nil) == (out.profitCenter == nil) {
		bad = append(bad, lineError(i, "cost_center_id", "a cost center or a profit center, not both"))
	}
	if l.Amount.Currency != currencyLC {
		bad = append(bad, lineError(i, "amount.currency", "the legal entity's local currency, "+currencyLC))
	}
	amount, err := money.ParseAmount(l.Amount.Amount, exp)
	if err != nil {
		bad = append(bad, lineError(i, "amount.amount", fmt.Sprintf("an amount with exactly %d decimals", exp)))
	}
	out.amount = amount
	if q, ok := l.Quantity.Get(); ok {
		if money.PositiveQuantity(q.Value) != nil {
			bad = append(bad, lineError(i, "quantity.value", "a positive decimal of at most six decimals"))
		} else {
			r, _ := money.ParseDecimalRat(q.Value) // PositiveQuantity accepted it
			canonical := r.FloatString(money.QuantityDecimals)
			out.quantity, out.unit = &canonical, &q.Unit
		}
		if !units[q.Unit] {
			bad = append(bad, lineError(i, "quantity.unit", "a unit of measure"))
		}
	}
	return out, bad
}

// knownUnits are the units of measure the lines' quantities name that
// exist (reference data).
func knownUnits(ctx context.Context, tx pgx.Tx, in []gen.PlanBatchLine) (map[string]bool, error) {
	var named []string
	for _, l := range in {
		if q, ok := l.Quantity.Get(); ok {
			named = append(named, q.Unit)
		}
	}
	out := map[string]bool{}
	if len(named) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, `SELECT code FROM ref.unit_of_measure WHERE code = ANY($1)`, named)
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("controlling: units of measure: %w", err)
	}
	for _, c := range codes {
		out[c] = true
	}
	return out, nil
}

// digestOf is the lines' digest, whatever their order.
func digestOf(lines []planLine) string {
	keys := make([]string, len(lines))
	for i, l := range lines {
		q := "-"
		if l.quantity != nil {
			q = *l.quantity + " " + *l.unit
		}
		keys[i] = fmt.Sprintf("%s|%d|%s", l.key(), l.amount, q)
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}

// planCenters are the cost centers and profit centers whose plan a batch
// replaces, each once, in the order they were added.
type planCenters struct {
	cost, profit []uuid.UUID
	seen         map[uuid.UUID]bool
}

func newPlanCenters() planCenters {
	return planCenters{cost: []uuid.UUID{}, profit: []uuid.UUID{}, seen: map[uuid.UUID]bool{}}
}

// add adds a line's or a current plan's center, once.
func (c *planCenters) add(costCenter, profitCenter *uuid.UUID) {
	if costCenter != nil && !c.seen[*costCenter] {
		c.seen[*costCenter] = true
		c.cost = append(c.cost, *costCenter)
	}
	if profitCenter != nil && !c.seen[*profitCenter] {
		c.seen[*profitCenter] = true
		c.profit = append(c.profit, *profitCenter)
	}
}

// planOf are the lines' cost centers and profit centers, in the lines'
// order.
func planOf(lines []planLine) planCenters {
	c := newPlanCenters()
	for _, l := range lines {
		c.add(l.costCenter, l.profitCenter)
	}
	return c
}

// checkObjects refuses lines naming a center, account or period the
// ledger does not allow a plan of the legal entity's year to name, and
// returns the plan locks (PlanObjects.PlanLocked) of the cost centers
// whose plan the batch replaces. The ledger holds those cost centers
// until the batch commits, so no plan lock is applied between the check
// (checkPlanLocks) and the lines' write.
func (e *Endpoints) checkObjects(ctx context.Context, le uuid.UUID, year int16, lines []planLine, of planCenters, ref lineRef) (map[uuid.UUID]map[int16]bool, error) {
	accounts := []uuid.UUID{}
	for _, l := range lines {
		accounts = append(accounts, l.account)
	}
	ok, err := e.Ledger.PlanObjects(ctx, le, year, of.cost, of.profit, accounts)
	if err != nil {
		return nil, err
	}
	var bad []problem.FieldError
	for i, l := range lines {
		bad = append(bad, objectErrors(ref(i), l, ok)...)
	}
	if len(bad) > 0 {
		return nil, batchInvalid(bad...)
	}
	return ok.PlanLocked, nil
}

// checkPlanLocks refuses a batch that would change a cost center's plan in
// a period in which the center is locked for planning (T56g2, ADR-0050
// §8): a line it writes there, or a current line there that it would mark
// replaced, since a batch replaces its centers' plan (costCenters) in
// every period of the year. A replaced line is named by the batch's first
// line on its center; a copy also replaces the plan of centers it has no
// line of, which its refusal names as replaced_cost_centers[<id>]. The category is held FOR
// UPDATE, so no other batch changes its current lines meanwhile.
func checkPlanLocks(ctx context.Context, tx pgx.Tx, b Batch, lines []planLine, costCenters []uuid.UUID, locked map[uuid.UUID]map[int16]bool, ref lineRef) error {
	var refused []problem.FieldError
	first, written := map[uuid.UUID]int{}, map[string]bool{}
	for i, l := range lines {
		if l.costCenter == nil {
			continue
		}
		if _, ok := first[*l.costCenter]; !ok {
			first[*l.costCenter] = i
		}
		if locked[*l.costCenter][l.period] {
			written[fmt.Sprint(*l.costCenter, l.period)] = true
			refused = append(refused, planLockedField(ref(i)+".cost_center_id", "a cost center not locked for planning in the period"))
		}
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT cost_center_id, fiscal_period FROM controlling.plan_line WHERE category_id = $1 AND legal_entity_id = $2
		AND fiscal_year = $3 AND cost_center_id = ANY($4) AND replaced_by_batch_id IS NULL ORDER BY cost_center_id, fiscal_period`,
		b.CategoryID, b.LegalEntityID, b.FiscalYear, costCenters)
	var center uuid.UUID
	var period int16
	if _, err := pgx.ForEachRow(rows, []any{&center, &period}, func() error {
		// A period the batch writes in is refused on its own lines already.
		if !locked[center][period] || written[fmt.Sprint(center, period)] {
			return nil
		}
		if i, ok := first[center]; ok {
			refused = append(refused, planLockedField(ref(i)+".cost_center_id", fmt.Sprintf("a cost center without a current plan in period %d, "+
				"locked for planning, which the batch would replace", period)))
			return nil
		}
		refused = append(refused, planLockedField(fmt.Sprintf("replaced_cost_centers[%s]", center), fmt.Sprintf("a target without a current plan "+
			"of the cost center in period %d, locked for planning, which the copy would replace", period)))
		return nil
	}); err != nil {
		return fmt.Errorf("controlling: current plan of the batch's cost centers: %w", err)
	}
	if len(refused) > 0 {
		return planLocked(refused...)
	}
	return nil
}

func planLockedField(field, message string) problem.FieldError {
	return problem.FieldError{Field: field, Code: "plan_locked", Message: message}
}

// planLocked refuses a batch changing a cost center's plan in a period in
// which the center's lock_plan is set (ADR-0050 §3, §8): unprocessable as
// the ledger's refusal of a line on a center locked for actual costs,
// naming each line.
func planLocked(fields ...problem.FieldError) error {
	return problem.Unprocessable("controlling.cost_center_plan_locked", "The batch would change a cost center's plan in a period the center is "+
		"locked for planning in; nothing was written. A center change lifts the lock first.", fields[:min(len(fields), maxLineErrors)]...)
}

func objectErrors(ref string, l planLine, ok ledger.PlanObjects) []problem.FieldError {
	var bad []problem.FieldError
	if l.costCenter != nil && !ok.CostCenters[*l.costCenter] {
		bad = append(bad, refError(ref, "cost_center_id", "an active cost center of the legal entity"))
	}
	if l.profitCenter != nil && !ok.ProfitCenters[*l.profitCenter] {
		bad = append(bad, refError(ref, "profit_center_id", "an active profit center assigned to the legal entity"))
	}
	if !ok.Accounts[l.account] {
		bad = append(bad, refError(ref, "account_id", "a profit and loss account set up in the legal entity"))
	}
	if !ok.Periods[l.period] {
		bad = append(bad, refError(ref, "fiscal_period", "a period of the fiscal year in the legal entity's calendar"))
	}
	return bad
}
