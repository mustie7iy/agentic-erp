package consolidation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Accepting differences and finalizing runs (ADR-0059 §8, §10). Both are
// people's decisions (x-agent: false, which authz enforces for every
// operation the contract marks so). Every write on a run reads it FOR
// UPDATE first (§5), so an acceptance, a finalization, a discard and the
// workflow's steps serialise on it; finalizing locks every run of its
// group's period that is not discarded, in id order, before it reads which
// one is final, so two finalizations of a period serialise and the second
// supersedes the first's.

// personRef names a principal missing from the identity context.
var personRef = idref.Kind{Name: "principal", Context: "identity"}

// periodKey is a company's period of a fiscal year.
type periodKey struct {
	company      uuid.UUID
	year, period int16
}

// periodsNow reads, as they are now, the leading-ledger periods of the
// companies of the runs that are all translated (their stale is known),
// one ledger read per fiscal year and period. A period the ledger no
// longer has is the zero Period, whose version no run recorded.
func (e *Endpoints) periodsNow(ctx context.Context, rs []Run, ms map[uuid.UUID][]Member) (map[periodKey]Period, error) {
	type yearPeriod struct{ year, period int16 }
	companies := map[yearPeriod][]uuid.UUID{}
	var order []yearPeriod
	for _, r := range rs {
		if !translated(ms[r.ID]) {
			continue
		}
		yp := yearPeriod{r.FiscalYear, r.Period}
		if _, ok := companies[yp]; !ok {
			order = append(order, yp)
		}
		for _, m := range ms[r.ID] {
			if !slices.Contains(companies[yp], m.LegalEntityID) {
				companies[yp] = append(companies[yp], m.LegalEntityID)
			}
		}
	}
	out := map[periodKey]Period{}
	for _, yp := range order {
		ps, err := e.Ledger.PeriodsOf(ctx, companies[yp], yp.year, yp.period)
		if err != nil {
			return nil, fmt.Errorf("consolidation: periods %d/%d of %d companies: %w", yp.year, yp.period, len(companies[yp]), err)
		}
		for company, p := range ps {
			out[periodKey{company: company, year: yp.year, period: yp.period}] = p
		}
	}
	return out, nil
}

// translated reports whether a run has companies and every one is
// translated, with its period's status and version recorded.
func translated(ms []Member) bool {
	return len(ms) > 0 && !slices.ContainsFunc(ms, func(m Member) bool { return m.PeriodVersion == nil })
}

// moved are a translated run's companies whose period's version now
// differs from the one the run recorded with their figures (ADR-0059 §10).
func moved(r Run, ms []Member, now map[periodKey]Period) []Member {
	var out []Member
	for _, m := range ms {
		if m.PeriodVersion == nil || now[periodKey{company: m.LegalEntityID, year: r.FiscalYear, period: r.Period}].Version != *m.PeriodVersion {
			out = append(out, m)
		}
	}
	return out
}

// staleOf reads whether a run is stale; ok is false while a company of it
// is not translated.
func (e *Endpoints) staleOf(ctx context.Context, tx pgx.Tx, r Run) (stale, ok bool, err error) {
	ms, err := members(ctx, tx, []uuid.UUID{r.ID})
	if err != nil {
		return false, false, err
	}
	now, err := e.periodsNow(ctx, []Run{r}, ms)
	if err != nil || !translated(ms[r.ID]) {
		return false, false, err
	}
	return len(moved(r, ms[r.ID], now)) > 0, true, nil
}

// FinalizeConsolidationRun implements POST
// /v1/consolidation-runs/{id}:finalize (ADR-0059 §10): a computed run
// without open differences, whose companies' periods were locked when it
// read their figures and are locked now at the versions it recorded,
// becomes the group's final run of its period, superseding the previous
// final one, if any; each run that changes is audited and published.
func (e *Endpoints) FinalizeConsolidationRun(ctx context.Context, params gen.FinalizeConsolidationRunParams) (gen.FinalizeConsolidationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	if err := lockPeriodRuns(ctx, tx, r); err != nil {
		return nil, err
	}
	// As it is once its period's runs are locked, its own among them.
	if r, err = readRun(ctx, tx, params.ID, false); err != nil {
		return nil, err
	}
	if err := etag.Check(params.IfMatch, r.Version); err != nil {
		return nil, errs.Wrapf(err, "consolidation: finalize run %s", r.ID)
	}
	if err := e.finalizable(ctx, tx, r); err != nil {
		return nil, err
	}
	if err := supersede(ctx, tx, p, r); err != nil {
		return nil, err
	}
	if r, err = setStatus(ctx, tx, p, r, RunFinal, ", finalized_by = $5, finalized_at = now()", p.ID); err != nil {
		return nil, err
	}
	if err := freeze(ctx, tx, p, r); err != nil {
		return nil, err
	}
	out, err := e.shown(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	// ogen names the run-with-ETag response, shared with :discard, after
	// the first operation that returns it.
	return &gen.DiscardConsolidationRunOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// lockPeriodRuns locks the runs of a run's group and period that are not
// discarded, in the order of their ids, so finalizations of the period
// serialise without a deadlock.
func lockPeriodRuns(ctx context.Context, tx pgx.Tx, r Run) error {
	rows, _ := tx.Query(ctx, `SELECT id FROM consolidation.run WHERE group_code = $1 AND fiscal_year = $2 AND fiscal_period = $3 AND status <> 'discarded'
		ORDER BY id FOR UPDATE`, r.GroupCode, r.FiscalYear, r.Period)
	_, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return errs.Wrapf(err, "consolidation: lock the runs of %s %d/%d", r.GroupCode, r.FiscalYear, r.Period)
}

// finalizable refuses a run that is not computed (409
// consolidation.run_not_computed), has an open difference (409
// consolidation.differences_open), a company whose period was not locked
// when the run read it or is not locked now (409
// consolidation.period_not_locked), or is stale (409
// consolidation.run_stale).
func (e *Endpoints) finalizable(ctx context.Context, tx pgx.Tx, r Run) error {
	if r.Status != RunComputed {
		return problem.Conflict("consolidation.run_not_computed", fmt.Sprintf("Run %s is %s; only a computed run is finalized.", r.ID, r.Status))
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM consolidation.run_difference WHERE run_id = $1 AND status = 'open'`, r.ID).Scan(&open); err != nil {
		return fmt.Errorf("consolidation: open differences of run %s: %w", r.ID, err)
	}
	if open > 0 {
		return problem.Conflict("consolidation.differences_open", fmt.Sprintf("Run %s has %d open intercompany difference(s); accept each "+
			"(POST %s/differences/{difference_id}:accept) or have the companies correct their postings and run the group again.", r.ID, open, runPath(r.ID)))
	}
	ms, err := members(ctx, tx, []uuid.UUID{r.ID})
	if err != nil {
		return err
	}
	now, err := e.periodsNow(ctx, []Run{r}, ms)
	if err != nil {
		return err
	}
	if err := locked(r, ms[r.ID], now); err != nil {
		return err
	}
	return fresh(r, ms[r.ID], now)
}

// locked refuses a run a company's period was not locked in when the run
// read its figures, or is not locked in now.
func locked(r Run, ms []Member, now map[periodKey]Period) error {
	var not []string
	for _, m := range ms {
		// Computed means every company translated (CompleteConsolidationRun),
		// which the schema does not hold: one never read counts as none.
		then, current := orNone(deref(m.PeriodStatus)), now[periodKey{company: m.LegalEntityID, year: r.FiscalYear, period: r.Period}].Status
		if then != "locked" || current != "locked" {
			not = append(not, fmt.Sprintf("company %s (%s when the run read its figures, %s now)", m.CompanyCode, then, orNone(current)))
		}
	}
	if len(not) == 0 {
		return nil
	}
	return problem.Conflict("consolidation.period_not_locked", fmt.Sprintf("Run %s read figures of leading-ledger periods %d/%d that are not locked, "+
		"then or now: %s. A run is finalized from locked periods only: lock each (the close's sign-off, or lockFiscalPeriod) and run the group again.",
		r.ID, r.FiscalYear, r.Period, strings.Join(not, ", ")))
}

// fresh refuses a stale run, naming its companies whose period changed
// and any pending unlock request.
func fresh(r Run, ms []Member, now map[periodKey]Period) error {
	mv := moved(r, ms, now)
	if len(mv) == 0 {
		return nil
	}
	var named []string
	for _, m := range mv {
		p := now[periodKey{company: m.LegalEntityID, year: r.FiscalYear, period: r.Period}]
		s := fmt.Sprintf("company %s (version %d when the run read its figures, %d now", m.CompanyCode, *m.PeriodVersion, p.Version)
		if p.UnlockRequestID != nil {
			s += ", unlock request " + p.UnlockRequestID.String() + " pending"
		}
		named = append(named, s+")")
	}
	return problem.Conflict("consolidation.run_stale", fmt.Sprintf("The leading-ledger periods %d/%d changed since run %s read their figures: %s. "+
		"Run the group again and finalize the new run.", r.FiscalYear, r.Period, r.ID, strings.Join(named, ", ")))
}

// deref is a recorded status, "" when none was recorded.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orNone is a period's status now, "none" when the ledger has no such
// period any more.
func orNone(status string) string {
	if status == "" {
		return "none"
	}
	return status
}

// supersede sets the group's final run of the period, if any, superseded
// by r, before r becomes final, so the one-final index holds at every
// statement (ADR-0059 §5).
func supersede(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	var prior uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM consolidation.run WHERE group_code = $1 AND fiscal_year = $2 AND fiscal_period = $3 AND status = 'final'`,
		r.GroupCode, r.FiscalYear, r.Period).Scan(&prior)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("consolidation: final run of %s %d/%d: %w", r.GroupCode, r.FiscalYear, r.Period, err)
	}
	old, err := lockRun(ctx, tx, prior)
	if err != nil {
		return err
	}
	_, err = setStatus(ctx, tx, p, old, RunSuperseded, ", superseded_by = $5", r.ID)
	return err
}

// AcceptIntercompanyDifference implements POST
// /v1/consolidation-runs/{id}/differences/{difference_id}:accept
// (ADR-0059 §8): an open difference of a computed run accepted with who,
// when and why; the run's version moves with it (§5). Audited and
// published as IntercompanyDifferenceAccepted.
func (e *Endpoints) AcceptIntercompanyDifference(ctx context.Context, req *gen.ReasonInput,
	params gen.AcceptIntercompanyDifferenceParams) (gen.AcceptIntercompanyDifferenceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	d, err := readDifference(ctx, tx, r, params.DifferenceID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.Check(params.IfMatch, d.version); err != nil {
		return nil, errs.Wrapf(err, "consolidation: accept difference %s", d.id)
	}
	if err := problem.RequireText("reason", req.Reason); err != nil {
		return nil, err
	}
	if r.Status != RunComputed {
		return nil, problem.Conflict("consolidation.run_not_computed", fmt.Sprintf("Run %s is %s; the differences of a computed run are accepted.",
			r.ID, r.Status))
	}
	if d.status != DiffOpen {
		return nil, problem.Conflict("consolidation.difference_not_open", fmt.Sprintf("Difference %s is %s; only an open difference is accepted.",
			d.id, d.status))
	}
	if _, err := tx.Exec(ctx, `UPDATE consolidation.run_difference SET status = 'accepted', accepted_by = $2, accepted_at = now(), reason = $3,
		version = version + 1 WHERE id = $1 AND status = 'open'`, d.id, p.ID, req.Reason); err != nil {
		return nil, fmt.Errorf("consolidation: accept difference %s: %w", d.id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE consolidation.run SET version = version + 1, updated_by = $2 WHERE id = $1 AND status = 'computed'`,
		r.ID, p.ID); err != nil {
		return nil, fmt.Errorf("consolidation: run %s after accepting difference %s: %w", r.ID, d.id, err)
	}
	return e.accepted(ctx, tx, r, d.id)
}

// accepted reads an accepted difference back, audits and publishes it,
// and shows it.
func (e *Endpoints) accepted(ctx context.Context, tx pgx.Tx, r Run, id uuid.UUID) (gen.AcceptIntercompanyDifferenceRes, error) {
	d, err := readDifference(ctx, tx, r, id, false)
	if err != nil {
		return nil, err
	}
	n, err := e.namesOf(ctx, tx, r, nil)
	if err != nil {
		return nil, err
	}
	if n.people, err = e.acceptors(ctx, []storedDifference{d}); err != nil {
		return nil, err
	}
	ms, err := members(ctx, tx, []uuid.UUID{r.ID})
	if err != nil {
		return nil, err
	}
	partnerCode := "" // a pair's partner is always a company of the run
	for _, m := range ms[r.ID] {
		if m.LegalEntityID == *d.partner {
			partnerCode = m.CompanyCode
		}
	}
	out := n.differenceAPI(d)
	payload := map[string]any{"company_code": d.code, "partner_code": partnerCode, "difference_id": d.id, "run_id": r.ID, "group_code": r.GroupCode, "fiscal_year": r.FiscalYear, "fiscal_period": r.Period,
		"elimination_code": d.elimination, "legal_entity_id": d.company, "partner_id": d.partner, "difference": out.Difference.Value,
		"tolerance": out.Tolerance.Value, "reason": *d.reason, "status": d.status, "version": d.version, "run_version": r.Version + 1}
	if err := audit.Record(ctx, "consolidation.difference.accepted", "consolidation.run_difference", d.id, payload,
		outbox.Event{Context: "consolidation", Name: "intercompany_difference_accepted", AggregateType: "consolidation.run_difference", AggregateID: d.id,
			Payload: payload}); err != nil {
		return nil, err
	}
	return &gen.IntercompanyDifferenceHeaders{ETag: gen.NewOptString(etag.Format(d.version)), Response: out}, nil
}

// readDifference reads a difference of a run, FOR UPDATE with lock; one
// of another run, or none, is not found.
func readDifference(ctx context.Context, tx pgx.Tx, r Run, id uuid.UUID, lock bool) (storedDifference, error) {
	q := `SELECT ` + differenceColumns + ` ` + differenceWhere + ` AND d.id = $5`
	if lock {
		q += ` FOR UPDATE OF d`
	}
	rows, _ := tx.Query(ctx, q, r.ID, "", "", nil, id)
	d, err := pgx.CollectExactlyOneRow(rows, scanDifference)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return storedDifference{}, problem.NotFoundOf("intercompany difference")
	case err != nil:
		return storedDifference{}, fmt.Errorf("consolidation: difference %s of run %s: %w", id, r.ID, err)
	}
	return d, nil
}
