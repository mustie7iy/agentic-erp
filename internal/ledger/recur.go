package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Recurring entries (ADR-0056): the workflows' dispatcher asks, as the
// worker role, which tenants have templates due, and in each tenant which
// dates are due on its companies' days (DueOccurrences). It starts one
// workflow per template and date, whose one activity parks the entry
// (ParkOccurrence), as the ERP's recurrer, for the template's owner. The
// database keeps one entry per template and date
// (parked_entry_occurrence_once), and the activity deals with a template's
// dates in order under its lock, skipping one behind its next date, so
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
	OccurrenceSkipped = "skipped" // nothing to do: a date behind the template's next (parked, refused or off its schedule), or a template retired, unscheduled or unowned
)

// RecurringTenants lists, as the worker role across tenants, the tenants
// with an active template a person owns whose next date is on or before
// UTC's date at now plus a day. That is a superset: no zone's date is more
// than a day after UTC's (UTC+14 is the furthest ahead), and the worker
// role cannot read the companies' zones, so DueOccurrences picks the dates
// in each tenant.
func RecurringTenants(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM ledger.entry_template WHERE status = 'active' AND owner_id IS NOT NULL
		AND next_due_on <= $1::date AND tenant_id IN `+database.ActiveTenants+` ORDER BY tenant_id`, clock.DayIn(now, time.UTC).AddDate(0, 0, 1))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("ledger: list tenants with entry templates due: %w", err)
	}
	return tenants, nil
}

// Occurrence is a due date of a template, at the version the template had
// when it was found due.
type Occurrence struct {
	TemplateID uuid.UUID
	Date       time.Time
	Version    int32
}

// DueOccurrences lists the tenant's due dates, in the request's tenant
// scope: for each active template a person owns, its next date
// (next_due_on) when that is today or past on its company's day, in its
// time zone. Dates are dealt with one at a time and in order: the next is
// listed once this one is parked or refused, so a missed run catches up
// one date per template and round.
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
		// next_due_on is set: the query selects on it.
		if !t.NextDueOn.After(today.day) {
			out = append(out, Occurrence{TemplateID: t.ID, Date: *t.NextDueOn, Version: t.Version})
		}
	}
	return out, nil
}

// Errors of a date that is not the template's to deal with yet: the
// activity is retried until it is.
var (
	ErrEarlierDatePending = errors.New("an earlier date of the template is not parked or refused yet")
	ErrNotDueYet          = errors.New("the date is not due yet on the company's day")
)

// ParkOccurrence parks the template's entry for the due date, in the
// request's tenant scope as the recurrer, owned by the template's owner,
// and moves the template's next date on. The template is read FOR UPDATE,
// so runs for its dates take turns, and its dates are dealt with strictly
// in order (next_due_on): a date before the next one, parked, refused or
// no longer the schedule's, is skipped, so a second run, or one whose
// completion was lost, does nothing and publishes nothing; a date after
// the next one, or not due yet on the company's day, is an error the
// workflow retries, so no date is passed over unseen. A date of a template
// retired, without a schedule or that no person owns, is skipped. A date
// whose entry is refused (an account blocked since the template was
// saved, an owner who may no longer park in the company) is recorded on
// the template with the problem, published (EntryTemplateChanged) and
// passed over: the template is corrected and that date parked with :park.
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
	if due, err := e.due(ctx, t, date); err != nil || !due {
		return OccurrenceSkipped, err
	}
	err = e.ownerMayPark(ctx, tx, p, t)
	if err == nil {
		err = e.parkDue(ctx, tx, p, t, date)
	}
	if err != nil {
		// A refusal, the checks' or the database's (a constraint's
		// problem), is the template's to fix; anything else, a lock
		// timeout included, is retried.
		if why := problem.From(err); why.Status < 500 && why.RetryAfter == 0 {
			return OccurrenceRefused, recordRefusal(ctx, tx, p, t, date, why)
		}
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET last_occurrence_on = $2, next_due_on = $3,
			updated_by = $4, version = version + 1 WHERE id = $1`, t.ID, date, t.Schedule.Next(&date), p.ID); err != nil {
		return "", fmt.Errorf("ledger: move entry template %s past %s: %w", t.ID, date.Format(time.DateOnly), err)
	}
	return OccurrenceParked, nil
}

// due tells whether date is the template's to park now: it is active, a
// person owns it, and date is its next date (next_due_on), today or past on
// its company's day. A date before the next is not; a later date, or one
// not due yet, is an error to retry.
func (e *Endpoints) due(ctx context.Context, t EntryTemplate, date time.Time) (bool, error) {
	next := t.NextDueOn
	switch {
	case t.Status != TemplateActive || t.OwnerID == nil || next == nil || date.Before(*next):
		return false, nil
	case date.After(*next):
		return false, fmt.Errorf("ledger: template %s on %s, next due %s: %w", t.ID, date.Format(time.DateOnly), next.Format(time.DateOnly),
			ErrEarlierDatePending)
	}
	today, err := e.days().of(ctx, t.LegalEntityID)
	if err != nil {
		return false, err
	}
	if date.After(today.day) {
		return false, fmt.Errorf("ledger: template %s on %s: %w", t.ID, date.Format(time.DateOnly), ErrNotDueYet)
	}
	return true, nil
}

// ParkPermission is parkJournalEntry's x-permission (TestParkPermission
// reads it from the contract): what a template's owner needs for the
// recurrer to park for them.
const ParkPermission = "ledger.entry.park"

// ownerMayPark refuses an entry for an owner who may no longer park it
// (ADR-0056): one no longer active, without ledger.entry.park unbarred, or
// without access to the template's company. The recurrer parks for the
// owner, so the owner's standing today is what counts.
func (e *Endpoints) ownerMayPark(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t EntryTemplate) error {
	if e.People == nil {
		return errors.New("ledger: no directory of people to read the template owner's standing from")
	}
	active, err := e.People.Active(ctx, tx, *t.OwnerID)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	refused := problem.Forbidden("ledger.owner_not_permitted",
		"The template's owner may no longer park entries in its company; the date is passed over.")
	if !active {
		return refused
	}
	owner := reqctx.WithPrincipal(ctx, reqctx.Principal{ID: *t.OwnerID, Kind: reqctx.KindHuman, TenantID: p.TenantID})
	usable, _, err := authz.Effective(owner)
	if err != nil {
		return err
	}
	access, err := authz.Companies(owner)
	if err != nil {
		return err
	}
	if !slices.Contains(usable, ParkPermission) || !access.Allows(t.LegalEntityID) {
		return refused
	}
	return nil
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
// refused, and why, moves its next date past it, and publishes it.
func recordRefusal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t EntryTemplate, date time.Time, why *problem.Problem) error {
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET refused_on = $2, refusal_code = $3, refusal_detail = $4, next_due_on = $5,
			updated_by = $6, version = version + 1 WHERE id = $1`, t.ID, date, why.Code, why.Detail, t.Schedule.Next(&date), p.ID); err != nil {
		return fmt.Errorf("ledger: record the refusal of template %s on %s: %w", t.ID, date.Format(time.DateOnly), err)
	}
	updated, err := scanTemplate(tx.QueryRow(ctx, templateSelect+` WHERE t.id = $1`, t.ID))
	if err != nil {
		return fmt.Errorf("ledger: read entry template %s: %w", t.ID, err)
	}
	return templateChanged(ctx, updated, nil, "refuse", map[string]any{"occurrence_date": date.Format(time.DateOnly), "code": why.Code,
		"detail": why.Detail})
}
