package mrp

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// S&OP plan versions (ADR-0084 As built S2n): a planner starts a review
// of a company, a draft version counted on the company's day
// (sopreview.go), sets the consensus of its lines at its version, and
// fixes it, after which neither the version nor its lines change (the
// database refuses it). Each step is audited; the version's row lock
// orders a change of consensus against fixing.

// Plan version statuses.
const (
	SopDraft = "draft"
	SopFixed = "fixed"
)

// SopPlan is a stored S&OP plan version with its lines.
type SopPlan struct {
	ID, LegalEntityID       uuid.UUID
	Number                  int32
	Status                  string
	AsOf, FirstDay, LastDay time.Time
	Months                  int16
	FixedAt                 *time.Time
	FixedBy                 *uuid.UUID
	Version                 int32
	CreatedAt, UpdatedAt    time.Time
	CreatedBy, UpdatedBy    uuid.UUID
	Lines                   []SopLine
}

// SopLine is a version's line of a material group in its base unit
// (Unit) and a month, as plain decimals in that unit; Stock is the plants'
// stock in the first month and zero after.
type SopLine struct {
	Group, Unit                                                   string
	Month, FirstDay, LastDay                                      time.Time
	Materials                                                     int32
	Forecast, Orders, Stock, PurchaseOrders, StockTransports string
	Proposals, Consensus                                          string
}

// Key is the line's key, group/unit/month, by which a change names it.
func (l SopLine) Key() string { return l.Group + "/" + l.Unit + "/" + l.Month.Format("2006-01") }

// sopPath is a version's resource path.
func sopPath(id uuid.UUID) string { return "/v1/sop-plans/" + id.String() }

const sopSelect = `SELECT id, legal_entity_id, number, status, as_of, first_day, last_day, months, fixed_at, fixed_by, version, created_at, updated_at,
		created_by, updated_by
	FROM mrp.sop_plan`

func scanSop(row pgx.Row) (SopPlan, error) {
	var p SopPlan
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.Number, &p.Status, &p.AsOf, &p.FirstDay, &p.LastDay, &p.Months, &p.FixedAt, &p.FixedBy, &p.Version,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &p.UpdatedBy)
	return p, errs.Wrapf(err, "scan S&OP plan")
}

// loadSop reads a version of a company the caller may act in with its
// lines, under its row lock with lock; another company's is not found.
func loadSop(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (SopPlan, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return SopPlan{}, err
	}
	q := sopSelect + ` WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	p, err := scanSop(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, p.LegalEntityID, problem.NotFoundOf("S&OP plan"), "mrp: S&OP plan %s", id); err != nil {
		return SopPlan{}, err
	}
	ps := []SopPlan{p}
	if err := withSopLines(ctx, tx, ps); err != nil {
		return SopPlan{}, err
	}
	return ps[0], nil
}

// withSopLines reads the versions' lines, by group, unit and month, into
// them.
func withSopLines(ctx context.Context, tx pgx.Tx, ps []SopPlan) error {
	ids := make([]uuid.UUID, len(ps))
	at := make(map[uuid.UUID]*SopPlan, len(ps))
	for i := range ps {
		ids[i], at[ps[i].ID] = ps[i].ID, &ps[i]
	}
	rows, _ := tx.Query(ctx, `SELECT plan_id, material_group, uom_code, month, first_day, last_day, materials, trim_scale(forecast)::text,
			trim_scale(orders)::text, trim_scale(stock)::text, trim_scale(purchase_orders)::text, trim_scale(stock_transports)::text,
			trim_scale(proposals)::text, trim_scale(consensus)::text
		FROM mrp.sop_plan_line WHERE plan_id = ANY($1) ORDER BY plan_id, material_group COLLATE "C", uom_code COLLATE "C", month`, ids)
	var plan uuid.UUID
	var l SopLine
	_, err := pgx.ForEachRow(rows, []any{&plan, &l.Group, &l.Unit, &l.Month, &l.FirstDay, &l.LastDay, &l.Materials, &l.Forecast, &l.Orders,
		&l.Stock, &l.PurchaseOrders, &l.StockTransports, &l.Proposals, &l.Consensus}, func() error {
		at[plan].Lines = append(at[plan].Lines, l)
		return nil
	})
	return errs.Wrapf(err, "mrp: lines of %d S&OP plans", len(ps))
}

// SopInput is an S&OP review as a request starts it; Months 0 is
// DefaultMonths.
type SopInput struct {
	LegalEntityID uuid.UUID
	Months        int
}

// company reads an active company the caller may act in (422
// mrp.company_invalid otherwise).
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, err
	}
	le, ok, err := e.Plants.Entity(ctx, id)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("mrp: company %s: %w", id, err)
	}
	if !ok || le.Status != tenancy.Active || !access.Allows(id) {
		return tenancy.Entity{}, problem.Invalid("mrp.company_invalid", "legal_entity_id", "No active company with this id you may act in.")
	}
	return le, nil
}

// StartSop starts an S&OP review of a company: an active company the
// caller may act in, a horizon of 1 to MaxMonths months (422
// mrp.invalid_horizon), counted on the company's day (sopreview.go) into
// a draft version numbered next in the company, its consensus the
// forecast. Audited.
func (e *Endpoints) StartSop(ctx context.Context, in SopInput) (SopPlan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SopPlan{}, err
	}
	months := cmp.Or(in.Months, DefaultMonths)
	if months < 1 || months > MaxMonths {
		return SopPlan{}, problem.Invalid("mrp.invalid_horizon", "months", fmt.Sprintf("From 1 to %d months.", MaxMonths))
	}
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return SopPlan{}, err
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return SopPlan{}, fmt.Errorf("mrp: day of company %s: %w", le.Code, err)
	}
	lines, err := e.countReview(ctx, tx, le, day.Date, months)
	if err != nil {
		return SopPlan{}, err
	}
	id := uuid.Must(uuid.NewV7())
	b := horizon(day.Date, months)
	var number int32
	if err := tx.QueryRow(ctx, `INSERT INTO mrp.sop_plan (tenant_id, id, legal_entity_id, number, as_of, first_day, last_day, months, created_by, updated_by)
		SELECT $1, $2, $3, coalesce(max(number), 0) + 1, $4, $5, $6, $7, $8, $8 FROM mrp.sop_plan WHERE legal_entity_id = $3 RETURNING number`,
		p.TenantID, id, le.ID, day.Date, b[0].first, b[len(b)-1].last, months, p.ID).Scan(&number); err != nil {
		return SopPlan{}, fmt.Errorf("mrp: S&OP plan of company %s: %w", le.Code, err)
	}
	if err := writeSopLines(ctx, tx, p.TenantID, id, lines); err != nil {
		return SopPlan{}, err
	}
	out, err := loadSop(ctx, tx, id, false)
	if err != nil {
		return SopPlan{}, err
	}
	return out, audit.Record(ctx, "mrp.sop_plan.create", "mrp.sop_plan", id, map[string]any{"legal_entity_id": le.ID, "company_code": le.Code,
		"number": number, "as_of": day.Date.Format(time.DateOnly), "months": months, "lines": len(lines)})
}

// writeSopLines writes a version's lines.
func writeSopLines(ctx context.Context, tx pgx.Tx, tenant, plan uuid.UUID, lines []SopLine) error {
	n := len(lines)
	groups, units, nums := make([]string, n), make([]string, n), make([]int32, n)
	days := [3][]time.Time{make([]time.Time, n), make([]time.Time, n), make([]time.Time, n)}
	q := [7][]string{}
	for k := range q {
		q[k] = make([]string, n)
	}
	for i, l := range lines {
		groups[i], units[i], nums[i] = l.Group, l.Unit, l.Materials
		days[0][i], days[1][i], days[2][i] = l.Month, l.FirstDay, l.LastDay
		for k, v := range []string{l.Forecast, l.Orders, l.Stock, l.PurchaseOrders, l.StockTransports, l.Proposals, l.Consensus} {
			q[k][i] = v
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO mrp.sop_plan_line (tenant_id, plan_id, material_group, uom_code, month, first_day, last_day, materials, forecast,
			orders, stock, purchase_orders, stock_transports, proposals, consensus)
		SELECT $1, $2, g, u, m, f, l, n, fc::numeric, so::numeric, st::numeric, po::numeric, tr::numeric, pr::numeric, co::numeric
		FROM unnest($3::text[], $4::text[], $5::date[], $6::date[], $7::date[], $8::int[], $9::text[], $10::text[], $11::text[], $12::text[], $13::text[],
			$14::text[], $15::text[]) AS t(g, u, m, f, l, n, fc, so, st, po, tr, pr, co)`,
		tenant, plan, groups, units, days[0], days[1], days[2], nums, q[0], q[1], q[2], q[3], q[4], q[5], q[6])
	return errs.Wrapf(err, "mrp: lines of S&OP plan %s", plan)
}

// draft reads a version under its row lock at the caller's version (412
// otherwise) and refuses one fixed already (409 mrp.sop_plan_fixed).
func draft(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (SopPlan, error) {
	p, err := loadSop(ctx, tx, id, true)
	if err != nil {
		return SopPlan{}, err
	}
	if err := ifMatch(p.Version); err != nil {
		return SopPlan{}, err
	}
	if p.Status != SopDraft {
		return SopPlan{}, problem.Conflict("mrp.sop_plan_fixed", fmt.Sprintf("S&OP plan %d is %s, so it changes no more; start a new review "+
			"(POST /v1/sop-plans).", p.Number, p.Status))
	}
	return p, nil
}

// consensusChange is a line's consensus before and after a change.
type consensusChange struct {
	line  SopLine
	after string
}

// changes checks the consensus a change sets, by key in key order: each a
// line of the version (422 mrp.sop_line_unknown) and a quantity of zero or
// more (422 mrp.invalid_quantity), naming the key; it is the lines whose
// consensus moves, none being 422 mrp.nothing_to_change.
func (p SopPlan) changes(consensus map[string]string) ([]consensusChange, error) {
	lines := make(map[string]SopLine, len(p.Lines))
	for _, l := range p.Lines {
		lines[l.Key()] = l
	}
	keys := make([]string, 0, len(consensus))
	for k := range consensus {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []consensusChange
	for _, k := range keys {
		l, ok := lines[k]
		switch {
		case !ok:
			return nil, problem.Invalid("mrp.sop_line_unknown", "consensus."+k, "No line of the plan with this key (group/unit/YYYY-MM).")
		case money.QuantityOrZero(consensus[k]) != nil:
			return nil, problem.Invalid("mrp.invalid_quantity", "consensus."+k, fmt.Sprintf("zero or a plain decimal of at most %d integer digits "+
				"and %d decimals", money.QuantityIntDigits, money.QuantityDecimals))
		case !sameQuantity(l.Consensus, consensus[k]):
			out = append(out, consensusChange{line: l, after: consensus[k]})
		}
	}
	if len(out) == 0 {
		return nil, problem.Unprocessable("mrp.nothing_to_change", "The lines' consensus is so already.")
	}
	return out, nil
}

// SetConsensus sets the consensus of a draft version's lines by key at
// its version (412 otherwise; 409 mrp.sop_plan_fixed once fixed), each
// checked (changes). Audited with each line's consensus before and after.
func (e *Endpoints) SetConsensus(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, consensus map[string]string) (SopPlan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SopPlan{}, err
	}
	before, err := draft(ctx, tx, id, ifMatch)
	if err != nil {
		return SopPlan{}, err
	}
	ch, err := before.changes(consensus)
	if err != nil {
		return SopPlan{}, err
	}
	groups, units, months, values := make([]string, len(ch)), make([]string, len(ch)), make([]time.Time, len(ch)), make([]string, len(ch))
	moved := make([]map[string]any, len(ch))
	for i, c := range ch {
		groups[i], units[i], months[i], values[i] = c.line.Group, c.line.Unit, c.line.Month, c.after
		moved[i] = map[string]any{"key": c.line.Key(), "before": c.line.Consensus, "after": money.PlainQuantity(money.ParseRat(c.after))}
	}
	if _, err := tx.Exec(ctx, `UPDATE mrp.sop_plan_line l SET consensus = c.v::numeric
		FROM unnest($2::text[], $3::text[], $4::date[], $5::text[]) AS c(g, u, m, v)
		WHERE l.plan_id = $1 AND l.material_group = c.g AND l.uom_code = c.u AND l.month = c.m`, id, groups, units, months, values); err != nil {
		return SopPlan{}, fmt.Errorf("mrp: consensus of S&OP plan %d: %w", before.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE mrp.sop_plan SET version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return SopPlan{}, fmt.Errorf("mrp: change S&OP plan %d: %w", before.Number, err)
	}
	out, err := loadSop(ctx, tx, id, false)
	if err != nil {
		return SopPlan{}, err
	}
	return out, audit.Record(ctx, "mrp.sop_plan.consensus", "mrp.sop_plan", id, map[string]any{"number": before.Number, "lines": moved})
}

// FixSop fixes a draft version at its version (412 otherwise; 409
// mrp.sop_plan_fixed once fixed): fixed, naming who fixed it and when,
// it changes no more. Audited.
func (e *Endpoints) FixSop(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (SopPlan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return SopPlan{}, err
	}
	before, err := draft(ctx, tx, id, ifMatch)
	if err != nil {
		return SopPlan{}, err
	}
	// fixed_at is the instant, as created_at is, by the database's clock;
	// the company's day (as_of) is a business date, by e.Now.
	if _, err := tx.Exec(ctx, `UPDATE mrp.sop_plan SET status = 'fixed', fixed_at = now(), fixed_by = $2, version = version + 1, updated_by = $2
		WHERE id = $1`, id, p.ID); err != nil {
		return SopPlan{}, fmt.Errorf("mrp: fix S&OP plan %d: %w", before.Number, err)
	}
	out, err := loadSop(ctx, tx, id, false)
	if err != nil {
		return SopPlan{}, err
	}
	return out, audit.Record(ctx, "mrp.sop_plan.fix", "mrp.sop_plan", id, map[string]any{"legal_entity_id": before.LegalEntityID,
		"number": before.Number, "lines": len(before.Lines)})
}
