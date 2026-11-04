package closing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Posting period statuses as the ledger names them (T32).
const (
	periodOpen   = "open"
	periodClosed = "closed"
	periodLocked = "locked"
)

// lockedLedger is a covered ledger as a sign-off recorded it: its period,
// and whether this sign-off locked it (false: it was locked already).
type lockedLedger struct {
	LedgerID   uuid.UUID `json:"ledger_id"`
	LedgerCode string    `json:"ledger_code"`
	LedgerName string    `json:"ledger_name"`
	Leading    bool      `json:"is_leading"`
	PeriodID   uuid.UUID `json:"fiscal_period_id"`
	Locked     bool      `json:"locked"`
}

// signOff is a stored sign-off of a close.
type signOff struct {
	ID, CloseID uuid.UUID
	SignedBy    uuid.UUID
	SignedAt    time.Time
	Note        *string
	Ledgers     []lockedLedger
}

// SignOffPeriodClose implements POST /v1/period-closes/{id}:signOff
// (ADR-0058 §7): under the close's lock, every manual task done, every
// check passing when run again (its result then recorded), each covered
// period closed or locked; each closed one is then locked through the
// ledger's period lock in the order postings take them, and the sign-off
// is recorded, audited and published. A refusal keeps nothing: the request
// rolls back.
func (e *Endpoints) SignOffPeriodClose(ctx context.Context, req gen.OptPeriodCloseSignOffInput, params gen.SignOffPeriodCloseParams) (gen.SignOffPeriodCloseRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, covered, err := e.lockedForSignOff(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	tasks, err := tasksOf(ctx, tx, c.ID)
	if err != nil {
		return nil, err
	}
	if err := manualDone(tasks); err != nil {
		return nil, err
	}
	if err := e.checksPass(ctx, tx, p, tasks); err != nil {
		return nil, err
	}
	if err := periodsClosed(c, covered); err != nil {
		return nil, err
	}
	locked, err := e.lockPeriods(ctx, covered)
	if err != nil {
		return nil, err
	}
	s := signOff{ID: uuid.Must(uuid.NewV7()), CloseID: c.ID, SignedBy: p.ID, Note: apitypes.Ptr[string](req.Value.Note), Ledgers: locked}
	if c, s, err = signedOff(ctx, tx, p, s); err != nil {
		return nil, err
	}
	if err := recordSignOff(ctx, c, s); err != nil {
		return nil, err
	}
	v, err := e.view(ctx, []uuid.UUID{c.LegalEntityID}, nil, s.SignedBy)
	if err != nil {
		return nil, err
	}
	return &gen.PeriodCloseSignOffHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: v.signOffToAPI(c, s)}, nil
}

// lockedForSignOff locks the close, checks If-Match against its version,
// and reads it with its covered ledgers' periods as they are now; a close
// signed off whose covered periods are all locked is refused: nothing is
// left to sign off.
func (e *Endpoints) lockedForSignOff(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch string) (periodClose, []LedgerPeriod, error) {
	version, err := lockClose(ctx, tx, id)
	if err != nil {
		return periodClose{}, nil, err
	}
	if err := etag.IfMatch(ifMatch)(version); err != nil {
		return periodClose{}, nil, err
	}
	c, err := readClose(ctx, tx, id)
	if err != nil {
		return periodClose{}, nil, err
	}
	covered, err := e.coveredPeriods(ctx, tx, c.ID, c.LegalEntityID, c.FiscalYear, c.FiscalPeriod)
	if err != nil {
		return periodClose{}, nil, err
	}
	if c.Status == CloseSignedOff && !slices.ContainsFunc(covered, func(k LedgerPeriod) bool { return k.Period.Status != periodLocked }) {
		return periodClose{}, nil, signedOffConflict(c)
	}
	return c, covered, nil
}

// signedOffConflict refuses a write on a close signed off whose covered
// periods are all locked (ADR-0058 §7).
func signedOffConflict(c periodClose) error {
	return problem.Conflict("closing.close_signed_off", fmt.Sprintf("Close %s of %d/%d is signed off and its periods are locked; "+
		"an approved unlock of a period (unlockFiscalPeriod) lets it be worked on again.", c.ID, c.FiscalYear, c.FiscalPeriod))
}

// manualDone refuses a sign-off while a manual task is open, naming them;
// a check task's status is its latest check's, which the sign-off runs
// again.
func manualDone(tasks []task) error {
	var open []string
	for _, t := range tasks {
		if t.Kind != config.CloseCheck && t.Status != TaskDone {
			open = append(open, t.Code)
		}
	}
	if len(open) == 0 {
		return nil
	}
	return problem.Conflict("closing.tasks_open", "Manual task "+strings.Join(open, ", ")+" is open; complete every manual task before signing off.")
}

// checksPass runs every check task's check again: when any fails the
// sign-off is refused, naming each failing task with what it found, and
// nothing is recorded; when all pass, each result is recorded on its task,
// as :check records it.
func (e *Endpoints) checksPass(ctx context.Context, tx pgx.Tx, p reqctx.Principal, tasks []task) error {
	type result struct {
		t task
		f found
	}
	var results []result
	var failing []problem.FailingCheck
	for _, t := range tasks {
		if t.Kind != config.CloseCheck {
			continue
		}
		passed, f, err := e.run(ctx, tx, t)
		if err != nil {
			return err
		}
		if !passed {
			failing = append(failing, problem.FailingCheck{TaskCode: t.Code, TaskName: t.Name, Check: *t.Check, Summary: f.Summary})
		}
		results = append(results, result{t, f})
	}
	if len(failing) > 0 {
		var lines []string
		for _, f := range failing {
			lines = append(lines, f.TaskCode+": "+f.Summary)
		}
		pr := problem.Conflict("closing.checks_failing", "A check fails when run again, so the close is not signed off: "+strings.Join(lines, " "))
		pr.FailingChecks = failing
		return pr
	}
	for _, r := range results {
		if err := recordCheck(ctx, tx, p, r.t, true, r.f); err != nil {
			return err
		}
	}
	return nil
}

// periodsClosed refuses a sign-off while a covered period is open, naming
// its ledgers: a close is signed off after its period stopped taking
// postings, and a sign-off never closes a period itself.
func periodsClosed(c periodClose, covered []LedgerPeriod) error {
	var open []string
	for _, k := range covered {
		if k.Period.Status == periodOpen {
			open = append(open, k.Code)
		}
	}
	if len(open) == 0 {
		return nil
	}
	return problem.Conflict("closing.period_not_closed", fmt.Sprintf("Period %d/%d is open in ledger %s; close it (closeFiscalPeriod) before signing off, "+
		"so that no posting lands in it after the sign-off.", c.FiscalYear, c.FiscalPeriod, strings.Join(open, ", ")))
}

// lockPeriods locks each closed covered period through the ledger's
// period lock, at the version read, in the order covered holds them: the
// leading ledger first, then by code, the order postings lock periods in
// (ADR-0058 §5), so a sign-off and a posting never wait on each other's
// periods in opposite orders. A period locked already is left as it is.
func (e *Endpoints) lockPeriods(ctx context.Context, covered []LedgerPeriod) ([]lockedLedger, error) {
	out := make([]lockedLedger, len(covered))
	for i, k := range covered {
		out[i] = lockedLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, Leading: k.Leading, PeriodID: k.Period.ID}
		if k.Period.Status != periodClosed {
			continue // locked: periodsClosed refused an open one
		}
		if err := e.Ledger.Lock(ctx, k.Period.ID, k.Period.Version); err != nil {
			return nil, fmt.Errorf("closing: lock period %s of ledger %s: %w", k.Period.ID, k.Code, err)
		}
		out[i].Locked = true
	}
	return out, nil
}

// signedOff writes a sign-off and sets its close signed off, moving its
// version; it returns the close and the sign-off as stored.
func signedOff(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s signOff) (periodClose, signOff, error) {
	locked, _ := json.Marshal(s.Ledgers) // ids, strings and booleans always encode
	if err := tx.QueryRow(ctx, `INSERT INTO closing.sign_off (tenant_id, id, close_id, signed_by, note, locked) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING signed_at`, p.TenantID, s.ID, s.CloseID, s.SignedBy, s.Note, locked).Scan(&s.SignedAt); err != nil {
		return periodClose{}, signOff{}, fmt.Errorf("closing: sign off close %s: %w", s.CloseID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE closing.period_close SET status = 'signed_off', version = version + 1, updated_by = $2 WHERE id = $1`,
		s.CloseID, p.ID); err != nil {
		return periodClose{}, signOff{}, fmt.Errorf("closing: set close %s signed off: %w", s.CloseID, err)
	}
	c, err := readClose(ctx, tx, s.CloseID)
	return c, s, err
}

// recordSignOff audits a sign-off and publishes it.
func recordSignOff(ctx context.Context, c periodClose, s signOff) error {
	periods := make([]map[string]any, len(s.Ledgers))
	for i, l := range s.Ledgers {
		periods[i] = map[string]any{"ledger_id": l.LedgerID, "ledger_code": l.LedgerCode, "fiscal_period_id": l.PeriodID, "locked": l.Locked}
	}
	return audit.Record(ctx, "closing.period_close.sign_off", "closing.period_close", c.ID,
		map[string]any{"legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear, "fiscal_period": c.FiscalPeriod, "sign_off_id": s.ID,
			"note": s.Note, "periods": periods, "version": c.Version},
		outbox.Event{Context: "closing", Name: "period_close_signed_off", AggregateType: "closing.period_close", AggregateID: c.ID,
			Payload: map[string]any{"period_close_id": c.ID, "sign_off_id": s.ID, "legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear,
				"fiscal_period": c.FiscalPeriod, "signed_by": s.SignedBy, "note": s.Note, "periods": periods, "status": c.Status, "version": c.Version}})
}

// signOffsOf reads a close's sign-offs, oldest first.
func signOffsOf(ctx context.Context, tx pgx.Tx, closeID uuid.UUID) ([]signOff, error) {
	rows, _ := tx.Query(ctx, `SELECT id, close_id, signed_by, signed_at, note, locked FROM closing.sign_off WHERE close_id = $1 ORDER BY signed_at, id`, closeID)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (signOff, error) {
		var s signOff
		err := r.Scan(&s.ID, &s.CloseID, &s.SignedBy, &s.SignedAt, &s.Note, &s.Ledgers)
		return s, errs.Wrapf(err, "scan sign-off")
	})
	if err != nil {
		return nil, fmt.Errorf("closing: sign-offs of close %s: %w", closeID, err)
	}
	return out, nil
}

// writable tells whether a write may go ahead on a close the caller holds
// the lock of (ADR-0058 §7): an open close, yes; a signed-off close whose
// covered periods are all locked, no (closing.close_signed_off); a
// signed-off close one of whose periods is no longer locked (an approved
// unlock, T32), yes, after reopen: it returns those periods' ledgers.
func (e *Endpoints) writable(ctx context.Context, tx pgx.Tx, closeID uuid.UUID) ([]string, error) {
	c, err := readClose(ctx, tx, closeID)
	if err != nil || c.Status != CloseSignedOff {
		return nil, err
	}
	covered, err := e.coveredPeriods(ctx, tx, c.ID, c.LegalEntityID, c.FiscalYear, c.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	var unlocked []string
	for _, k := range covered {
		if k.Period.Status != periodLocked {
			unlocked = append(unlocked, k.Code)
		}
	}
	if len(unlocked) == 0 {
		return nil, signedOffConflict(c)
	}
	return unlocked, nil
}

// reopen sets a signed-off close open again on its first write after an
// unlock, audited as reopened and published, before the write's own
// change; unlocked names the ledgers whose periods are no longer locked.
// Nothing is done for a close that is open (unlocked empty).
func reopen(ctx context.Context, tx pgx.Tx, p reqctx.Principal, closeID uuid.UUID, unlocked []string) error {
	if len(unlocked) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE closing.period_close SET status = 'open', version = version + 1, updated_by = $2 WHERE id = $1 AND status = 'signed_off'`,
		closeID, p.ID); err != nil {
		return fmt.Errorf("closing: reopen close %s: %w", closeID, err)
	}
	c, err := readClose(ctx, tx, closeID)
	if err != nil {
		return err
	}
	return record(ctx, c, change{action: "reopen", name: "reopened", detail: map[string]any{"unlocked_ledgers": unlocked}})
}
