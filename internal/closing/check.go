package closing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The checks this build runs (ADR-0058 §6; config.CloseChecks lists them
// for the template's validation).
const (
	CheckNoParked     = "no_parked_entries"
	CheckRevaluation  = "revaluation_posted"
	CheckDepreciation = "depreciation_posted"
)

// maxNamed is how many parked entries a no_parked_entries result names;
// it counts them all (ADR-0058 §6).
const maxNamed = 20

// openParked are the statuses of a parked entry still waiting for a
// person: parked, or submitted for approval and not yet decided (T35).
var openParked = []string{"parked", "pending_approval"}

// found is what a check found, recorded on its task (check_result) as it
// was when the check ran, names included: a later change of a name or an
// entry does not rewrite what the person saw.
type found struct {
	Summary     string        `json:"summary"`
	ParkedCount int           `json:"parked_entries_count,omitempty"`
	Parked      []foundParked `json:"parked_entries,omitempty"`
	Ledgers     []foundLedger `json:"ledgers,omitempty"`
}

// foundParked is a parked entry a no_parked_entries check named.
type foundParked struct {
	ID            uuid.UUID `json:"id"`
	DocumentType  string    `json:"document_type"`
	PostingDate   string    `json:"posting_date"`
	Status        string    `json:"status"`
	Reference     *string   `json:"reference,omitempty"`
	HeaderText    *string   `json:"header_text,omitempty"`
	CreatedBy     uuid.UUID `json:"created_by"`
	CreatedByKind string    `json:"created_by_kind"`
	CreatedByName string    `json:"created_by_name"`
}

// foundLedger is a covered ledger as a revaluation_posted or a
// depreciation_posted check found it: its run, and for depreciation
// without a posted run how many assets a run would select.
type foundLedger struct {
	LedgerID     uuid.UUID  `json:"ledger_id"`
	LedgerCode   string     `json:"ledger_code"`
	LedgerName   string     `json:"ledger_name"`
	KeyDate      string     `json:"key_date"`
	Run          *uuid.UUID `json:"revaluation_run_id,omitempty"`
	Depreciation *uuid.UUID `json:"depreciation_run_id,omitempty"`
	ToDepreciate *int       `json:"assets_to_depreciate,omitempty"`
}

// toAPI shows a recorded result; what a check found is shown for its own
// check only, an empty list when it found nothing.
func (f *found) toAPI(check string, passed bool, at time.Time, by uuid.UUID, byName string) gen.CloseCheckResult {
	out := gen.CloseCheckResult{Check: check, Passed: passed, CheckedAt: at, CheckedBy: by, CheckedByName: byName, Summary: f.Summary}
	switch check {
	case CheckNoParked:
		out.ParkedEntriesCount.SetTo(int32(f.ParkedCount)) // at most the entries of one period
		out.ParkedEntries = make([]gen.CloseCheckParkedEntry, len(f.Parked))
		for i, p := range f.Parked {
			day, _ := time.Parse(time.DateOnly, p.PostingDate) // written by run from a date
			out.ParkedEntries[i] = gen.CloseCheckParkedEntry{ID: p.ID, DocumentType: p.DocumentType, PostingDate: day, Status: p.Status,
				Reference: apitypes.OptString(p.Reference), HeaderText: apitypes.OptString(p.HeaderText), CreatedBy: p.CreatedBy,
				CreatedByKind: p.CreatedByKind, CreatedByName: p.CreatedByName}
		}
	case CheckRevaluation, CheckDepreciation:
		out.Ledgers = make([]gen.CloseCheckLedger, len(f.Ledgers))
		for i, l := range f.Ledgers {
			out.Ledgers[i] = l.toAPI()
		}
	}
	return out
}

// toAPI shows a covered ledger as its check found it.
func (l foundLedger) toAPI() gen.CloseCheckLedger {
	day, _ := time.Parse(time.DateOnly, l.KeyDate) // written by run from a date
	out := gen.CloseCheckLedger{LedgerID: l.LedgerID, LedgerCode: l.LedgerCode, LedgerName: l.LedgerName, KeyDate: day,
		Posted: l.Run != nil || l.Depreciation != nil, RevaluationRunID: apitypes.OptUUID(l.Run), DepreciationRunID: apitypes.OptUUID(l.Depreciation)}
	if l.ToDepreciate != nil {
		out.AssetsToDepreciate.SetTo(int32(*l.ToDepreciate)) // at most a company's assets
	}
	return out
}

// CheckCloseTask implements POST
// /v1/period-closes/{id}/tasks/{code}:check: runs a check task's check
// now, once every task it depends on is done, records the result on the
// task, and sets it done when it passed and open when it did not.
func (e *Endpoints) CheckCloseTask(ctx context.Context, params gen.CheckCloseTaskParams) (gen.CheckCloseTaskRes, error) {
	return e.taskAction(ctx, params.ID, params.Code, params.IfMatch, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task) (change, bool, error) {
		switch {
		case t.Kind != config.CloseCheck:
			return change{}, false, problem.Conflict("closing.task_not_check", "Task "+t.Code+" is a manual task; a person completes it with :complete.")
		case len(t.BlockedBy) > 0:
			return change{}, false, problem.Conflict("closing.task_blocked", "Task "+t.Code+" waits for "+strings.Join(t.BlockedBy, ", ")+
				", still open; complete them first.")
		}
		passed, f, err := e.run(ctx, tx, t)
		if err != nil {
			return change{}, false, err
		}
		if err := recordCheck(ctx, tx, p, t, passed, f); err != nil {
			return change{}, false, err
		}
		return change{action: "check_task", name: "task_checked", task: &t.Code, passed: &passed,
			detail: map[string]any{"check": *t.Check, "passed": passed, "summary": f.Summary}}, true, nil
	})
}

// recordCheck records a check's result on its task: a pass makes it done,
// keeping the completion of a task done already; a failure makes it open.
// The close's lock serialises its writes, so nothing changes the task
// between its read and this update.
func recordCheck(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task, passed bool, f found) error {
	result, _ := json.Marshal(f) // strings, numbers, ids and lists of them always encode
	_, err := tx.Exec(ctx, `UPDATE closing.close_task SET checked_at = now(), checked_by = $3, check_passed = $4, check_result = $5,
			status = CASE WHEN $4 THEN 'done' ELSE 'open' END,
			completed_by = CASE WHEN $4 THEN coalesce(completed_by, $3) END, completed_at = CASE WHEN $4 THEN coalesce(completed_at, now()) END,
			version = version + 1, updated_by = $3
		WHERE close_id = $1 AND code = $2`, t.CloseID, t.Code, p.ID, passed, result)
	return errs.Wrapf(err, "closing: record check of task %s of close %s", t.Code, t.CloseID)
}

// run runs a task's check on its close's ledgers as the ledger holds them
// now; a check this build does not know is broken data (the template's
// validation admits only config.CloseChecks).
func (e *Endpoints) run(ctx context.Context, tx pgx.Tx, t task) (bool, found, error) {
	covered, err := e.coveredPeriods(ctx, tx, t.CloseID, t.LegalEntityID, t.FiscalYear, t.FiscalPeriod)
	if err != nil {
		return false, found{}, err
	}
	switch *t.Check {
	case CheckNoParked:
		return e.noParked(ctx, t.LegalEntityID, covered[0].Period) // the leading ledger, always covered, first
	case CheckRevaluation:
		return e.revaluationPosted(ctx, t.LegalEntityID, covered)
	case CheckDepreciation:
		return e.depreciationPosted(ctx, t.LegalEntityID, t.FiscalYear, t.FiscalPeriod, covered)
	}
	return false, found{}, fmt.Errorf("closing: task %s of close %s runs check %q, which this build does not", t.Code, t.CloseID, *t.Check)
}

// noParked passes when no parked entry of the company dated in the period
// (the leading ledger's) waits for a person, and names the first of those
// that do with who parked them.
func (e *Endpoints) noParked(ctx context.Context, legalEntity uuid.UUID, period *Period) (bool, found, error) {
	n, ps, err := e.Ledger.Parked(ctx, legalEntity, period.StartDate, period.EndDate, openParked, maxNamed)
	if err != nil {
		return false, found{}, fmt.Errorf("closing: parked entries of %s: %w", legalEntity, err)
	}
	people, err := idref.Resolve(ctx, e.People.Named, idref.Of(ps, func(p ParkedEntry) *uuid.UUID { return &p.CreatedBy }), personRef)
	if err != nil {
		return false, found{}, fmt.Errorf("closing: who parked %d entries: %w", len(ps), err)
	}
	f := found{ParkedCount: n, Parked: make([]foundParked, len(ps))}
	for i, p := range ps {
		f.Parked[i] = foundParked{ID: p.ID, DocumentType: p.DocumentType, PostingDate: p.PostingDate.Format(time.DateOnly), Status: p.Status,
			Reference: p.Reference, HeaderText: p.HeaderText, CreatedBy: p.CreatedBy, CreatedByKind: p.CreatedByKind, CreatedByName: people[p.CreatedBy].Name}
	}
	span := period.StartDate.Format(time.DateOnly) + " to " + period.EndDate.Format(time.DateOnly)
	switch n {
	case 0:
		f.Summary = "No parked entry dated " + span + " is parked or pending approval."
	case 1:
		f.Summary = "1 parked entry dated " + span + " is parked or pending approval; post or cancel it."
	default:
		f.Summary = fmt.Sprintf("%d parked entries dated %s are parked or pending approval; post or cancel them.", n, span)
	}
	return n == 0, f, nil
}

// revaluationPosted passes when each covered ledger has a posted
// revaluation run keyed on its period's last day (ADR-0042), and names
// each ledger with its run.
func (e *Endpoints) revaluationPosted(ctx context.Context, legalEntity uuid.UUID, covered []LedgerPeriod) (bool, found, error) {
	f := found{Ledgers: make([]foundLedger, len(covered))}
	var missing []string
	for i, k := range covered {
		run, ok, err := e.Ledger.PostedRevaluation(ctx, legalEntity, k.LedgerID, k.Period.EndDate)
		if err != nil {
			return false, found{}, fmt.Errorf("closing: revaluation of ledger %s: %w", k.Code, err)
		}
		f.Ledgers[i] = foundLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, KeyDate: k.Period.EndDate.Format(time.DateOnly)}
		if ok {
			f.Ledgers[i].Run = &run
		} else {
			missing = append(missing, k.Code)
		}
	}
	f.Summary = "Every covered ledger has a posted revaluation run at its period's last day."
	if len(missing) > 0 {
		f.Summary = "No posted revaluation run at the period's last day in ledger " + strings.Join(missing, ", ") + "; run the revaluation first."
	}
	return len(missing) == 0, f, nil
}

// depreciationPosted passes when each covered ledger has a posted
// depreciation run of the period, or has no asset a run of it would
// select (ADR-0057 §7), and names each ledger with its run or, without
// one, how many assets a run would select.
func (e *Endpoints) depreciationPosted(ctx context.Context, legalEntity uuid.UUID, year, period int16, covered []LedgerPeriod) (bool, found, error) {
	f := found{Ledgers: make([]foundLedger, len(covered))}
	var missing []string
	for i, k := range covered {
		run, selected, err := e.Depreciation.Posted(ctx, legalEntity, k.LedgerID, year, period, k.Period.EndDate)
		if err != nil {
			return false, found{}, fmt.Errorf("closing: depreciation of ledger %s: %w", k.Code, err)
		}
		f.Ledgers[i] = foundLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, KeyDate: k.Period.EndDate.Format(time.DateOnly),
			Depreciation: run}
		if run == nil {
			f.Ledgers[i].ToDepreciate = &selected
			if selected > 0 {
				missing = append(missing, fmt.Sprintf("%s (%s)", k.Code, assetCount(selected)))
			}
		}
	}
	f.Summary = fmt.Sprintf("Every covered ledger has a posted depreciation run of %d/%d or nothing to depreciate.", year, period)
	if len(missing) > 0 {
		f.Summary = fmt.Sprintf("No posted depreciation run of %d/%d in ledger %s; run the depreciation first.", year, period, strings.Join(missing, ", "))
	}
	return len(missing) == 0, f, nil
}

// assetCount is n assets in words.
func assetCount(n int) string {
	if n == 1 {
		return "1 asset to depreciate"
	}
	return fmt.Sprintf("%d assets to depreciate", n)
}
