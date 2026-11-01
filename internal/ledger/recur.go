package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Recurring entries (ADR-0056): the workflows' dispatcher asks, as the
// worker role, which tenants have templates due, and in each tenant which
// dates are due on its companies' days (DueOccurrences). It starts one
// workflow per template and date, whose one activity parks the entry
// (ParkOccurrence), as the ERP's recurrer, for the template's owner. The
// database keeps one entry per template and date
// (parked_entry_occurrence_once), and the activity looks for it first, so
// running it twice parks nothing twice and publishes nothing twice.

// RecurrerActor is the principal of recurring entries, in every tenant: a
// system actor, never a token holder.
var RecurrerActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:entry-recurrer"))

// Recurrer is the principal a recurring entry is parked as in the tenant.
func Recurrer(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: RecurrerActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// What ParkOccurrence did with a due date.
const (
	OccurrenceParked  = "parked"  // an entry parked for the owner
	OccurrenceRefused = "refused" // parking it was refused; the template says why
	OccurrenceSkipped = "skipped" // nothing to do: parked before, not due, not a date of the schedule, or retired
)

// RecurringTenants lists, as the worker role across tenants, the tenants
// with an active template a person owns whose next date is on or before
// UTC's date at now plus a day. That is a superset: no zone's date is more
// than a day after UTC's (UTC+14 is the furthest ahead), and the worker
// role cannot read the companies' zones, so DueOccurrences picks the dates
// in each tenant.
func RecurringTenants(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM ledger.entry_template WHERE status = 'active' AND owner_id IS NOT NULL
		AND next_due_on <= $1::date ORDER BY tenant_id`, clock.DayIn(now, time.UTC).AddDate(0, 0, 1))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("ledger: list tenants with entry templates due: %w", err)
	}
	return tenants, nil
}

// Occurrence is a due date of a template.
type Occurrence struct {
	TemplateID uuid.UUID
	Date       time.Time
}

// DueOccurrences lists the tenant's due dates, in the request's tenant
// scope: for each active template a person owns, the dates of its
// schedule after the last one dealt with up to its company's date today,
// in its time zone, oldest first per template.
func (e *Endpoints) DueOccurrences(ctx context.Context) ([]Occurrence, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, templateSelect+` WHERE t.status = 'active' AND t.owner_id IS NOT NULL AND t.next_due_on <= $1::date
		ORDER BY t.next_due_on, t.id`, clock.DayIn(clock.Now(e.Now), time.UTC).AddDate(0, 0, 1))
	templates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntryTemplate, error) { return scanTemplate(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list entry templates due: %w", err)
	}
	var out []Occurrence
	for _, t := range templates {
		today, err := e.days().of(ctx, t.LegalEntityID)
		if err != nil {
			return nil, err
		}
		// next_due_on is set only with a schedule (entry_template_schedule).
		for _, d := range t.Schedule.Due(t.through(), today.day) {
			out = append(out, Occurrence{TemplateID: t.ID, Date: d})
		}
	}
	return out, nil
}

// ParkOccurrence parks the template's entry for the due date, in the
// request's tenant scope as the recurrer, owned by the template's owner,
// and moves the template's next date on. The template is read FOR UPDATE,
// so runs for its dates take turns, and the entry is looked for first: a
// second run, or one whose completion was lost, finds it and does
// nothing. A date that is not due yet on the company's day, or no longer
// one of the schedule's, or of a template retired or no person owns, is
// skipped. A date whose entry parking refuses (an account blocked since
// the template was saved) is recorded on the template with the problem,
// published (EntryTemplateChanged) and passed over: the owner corrects the
// template and parks that date with :park.
func (e *Endpoints) ParkOccurrence(ctx context.Context, template uuid.UUID, date time.Time) (string, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	t, err := scanTemplate(tx.QueryRow(ctx, templateSelect+` WHERE t.id = $1 FOR UPDATE OF t`, template))
	if errors.Is(err, pgx.ErrNoRows) {
		return OccurrenceSkipped, nil
	}
	if err != nil {
		return "", fmt.Errorf("ledger: lock entry template %s: %w", template, err)
	}
	if due, err := e.due(ctx, tx, t, date); err != nil || !due {
		return OccurrenceSkipped, err
	}
	if err := e.parkDue(ctx, tx, p, t, date); err != nil {
		// A refusal, the checks' or the database's (a constraint's
		// problem), is the template's to fix; anything else, a lock
		// timeout included, is retried.
		if why := problem.From(err); why.Status < 500 && why.RetryAfter == 0 {
			return OccurrenceRefused, recordRefusal(ctx, tx, p, t, date, why)
		}
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET last_occurrence_on = greatest(last_occurrence_on, $2), next_due_on = $3,
			updated_by = $4, version = version + 1 WHERE id = $1`, t.ID, date, t.Schedule.Next(laterOf(t.through(), date)), p.ID); err != nil {
		return "", fmt.Errorf("ledger: move entry template %s past %s: %w", t.ID, date.Format(time.DateOnly), err)
	}
	return OccurrenceParked, nil
}

// laterOf is the later of a date the template dealt with and date.
func laterOf(through *time.Time, date time.Time) *time.Time {
	if through != nil && through.After(date) {
		return through
	}
	return &date
}

// due tells whether the template parks an entry for date now: it is
// active, a person owns it, date is one of its schedule's dates and today
// or past on its company's day, and no entry was parked for it.
func (e *Endpoints) due(ctx context.Context, tx pgx.Tx, t EntryTemplate, date time.Time) (bool, error) {
	if t.Status != TemplateActive || t.OwnerID == nil || t.Schedule == nil || !t.Schedule.Keeps(date) {
		return false, nil
	}
	today, err := e.days().of(ctx, t.LegalEntityID)
	if err != nil || date.After(today.day) {
		return false, err
	}
	var parked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.parked_entry WHERE template_id = $1 AND occurrence_date = $2)`,
		t.ID, date).Scan(&parked); err != nil {
		return false, fmt.Errorf("ledger: look for the entry of template %s on %s: %w", t.ID, date.Format(time.DateOnly), err)
	}
	return !parked, nil
}

// parkDue parks the template's entry dated date in a savepoint, so a
// refusal, from the checks or the database, leaves the transaction usable
// to record it. The template's company is checked active as a person's
// entry's is, without the recurrer's company access: it acts for the
// owner in the template's company.
func (e *Endpoints) parkDue(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t EntryTemplate, date time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: savepoint for template %s: %w", t.ID, err)
	}
	defer sp.Rollback(context.WithoutCancel(ctx))
	ctx = reqctx.WithTx(ctx, sp)
	le, err := e.activeEntity(ctx, authz.Access{All: true}, t.LegalEntityID, "legal_entity_id")
	if err != nil {
		return err
	}
	in := t.entry(date)
	v, err := e.prepareFor(ctx, sp, le, in)
	if err != nil {
		return err
	}
	// Published as parked (ParkedJournalEntryChanged), naming the template
	// and the date: the owner's notice besides their list (owned_by=me).
	if _, err := parkPrepared(ctx, sp, p, v, in, parking{template: &t.ID, occurrence: &date, owner: t.OwnerID}); err != nil {
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("ledger: release savepoint for template %s: %w", t.ID, err)
	}
	return nil
}

// recordRefusal records on the template that its date's entry was
// refused, and why, moves its next date past it, and publishes it. The
// template keeps its latest refused date: one refused before an earlier
// date is recorded in the audit trail and the event only.
func recordRefusal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t EntryTemplate, date time.Time, why *problem.Problem) error {
	on, code, detail := date, why.Code, why.Detail
	if r := t.Refusal; r != nil && r.On.After(date) {
		on, code, detail = r.On, r.Code, r.Detail
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET refused_on = $2, refusal_code = $3, refusal_detail = $4, next_due_on = $5,
			updated_by = $6, version = version + 1 WHERE id = $1`, t.ID, on, code, detail, t.Schedule.Next(laterOf(t.through(), date)), p.ID); err != nil {
		return fmt.Errorf("ledger: record the refusal of template %s on %s: %w", t.ID, date.Format(time.DateOnly), err)
	}
	updated, err := scanTemplate(tx.QueryRow(ctx, templateSelect+` WHERE t.id = $1`, t.ID))
	if err != nil {
		return fmt.Errorf("ledger: read entry template %s: %w", t.ID, err)
	}
	return templateChanged(ctx, updated, nil, "refuse", map[string]any{"occurrence_date": date.Format(time.DateOnly), "code": why.Code,
		"detail": why.Detail})
}
