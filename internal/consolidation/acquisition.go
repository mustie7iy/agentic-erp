package consolidation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The investment and equity elimination (ADR-0059 §9, As built T68h). For
// each member M first consolidated in period F, its acquisition is
// measured at F-1, the regular period before F: M translated through F-1
// at F-1's closing rate, its net assets the sum of its asset and liability
// rows (N in local currency, A translated), its acquisition equity every
// other row per group account (its equity accounts; retained earnings,
// with every result through F-1, which were earned before the group held
// it, IFRS 10.B88; the translation reserve), summing to -A. The
// investment I is the parent's balance on M's investment account through
// the run's period. The investment entry reverses the acquisition equity
// and the investment.
//
// Goodwill (I - A + the interest) and the non-controlling interest are
// not computed: the accounting specialist (po:2026-11-09, B5) accepts
// book values only for a subsidiary the parent incorporated, where IFRS 3
// does not apply and the investment is eliminated against its equity, and
// the pilot's group is such a subsidiary held whole (D11). A run therefore
// refuses a member held at less than 100% or acquired
// (consolidation.structure_unsupported) and one whose investment differs
// from its net assets at acquisition (consolidation.investment_mismatch);
// fair values, goodwill and the interest are the tracker's non-pilot
// leaves (B5-B7, B9), for which run_acquisition and acquisition keep
// their columns at zero.
//
// The first final run including M freezes its figures in acquisition and
// acquisition_equity; every later run reads them instead of measuring and
// refuses a change of M's origin, ownership, first period or investment
// account (consolidation.acquisition_changed) or of the parent's
// investment balance (consolidation.investment_changed): changes after
// first consolidation are out of scope (§1).

// Components of an investment entry's lines.
const (
	componentEquity     = "acquisition_equity"
	componentInvestment = "investment"
)

// nonPilot names the tracker's leaves a refused structure waits for.
const nonPilot = "goodwill, fair values and non-controlling interests are not built (ADR-0059 As built T68h; the tracker's leaves " +
	"for the accounting specialist's B5-B7 and B9)"

// structureUnsupported refuses a member that needs goodwill or a
// non-controlling interest (422 consolidation.structure_unsupported).
func structureUnsupported(group string, m config.GroupMember) *problem.Problem {
	return problem.Unprocessable("consolidation.structure_unsupported", fmt.Sprintf("Company %s, a member of group %s, is held at %s%% and %s; a run "+
		"consolidates a member the parent incorporated and holds whole, whose investment is eliminated against its equity without goodwill or a "+
		"non-controlling interest: %s.", m.Company, group, m.OwnershipPercent, originText(m.Origin), nonPilot),
		problem.FieldError{Field: "group_code", Code: "structure_unsupported", Message: "company " + m.Company + " incorporated by the parent and held at 100%"})
}

// originText says how the parent came to hold a member.
func originText(origin string) string {
	switch origin {
	case config.OriginIncorporated:
		return "was incorporated by the parent"
	case config.OriginAcquired:
		return "was acquired"
	}
	return "its origin is not configured"
}

// previousPeriod is the regular period before one: the previous fiscal
// year's last when it is the first.
func previousPeriod(year, period int16) (int16, int16) {
	if period == 1 {
		return year - 1, 12
	}
	return year, period - 1
}

// frozenMembers are the members of the group whose acquisition is frozen.
func frozenMembers(ctx context.Context, tx pgx.Tx, group string) (map[uuid.UUID]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id FROM consolidation.acquisition WHERE group_code = $1`, group)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, errs.Wrapf(err, "consolidation: frozen acquisitions of %s", group)
}

// acquirable checks that every member a run includes whose acquisition is
// not frozen can be measured: its leading ledger has the period before
// its first (422 consolidation.period_unknown) and, in another currency
// than the group's, that period has a closing rate (rateOf).
func (e *Endpoints) acquirable(ctx context.Context, tx pgx.Tx, t target) error {
	frozen, err := frozenMembers(ctx, tx, t.group.Code)
	if err != nil {
		return err
	}
	for _, c := range t.included {
		if c.member == nil || frozen[c.co.ID] {
			continue
		}
		if _, _, err := e.measurePeriod(ctx, t.rateType, t.currency, c.co, c.firstYear, c.firstP); err != nil {
			return err
		}
	}
	return nil
}

// measurePeriod is the period a member's acquisition is measured at, the
// one before its first, with its closing rate; a member in the group
// currency needs no period of its own (rate 1).
func (e *Endpoints) measurePeriod(ctx context.Context, rateType, group string, co Company, firstYear, firstP int16) (Period, Rate, error) {
	year, per := previousPeriod(firstYear, firstP)
	if co.LocalCurrency == group {
		return Period{}, Rate{Rate: one(), Shown: "1"}, nil
	}
	p, ok, err := e.Ledger.PeriodOf(ctx, co.ID, year, per)
	switch {
	case err != nil:
		return Period{}, Rate{}, fmt.Errorf("consolidation: period %d/%d of %s: %w", year, per, co.Code, err)
	case !ok:
		return Period{}, Rate{}, problem.Unprocessable("consolidation.period_unknown", fmt.Sprintf("Company %s has no period %d/%d in its leading "+
			"ledger, the period before its first, where its acquisition is measured (ADR-0059 §9).", co.Code, year, per),
			problem.FieldError{Field: "period", Code: "period_unknown", Message: "the period before company " + co.Code + "'s first"})
	}
	r, err := e.rateOf(ctx, rateType, group, co, p, year, per)
	return p, r, err
}

// freeze freezes, as a run is finalized, the acquisitions it measured of
// members no final run froze yet (ADR-0059 §9, §10), with their equity
// from its investment entries, each audited; and refuses the run when a
// member's acquisition it measured differs from one another final run
// froze since it was computed (409 consolidation.acquisition_changed).
func (e *Endpoints) freeze(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	if err := e.measuredLocked(ctx, tx, r); err != nil {
		return err
	}
	rows, _ := tx.Query(ctx, `INSERT INTO consolidation.acquisition (tenant_id, group_code, legal_entity_id, origin, ownership_percent, first_fiscal_year,
			first_fiscal_period, investment_account_id, local_currency, net_assets_lc, closing_rate, rate_valid_from, net_assets_gc, investment_gc,
			goodwill_gc, nci_gc, run_id, created_by)
		SELECT a.tenant_id, $2, a.legal_entity_id, a.origin, m.ownership_percent, m.first_fiscal_year, m.first_fiscal_period, a.investment_account_id,
			m.local_currency, a.net_assets_lc, a.closing_rate, a.rate_valid_from, a.net_assets_gc, a.investment_gc, a.goodwill_gc, a.nci_gc, a.run_id, $3
		FROM consolidation.run_acquisition a
		JOIN consolidation.run_member m ON m.tenant_id = a.tenant_id AND m.run_id = a.run_id AND m.legal_entity_id = a.legal_entity_id
		WHERE a.run_id = $1 AND NOT a.frozen ORDER BY m.company_code
		ON CONFLICT (tenant_id, group_code, legal_entity_id) DO NOTHING
		RETURNING legal_entity_id, net_assets_gc, investment_gc`, r.ID, r.GroupCode, p.ID)
	type frozen struct {
		company          uuid.UUID
		assets, invested int64
	}
	inserted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (frozen, error) {
		var f frozen
		return f, row.Scan(&f.company, &f.assets, &f.invested)
	})
	if err != nil {
		return fmt.Errorf("consolidation: freeze the acquisitions of run %s: %w", r.ID, err)
	}
	ids := make([]uuid.UUID, len(inserted))
	for i, f := range inserted {
		ids[i] = f.company
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consolidation.acquisition_equity (tenant_id, group_code, legal_entity_id, group_account, amount_gc)
		SELECT l.tenant_id, $2, en.legal_entity_id, l.group_account, -l.amount_gc FROM consolidation.run_entry en
		JOIN consolidation.run_entry_line l ON l.tenant_id = en.tenant_id AND l.run_id = en.run_id AND l.entry_no = en.entry_no
		WHERE en.run_id = $1 AND en.kind = 'investment' AND l.component = 'acquisition_equity' AND en.legal_entity_id = ANY($3)`,
		r.ID, r.GroupCode, ids); err != nil {
		return fmt.Errorf("consolidation: freeze the acquisition equity of run %s: %w", r.ID, err)
	}
	if err := frozenElsewhere(ctx, tx, r); err != nil {
		return err
	}
	// Audit entries join the request's buffer, which a unit of work
	// always has.
	recorded := make([]error, len(inserted))
	for i, f := range inserted {
		recorded[i] = audit.Record(ctx, "consolidation.acquisition.frozen", "consolidation.acquisition", f.company, map[string]any{"group_code": r.GroupCode,
			"legal_entity_id": f.company, "run_id": r.ID, "net_assets_gc": f.assets, "investment_gc": f.invested, "group_currency": r.GroupCurrency})
	}
	return errors.Join(recorded...)
}

// measuredLocked refuses to freeze an acquisition measured at a period
// that is not locked now (409 consolidation.period_not_locked, as §10
// refuses a run's own period): a frozen acquisition is never changed, so
// it is taken from locked figures only. A member in the group currency
// without that period in its ledger has nothing there to change.
func (e *Endpoints) measuredLocked(ctx context.Context, tx pgx.Tx, r Run) error {
	rows, _ := tx.Query(ctx, memberSelect+` m WHERE m.run_id = $1 AND EXISTS (SELECT FROM consolidation.run_acquisition a
		WHERE a.tenant_id = m.tenant_id AND a.run_id = m.run_id AND a.legal_entity_id = m.legal_entity_id AND NOT a.frozen) ORDER BY m.company_code`, r.ID)
	ms, err := pgx.CollectRows(rows, scanMember)
	if err != nil {
		return fmt.Errorf("consolidation: members measured by run %s: %w", r.ID, err)
	}
	var not []string
	for _, m := range ms {
		year, per := previousPeriod(*m.FirstYear, *m.FirstPeriod)
		p, ok, err := e.Ledger.PeriodOf(ctx, m.LegalEntityID, year, per)
		if err != nil {
			return fmt.Errorf("consolidation: period %d/%d of %s: %w", year, per, m.CompanyCode, err)
		}
		if (ok || m.LocalCurrency != r.GroupCurrency) && p.Status != "locked" {
			not = append(not, fmt.Sprintf("company %s's %d/%d (%s)", m.CompanyCode, year, per, orNone(p.Status)))
		}
	}
	if len(not) == 0 {
		return nil
	}
	return problem.Conflict("consolidation.period_not_locked", fmt.Sprintf("Run %s would freeze the acquisitions it measured at periods that are not "+
		"locked: %s. A frozen acquisition is never changed (ADR-0059 §9): lock each period before a member's first and finalize again.", r.ID,
		strings.Join(not, ", ")))
}

// frozenElsewhere refuses a run one of whose measured acquisitions differs
// from the one another final run froze: in its figures or its equity.
func frozenElsewhere(ctx context.Context, tx pgx.Tx, r Run) error {
	rows, _ := tx.Query(ctx, `SELECT m.company_code, q.run_id FROM consolidation.run_acquisition a
		JOIN consolidation.run_member m ON m.tenant_id = a.tenant_id AND m.run_id = a.run_id AND m.legal_entity_id = a.legal_entity_id
		JOIN consolidation.acquisition q ON q.tenant_id = a.tenant_id AND q.group_code = $2 AND q.legal_entity_id = a.legal_entity_id
		WHERE a.run_id = $1 AND NOT a.frozen AND q.run_id <> a.run_id
			AND ((a.origin, m.ownership_percent, m.first_fiscal_year, m.first_fiscal_period, a.investment_account_id, a.net_assets_lc, a.closing_rate,
				a.rate_valid_from, a.net_assets_gc, a.investment_gc, a.goodwill_gc, a.nci_gc) IS DISTINCT FROM (q.origin, q.ownership_percent,
				q.first_fiscal_year, q.first_fiscal_period, q.investment_account_id, q.net_assets_lc, q.closing_rate, q.rate_valid_from, q.net_assets_gc,
				q.investment_gc, q.goodwill_gc, q.nci_gc)
			OR EXISTS ((SELECT l.group_account, -l.amount_gc FROM consolidation.run_entry en
					JOIN consolidation.run_entry_line l ON l.tenant_id = en.tenant_id AND l.run_id = en.run_id AND l.entry_no = en.entry_no
					WHERE en.run_id = a.run_id AND en.kind = 'investment' AND en.legal_entity_id = a.legal_entity_id AND l.component = 'acquisition_equity'
				EXCEPT SELECT e.group_account, e.amount_gc FROM consolidation.acquisition_equity e
					WHERE e.tenant_id = q.tenant_id AND e.group_code = q.group_code AND e.legal_entity_id = q.legal_entity_id)
				UNION ALL (SELECT e.group_account, e.amount_gc FROM consolidation.acquisition_equity e
					WHERE e.tenant_id = q.tenant_id AND e.group_code = q.group_code AND e.legal_entity_id = q.legal_entity_id
				EXCEPT SELECT l.group_account, -l.amount_gc FROM consolidation.run_entry en
					JOIN consolidation.run_entry_line l ON l.tenant_id = en.tenant_id AND l.run_id = en.run_id AND l.entry_no = en.entry_no
					WHERE en.run_id = a.run_id AND en.kind = 'investment' AND en.legal_entity_id = a.legal_entity_id AND l.component = 'acquisition_equity')))
		ORDER BY m.company_code`, r.ID, r.GroupCode)
	var changed []string
	var code string
	var by uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&code, &by}, func() error {
		changed = append(changed, fmt.Sprintf("%s (frozen by run %s)", code, by))
		return nil
	}); err != nil {
		return fmt.Errorf("consolidation: acquisitions of run %s against the frozen ones: %w", r.ID, err)
	}
	if len(changed) > 0 {
		return problem.Conflict("consolidation.acquisition_changed", fmt.Sprintf("The acquisition run %s measured of %s differs from the one a final run "+
			"froze since it was computed (ADR-0059 §9); run the group again, which reads the frozen acquisition.", r.ID, strings.Join(changed, ", ")))
	}
	return nil
}

// memberKey is a company of a run.
type memberKey struct{ run, company uuid.UUID }

// acquisitionsShown are the runs' members' acquisitions as the API shows
// them, by run and member.
func acquisitionsShown(ctx context.Context, tx pgx.Tx, rs []Run, ms map[uuid.UUID][]Member) (map[memberKey]gen.ConsolidationAcquisition, error) {
	ids := make([]uuid.UUID, len(rs))
	group := map[uuid.UUID]string{}
	for i, r := range rs {
		ids[i], group[r.ID] = r.ID, r.GroupCurrency
	}
	local := map[memberKey]Member{}
	for run, members := range ms {
		for _, m := range members {
			local[memberKey{run, m.LegalEntityID}] = m
		}
	}
	all, err := storedAcquisitions(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	exps := map[string]int{}
	shown := func(m money.Minor, code string) (gen.Money, error) {
		exp, ok := exps[code]
		if !ok {
			if exp, err = currency.Exponent(ctx, tx, code, nil); err != nil {
				return gen.Money{}, fmt.Errorf("consolidation: currency %s: %w", code, err)
			}
			exps[code] = exp
		}
		return gen.Money{Amount: m.Plain(exp), Currency: code}, nil
	}
	out := map[memberKey]gen.ConsolidationAcquisition{}
	for _, s := range all {
		m := local[s.k]
		year, per := previousPeriod(*m.FirstYear, *m.FirstPeriod)
		a := gen.ConsolidationAcquisition{FiscalYear: int32(year), Period: int32(per), ClosingRate: plainRate(s.rate), Frozen: s.frozen}
		if s.from != nil {
			a.RateValidFrom = gen.NewOptDate(*s.from)
		}
		if s.by != nil {
			a.FrozenByRunID = gen.NewOptUUID(*s.by)
		}
		for _, f := range []struct {
			to   *gen.Money
			m    money.Minor
			code string
		}{{&a.NetAssets, s.lc, m.LocalCurrency}, {&a.NetAssetsGroup, s.gc, group[s.k.run]}, {&a.Investment, s.invested, group[s.k.run]},
			{&a.Goodwill, s.goodwill, group[s.k.run]}, {&a.NonControllingInterest, s.nci, group[s.k.run]}} {
			if *f.to, err = shown(f.m, f.code); err != nil {
				return nil, err
			}
		}
		out[s.k] = a
	}
	return out, nil
}

// storedAcquisition is a run's member's acquisition as stored, with the
// run that froze it.
type storedAcquisition struct {
	k                               memberKey
	lc, gc, invested, goodwill, nci money.Minor
	rate                            string
	from                            *time.Time
	frozen                          bool
	by                              *uuid.UUID
}

// storedAcquisitions reads the runs' members' acquisitions. The run that
// froze one is the run it was read from, or the run itself once its
// finalization froze what it measured.
func storedAcquisitions(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]storedAcquisition, error) {
	rows, _ := tx.Query(ctx, `SELECT a.run_id, a.legal_entity_id, a.net_assets_lc, a.closing_rate::text, a.rate_valid_from, a.net_assets_gc, a.investment_gc,
			a.goodwill_gc, a.nci_gc, a.frozen, CASE WHEN a.frozen OR q.run_id = a.run_id THEN q.run_id END
		FROM consolidation.run_acquisition a JOIN consolidation.run r ON r.tenant_id = a.tenant_id AND r.id = a.run_id
		LEFT JOIN consolidation.acquisition q ON q.tenant_id = a.tenant_id AND q.group_code = r.group_code AND q.legal_entity_id = a.legal_entity_id
		WHERE a.run_id = ANY($1)`, ids)
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedAcquisition, error) {
		var s storedAcquisition
		return s, row.Scan(&s.k.run, &s.k.company, &s.lc, &s.rate, &s.from, &s.gc, &s.invested, &s.goodwill, &s.nci, &s.frozen, &s.by)
	})
	return all, errs.Wrapf(err, "consolidation: acquisitions of %d runs", len(ids))
}
