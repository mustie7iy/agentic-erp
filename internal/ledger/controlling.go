package ledger

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The configuration's controlling rules (ADR-0050 §4, T56c) as the ledger
// keeps them: each account's cost element (applyAccounts), the tenant's
// cost center categories, and per company the center accounting switches,
// the default profit center and the default cost centers by account.
// Check and Apply read the same state (readControlling): Check reports what
// is wrong as findings, Apply refuses the activation with them, since the
// tenant's centers and postings may have changed while the change request
// waited for approval.

// companyControlling is a company's controlling as the ledger has it and
// as the document asks for it, read for one check or application.
type companyControlling struct {
	today companyDay // the company's date in its time zone (T72n4)
	year  int        // the fiscal year today is in
	// costFrom and profitFrom are the fiscal years cost and profit center
	// accounting apply from, nil while off.
	costFrom, profitFrom *int16
	postedSince          bool // the company has postings in year or later
	// costCenters are the default cost centers by code that may be used,
	// profitCenter the default profit center (uuid.Nil: none).
	costCenters  map[string]uuid.UUID
	profitCenter uuid.UUID
}

// readControlling reads the company's controlling and adds a finding for
// each rule the document breaks against the tenant's data: center
// accounting turned off, or turned on while the company has postings in
// the current fiscal year or later; a default cost center or profit center
// that is unknown, not active, not the company's, not valid from today
// with no end, or locked from today on. lock takes the rows read for the
// application: the company's period statuses from the current year on, so
// postings under way finish first and later ones see the switch, and the
// centers for share, so they do not change before the transaction ends.
// Today is the company's date in the time zone the document gives it
// (T72n4, dayOfCompany), so the switches' year and the defaults' validity
// follow the company's calendar, not UTC's.
func readControlling(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, today companyDay, lock bool,
	out *findings) (companyControlling, error) {
	c := companyControlling{costCenters: map[string]uuid.UUID{}, today: today, year: fiscalYearOf(today.day, co.FiscalYearStartMonth)}
	err := tx.QueryRow(ctx, `SELECT (SELECT cost_center_accounting_from FROM ledger.company_controlling WHERE legal_entity_id = $1),
			(SELECT profit_center_accounting_from FROM ledger.company_controlling WHERE legal_entity_id = $1)`, le).Scan(&c.costFrom, &c.profitFrom)
	if err != nil {
		return c, fmt.Errorf("ledger: controlling of company %s: %w", co.Code, err)
	}
	if err := c.checkSwitches(ctx, tx, p, le, co, lock, out); err != nil {
		return c, err
	}
	if err := c.checkDefaultCostCenters(ctx, tx, p, le, co, lock, out); err != nil {
		return c, err
	}
	return c, c.checkDefaultProfitCenter(ctx, tx, p, le, co, lock, out)
}

// fiscalYearOf is the fiscal year a date is in, for a calendar whose years
// start in month m: named after the calendar year it ends in (fiscalYear).
func fiscalYearOf(d time.Time, m int) int {
	if m > 1 && int(d.Month()) >= m {
		return d.Year() + 1
	}
	return d.Year()
}

// checkSwitches reports center accounting turned off, or turned on while
// the company has postings in the current fiscal year or later: a year's
// P&L by center is complete or not kept (ADR-0050 §4).
func (c *companyControlling) checkSwitches(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, lock bool, out *findings) error {
	switches := []struct {
		field string
		on    bool
		from  *int16
	}{{"cost_center_accounting", co.CostCenterAccounting, c.costFrom}, {"profit_center_accounting", co.ProfitCenterAccounting, c.profitFrom}}
	turning := false
	for _, s := range switches {
		if s.from != nil && !s.on {
			out.add(p+"."+s.field, "%s is on since fiscal year %d and stays on, so every year's P&L by center is complete",
				strings.ReplaceAll(s.field, "_", " "), *s.from)
		}
		turning = turning || (s.from == nil && s.on)
	}
	if !turning {
		return nil
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year >= $2 FOR UPDATE`, le, c.year); err != nil {
			return fmt.Errorf("ledger: controlling of company %s: lock periods: %w", co.Code, err)
		}
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.journal_line WHERE legal_entity_id = $1 AND fiscal_year >= $2)`,
		le, c.year).Scan(&c.postedSince); err != nil {
		return fmt.Errorf("ledger: controlling of company %s: postings since %d: %w", co.Code, c.year, err)
	}
	for _, s := range switches {
		if s.from == nil && s.on && c.postedSince {
			out.add(p+"."+s.field, "the company has postings in fiscal year %d or later; turn %s on before the first posting of a fiscal year, "+
				"so the year's P&L by center is complete", c.year, strings.ReplaceAll(s.field, "_", " "))
		}
	}
	return nil
}

// checkDefaultCostCenters reports each default assignment whose cost center
// automatic postings could not use: one that is not an active cost center
// of the company, not valid from today with no end, or locked for actual
// costs from today on.
func (c *companyControlling) checkDefaultCostCenters(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, lock bool,
	out *findings) error {
	if len(co.DefaultAssignments) == 0 {
		return nil
	}
	codes := make([]string, len(co.DefaultAssignments))
	for i, d := range co.DefaultAssignments {
		codes[i] = d.CostCenter
	}
	rows, _ := tx.Query(ctx, `SELECT code, id FROM ledger.cost_center c WHERE legal_entity_id = $1 AND code = ANY($2) AND status = 'active'
		ORDER BY id`+rowLock(lock), le, codes)
	byCode := map[string]uuid.UUID{}
	var code string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&code, &id}, func() error { byCode[code] = id; return nil }); err != nil {
		return fmt.Errorf("ledger: default cost centers of company %s: %w", co.Code, err)
	}
	periods, err := costCenterPeriods(ctx, tx, slices.Collect(maps.Values(byCode)), noLock)
	if err != nil {
		return err
	}
	for _, d := range co.DefaultAssignments {
		id, ok := byCode[d.CostCenter]
		why := "is not an active cost center of the company"
		if ok {
			why = unusableDefault(periods[id], c.today, func(a CostCenterAttrs) bool { return a.LockCost })
		}
		if why != "" {
			out.add(p+".default_assignments["+d.Account+"].cost_center", "cost center %s %s; lines on account %s would have no usable default",
				d.CostCenter, why, d.Account)
			continue
		}
		c.costCenters[d.CostCenter] = id
	}
	return nil
}

// checkDefaultProfitCenter reports a default profit center that is not an
// active profit center assigned to the company, valid from today with no
// end and unlocked from today on.
func (c *companyControlling) checkDefaultProfitCenter(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, lock bool,
	out *findings) error {
	if co.DefaultProfitCenter == "" {
		return nil
	}
	rows, _ := tx.Query(ctx, `SELECT id FROM ledger.profit_center WHERE code = $1 AND status = 'active'`, co.DefaultProfitCenter)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("ledger: default profit center of company %s: %w", co.Code, err)
	}
	why := "is not an active profit center"
	if len(ids) == 1 {
		pc, err := getProfitCenter(ctx, tx, ids[0], rowLock(lock))
		if err != nil {
			return err
		}
		why = "is not assigned to the company"
		if slices.Contains(pc.LegalEntityIDs, le) {
			why = unusableDefault(pc.Periods, c.today, func(a ProfitCenterAttrs) bool { return a.Locked })
		}
		c.profitCenter = pc.ID
	}
	if why != "" {
		out.add(p+".default_profit_center", "profit center %s %s; the P&L lines it takes would be refused", co.DefaultProfitCenter, why)
		c.profitCenter = uuid.Nil
	}
	return nil
}

// rowLock is the lock readControlling takes on the default centers: for
// share when applying, so they do not change before the activation
// commits; none when checking.
func rowLock(lock bool) string {
	if lock {
		return forShare
	}
	return noLock
}

// unusableDefault says why a center with the spans cannot be a default from
// today on, "" when it can: a default must be valid from today with no end
// and not locked on any day from today, so automatic postings never meet a
// center they cannot use (ADR-0050 §4). Today is the company's date in its
// time zone (T72n4), which the reason names.
func unusableDefault[A comparable](spans []span[A], today companyDay, locked func(A) bool) string {
	if !within(spans, today.day, time.Time{}) {
		return "is not valid from " + today.text() + " with no end date"
	}
	for _, s := range spans {
		if endOf(s.To).After(today.day) && locked(s.Attr) {
			if s.From.After(today.day) {
				return "is locked from " + dateText(s.From) + ", after " + today.text()
			}
			return "is locked from " + today.text()
		}
	}
	return ""
}

// checkCategories reports the categories the document removes while an
// active cost center period has them: a cost center keeps its category
// until a move gives it another.
func checkCategories(ctx context.Context, tx pgx.Tx, doc *config.Config, out *findings) error {
	codes := []string{}
	for _, cat := range doc.Controlling.CostCenterCategories {
		codes = append(codes, cat.Code)
	}
	rows, _ := tx.Query(ctx, `SELECT p.category, string_agg(DISTINCT c.code, ', ' ORDER BY c.code) FROM ledger.cost_center_period p
		JOIN ledger.cost_center c ON c.tenant_id = p.tenant_id AND c.id = p.cost_center_id
		WHERE p.retired_by_change_id IS NULL AND p.category <> ALL($1) GROUP BY 1 ORDER BY 1`, codes)
	var category, centers string
	_, err := pgx.ForEachRow(rows, []any{&category, &centers}, func() error {
		out.add("controlling.cost_center_categories", "category %s is the category of cost centers %s; move them to another category first",
			category, centers)
		return nil
	})
	return errs.Wrapf(err, "ledger: cost center categories in use")
}

// applyCategories makes the tenant's cost center categories the
// document's, deactivating the ones it no longer has. It refuses the
// activation when a category removed is in use (checkCategories), checked
// after the deactivation: that waits for a center change holding the
// category for share, and the check's later statement then sees the
// period it wrote.
func applyCategories(ctx context.Context, tx pgx.Tx, p reqctx.Principal, doc *config.Config) error {
	cats := doc.Controlling.CostCenterCategories
	n := len(cats)
	codes, names := make([]string, n), make([]string, n)
	revenue, cost, rev, plan := make([]bool, n), make([]bool, n), make([]bool, n), make([]bool, n)
	for i, c := range cats {
		codes[i], names[i], revenue[i], cost[i], rev[i], plan[i] = c.Code, c.Name, c.RevenueAllowed, c.LockActualCost, c.LockActualRevenue, c.LockPlan
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.cost_center_category SET is_active = false, version = version + 1, updated_by = $2
		WHERE is_active AND code <> ALL($1)`, codes, p.ID); err != nil {
		return fmt.Errorf("ledger: deactivate cost center categories: %w", err)
	}
	var f findings
	if err := checkCategories(ctx, tx, doc, &f); err != nil {
		return err
	}
	if err := refused(f); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.cost_center_category AS k (tenant_id, code, name, revenue_allowed, lock_actual_cost,
			lock_actual_revenue, lock_plan, created_by, updated_by)
		SELECT $1, c, n, r, lc, lr, lp, $2, $2 FROM unnest($3::text[], $4::text[], $5::bool[], $6::bool[], $7::bool[], $8::bool[]) AS d(c, n, r, lc, lr, lp)
		ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name, revenue_allowed = EXCLUDED.revenue_allowed,
			lock_actual_cost = EXCLUDED.lock_actual_cost, lock_actual_revenue = EXCLUDED.lock_actual_revenue, lock_plan = EXCLUDED.lock_plan,
			is_active = true, version = k.version + 1, updated_by = $2
		WHERE (k.name, k.revenue_allowed, k.lock_actual_cost, k.lock_actual_revenue, k.lock_plan, k.is_active)
			IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.revenue_allowed, EXCLUDED.lock_actual_cost, EXCLUDED.lock_actual_revenue, EXCLUDED.lock_plan, true)`,
		p.TenantID, p.ID, codes, names, revenue, cost, rev, plan); err != nil {
		return fmt.Errorf("ledger: cost center categories: %w", err)
	}
	return nil
}

// refused is the activation's refusal for findings Check would have
// reported: the tenant's data changed while the change waited (409
// ledger.controlling_invalid, naming each).
func refused(f findings) error {
	if len(f) == 0 {
		return nil
	}
	msgs := make([]string, len(f))
	for i, x := range f {
		msgs[i] = x.Path + ": " + x.Message
	}
	return problem.Conflict("ledger.controlling_invalid", "The configuration's controlling rules no longer fit the ledger's data: "+
		strings.Join(msgs, "; ")+". Propose the change again.")
}

// applyControlling makes the company's controlling the document's: center
// accounting turned on from the current fiscal year, the default profit
// center and the default cost centers by account, deactivating the
// defaults it no longer has. It refuses the activation when the document
// no longer fits the tenant's data (readControlling).
func applyControlling(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company, today companyDay) error {
	var f findings
	c, err := readControlling(ctx, tx, "companies["+co.Code+"]", le, co, today, true, &f)
	if err != nil {
		return err
	}
	if err := refused(f); err != nil {
		return err
	}
	from := func(on bool, was *int16) *int16 {
		if !on {
			return nil
		}
		if was != nil {
			return was
		}
		y := int16(c.year) // a fiscal year, 1900-2999
		return &y
	}
	var pc *uuid.UUID
	if c.profitCenter != uuid.Nil {
		pc = &c.profitCenter
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.company_controlling AS k (tenant_id, legal_entity_id, cost_center_accounting_from,
			profit_center_accounting_from, default_profit_center_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (tenant_id, legal_entity_id) DO UPDATE SET cost_center_accounting_from = EXCLUDED.cost_center_accounting_from,
			profit_center_accounting_from = EXCLUDED.profit_center_accounting_from, default_profit_center_id = EXCLUDED.default_profit_center_id,
			version = k.version + 1, updated_by = $6
		WHERE (k.cost_center_accounting_from, k.profit_center_accounting_from, k.default_profit_center_id)
			IS DISTINCT FROM (EXCLUDED.cost_center_accounting_from, EXCLUDED.profit_center_accounting_from, EXCLUDED.default_profit_center_id)`,
		p.TenantID, le, from(co.CostCenterAccounting, c.costFrom), from(co.ProfitCenterAccounting, c.profitFrom), pc, p.ID); err != nil {
		return fmt.Errorf("ledger: apply company %s: controlling: %w", co.Code, err)
	}
	return applyDefaultAssignments(ctx, tx, p, le, chart, co, c.costCenters)
}

// applyDefaultAssignments makes the company's default cost centers by
// account the document's, the cost centers resolved by readControlling.
func applyDefaultAssignments(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company, centers map[string]uuid.UUID) error {
	numbers, ids := []string{}, []uuid.UUID{}
	for _, d := range co.DefaultAssignments {
		numbers, ids = append(numbers, d.Account), append(ids, centers[d.CostCenter])
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.default_assignment d SET is_active = false, version = d.version + 1, updated_by = $4
		FROM ledger.gl_account g WHERE d.legal_entity_id = $1 AND d.is_active AND g.id = d.account_id AND g.chart_id = $2 AND g.number <> ALL($3)`,
		le, chart, numbers, p.ID); err != nil {
		return fmt.Errorf("ledger: apply company %s: deactivate default assignments: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.default_assignment AS d (tenant_id, legal_entity_id, account_id, cost_center_id, created_by, updated_by)
		SELECT $1, $2, g.id, x.cc, $4, $4 FROM unnest($5::text[], $6::uuid[]) AS x(n, cc) JOIN ledger.gl_account g ON g.chart_id = $3 AND g.number = x.n
		ON CONFLICT (tenant_id, legal_entity_id, account_id) DO UPDATE SET cost_center_id = EXCLUDED.cost_center_id, is_active = true,
			version = d.version + 1, updated_by = $4
		WHERE (d.cost_center_id, d.is_active) IS DISTINCT FROM (EXCLUDED.cost_center_id, true)`,
		p.TenantID, le, chart, p.ID, numbers, ids); err != nil {
		return fmt.Errorf("ledger: apply company %s: default assignments: %w", co.Code, err)
	}
	return nil
}

// keepsDefaultCostCenter refuses a cost center's new periods when a
// company's default assignment names it and, after the change, it could
// not be a default from today on (unusableDefault): automatic postings to
// those accounts would fail (409 ledger.default_center_in_use). The change
// holds the cost center for update, so an activation naming it waits.
// Today is the cost center's company's date in its time zone by the
// clock the setup reads the same rule by (Setup.Now): a change takes
// effect from the company's date (product owner 2026-10-31, T72n4).
func keepsDefaultCostCenter(ctx context.Context, tx pgx.Tx, c CostCenter, spans []span[CostCenterAttrs], days companyDays) error {
	var accounts []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(g.number ORDER BY g.number), '{}') FROM ledger.default_assignment d
		JOIN ledger.gl_account g ON g.tenant_id = d.tenant_id AND g.id = d.account_id WHERE d.cost_center_id = $1 AND d.is_active`, c.ID).Scan(&accounts); err != nil {
		return fmt.Errorf("ledger: default assignments of cost center %s: %w", c.ID, err)
	}
	if len(accounts) == 0 {
		return nil
	}
	today, err := days.visible(ctx, c.LegalEntityID)
	if err != nil {
		return err
	}
	if why := unusableDefault(spans, today, func(a CostCenterAttrs) bool { return a.LockCost }); why != "" {
		return problem.Conflict("ledger.default_center_in_use", "The cost center is the default cost center of accounts "+strings.Join(accounts, ", ")+
			"; after the change it "+why+". Change the configuration's default_assignments first.")
	}
	return nil
}

// keepsDefaultProfitCenter refuses a profit center's new periods when it
// is a company's default profit center and, after the change, could not be
// a default from today on (409 ledger.default_center_in_use). A profit
// center may be the default of companies in different time zones: each
// judges it on its own date (T72n4).
func keepsDefaultProfitCenter(ctx context.Context, tx pgx.Tx, id uuid.UUID, spans []span[ProfitCenterAttrs], days companyDays) error {
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id FROM ledger.company_controlling WHERE default_profit_center_id = $1 ORDER BY legal_entity_id`, id)
	companies, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("ledger: companies defaulting to profit center %s: %w", id, err)
	}
	for _, le := range companies {
		today, err := days.visible(ctx, le)
		if err != nil {
			return err
		}
		if why := unusableDefault(spans, today, func(a ProfitCenterAttrs) bool { return a.Locked }); why != "" {
			// Not how many: the companies may include ones the requester cannot access.
			return problem.Conflict("ledger.default_center_in_use", "The profit center is a company's default profit center; after the change it "+
				why+". Change the configuration's default_profit_center first.")
		}
	}
	return nil
}
