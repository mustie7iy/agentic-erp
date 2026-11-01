package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Entry templates (ADR-0056, product owner 2026-11-01): a saved
// parked-entry body, header and lines without dates. A person parks an
// entry from it once (useTemplate), or gives it a schedule and the ERP
// parks one entry per due date for its owner (recur.go), who posts it as
// any parked entry: nothing is posted automatically. Amounts are fixed per
// date and editable on the parked entry before it is posted.

// Entry template statuses.
const (
	TemplateActive  = "active"
	TemplateRetired = "retired"
)

// EntryTemplate is a saved manual entry without its dates, its lines in
// transaction currency (Line.TC).
type EntryTemplate struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	LedgerID       uuid.UUID // as on a parked entry
	LedgerSpecific bool
	LedgerGroup    *string
	Name           string
	DocumentType   string
	Currency       string
	Exp            int
	Reference      *string
	HeaderText     *string
	Status         string
	CreatedByKind  string
	OwnerID        *uuid.UUID
	Schedule       *Schedule
	// Kept by the ERP: the schedule's next date, the last it parked, and
	// the last it could not park.
	NextDueOn        *time.Time
	LastOccurrenceOn *time.Time
	Refusal          *Refusal
	Lines            []Line
	Debits           money.Minor
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

// Refusal is a due date the ERP could not park an entry for, and the
// problem that refused it.
type Refusal struct {
	On     time.Time
	Code   string
	Detail string
}

// through is the last due date the ERP dealt with, parked or refused; the
// schedule goes on after it.
func (t EntryTemplate) through() *time.Time {
	last := t.LastOccurrenceOn
	if r := t.Refusal; r != nil && (last == nil || r.On.After(*last)) {
		last = &r.On
	}
	return last
}

// entry is the template as a manual entry dated day.
func (t EntryTemplate) entry(day time.Time) NewEntry {
	in := NewEntry{LegalEntityID: t.LegalEntityID, LedgerGroup: t.LedgerGroup, DocumentType: t.DocumentType, DocumentDate: day, PostingDate: day,
		Currency: t.Currency, Reference: t.Reference, HeaderText: t.HeaderText, Lines: asNewLines(t.Lines, t.Currency, t.Exp)}
	if t.LedgerSpecific {
		in.LedgerID = &t.LedgerID
	}
	return in
}

var templateSelect = `SELECT t.id, t.legal_entity_id, t.ledger_id, t.ledger_specific,
		(SELECT g.code FROM ledger.ledger_group g WHERE g.tenant_id = t.tenant_id AND g.id = t.ledger_group_id),
		t.name, t.document_type, t.currency, c.exponent, t.reference, t.header_text, t.status, t.created_by_kind, t.owner_id,
		t.frequency, t.day_of_month, t.starts_on, t.ends_on, t.occurrences, t.next_due_on, t.last_occurrence_on,
		t.refused_on, t.refusal_code, t.refusal_detail, t.version, t.created_at, t.created_by, t.updated_at, t.updated_by,
		` + templateLines.read("t") + `
	FROM ledger.entry_template t JOIN ref.currency c ON c.code = t.currency`

func scanTemplate(row pgx.Row) (EntryTemplate, error) {
	var t EntryTemplate
	var frequency, code, detail *string
	var day *int16
	var starts, refused *time.Time
	var s Schedule
	err := row.Scan(&t.ID, &t.LegalEntityID, &t.LedgerID, &t.LedgerSpecific, &t.LedgerGroup, &t.Name, &t.DocumentType, &t.Currency, &t.Exp,
		&t.Reference, &t.HeaderText, &t.Status, &t.CreatedByKind, &t.OwnerID, &frequency, &day, &starts, &s.EndsOn, &s.Count,
		&t.NextDueOn, &t.LastOccurrenceOn, &refused, &code, &detail, &t.Version, &t.CreatedAt, &t.CreatedBy, &t.UpdatedAt, &t.UpdatedBy,
		&t.Lines, &t.Debits)
	// The database holds a schedule's fields together
	// (entry_template_schedule), and a refusal's (entry_template_refusal).
	if err == nil && frequency != nil {
		s.Frequency, s.Day, s.StartsOn = *frequency, int(*day), *starts
		t.Schedule = &s
	}
	if err == nil && refused != nil {
		t.Refusal = &Refusal{On: *refused, Code: *code, Detail: *detail}
	}
	return t, errs.Wrapf(err, "scan entry template")
}

// getTemplate loads an entry template of a legal entity the caller may
// act in; lock reads it FOR UPDATE (ADR-0008).
func getTemplate(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (EntryTemplate, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return EntryTemplate{}, err
	}
	q := templateSelect + ` WHERE t.id = $1 AND ($2 OR t.legal_entity_id = ANY($3))`
	if lock {
		q += ` FOR UPDATE OF t`
	}
	t, err := scanTemplate(tx.QueryRow(ctx, q, id, access.All, access.IDs))
	if errors.Is(err, pgx.ErrNoRows) {
		return EntryTemplate{}, problem.NotFoundOf("entry template")
	}
	if err != nil {
		return EntryTemplate{}, fmt.Errorf("ledger: read entry template %s: %w", id, err)
	}
	return t, nil
}

func init() {
	problem.RegisterConstraint("entry_template_name_once", func() *problem.Problem {
		return problem.Conflict("ledger.template_name_taken", "The legal entity has an entry template of this name already.")
	})
	problem.RegisterConstraint("entry_template_line_uom_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.unknown_unit", "A quantity's unit of measure is not known.",
			problem.FieldError{Field: "lines.quantity.unit", Code: "unknown", Message: "a unit of measure code from the reference data"})
	})
}

// NewTemplate is an entry template to save.
type NewTemplate struct {
	LegalEntityID uuid.UUID
	LedgerID      *uuid.UUID
	LedgerGroup   *string
	Name          string
	DocumentType  string
	Currency      string
	Reference     *string
	HeaderText    *string
	Lines         []NewLine
	Schedule      *Schedule
}

// checked is a template checked as a parked entry is.
type checked struct {
	v    prepared
	next *time.Time // the schedule's next date after the last one dealt with
}

// check checks the template as parking checks an entry, on its schedule's
// next date or else the company's date today, and refuses a schedule its
// fields cannot describe. through is the last date the ERP dealt with.
func (e *Endpoints) check(ctx context.Context, tx pgx.Tx, in NewTemplate, through *time.Time) (checked, error) {
	var next *time.Time
	if s := in.Schedule; s != nil {
		if err := s.check(); err != nil {
			return checked{}, err
		}
		next = s.Next(through)
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return checked{}, err
	}
	day := next
	if day == nil {
		today, err := e.days().of(ctx, le.ID)
		if err != nil {
			return checked{}, err
		}
		day = &today.day
	}
	entry := NewEntry{LegalEntityID: le.ID, LedgerID: in.LedgerID, LedgerGroup: in.LedgerGroup, DocumentType: in.DocumentType,
		DocumentDate: *day, PostingDate: *day, Currency: in.Currency, Reference: in.Reference, HeaderText: in.HeaderText, Lines: in.Lines}
	v, err := e.prepareFor(ctx, tx, le, entry)
	if err != nil {
		return checked{}, err
	}
	if err := parkable(ctx, tx, v, *day); err != nil {
		return checked{}, err
	}
	return checked{v: v, next: next}, nil
}

// scheduleArgs are the schedule's columns: frequency, day_of_month,
// starts_on, ends_on and occurrences.
func scheduleArgs(s *Schedule) []any {
	if s == nil {
		return []any{nil, nil, nil, nil, nil}
	}
	return []any{s.Frequency, int16(s.Day), s.StartsOn, s.EndsOn, s.Count}
}

// createTemplate saves a template. A person owns what they save; an
// agent's or a service's template has no owner until a person takes it on
// by changing it, and parks nothing on its schedule until then.
func (e *Endpoints) createTemplate(ctx context.Context, in NewTemplate) (EntryTemplate, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return EntryTemplate{}, err
	}
	c, err := e.check(ctx, tx, in, nil)
	if err != nil {
		return EntryTemplate{}, err
	}
	id := uuid.Must(uuid.NewV7())
	var owner *uuid.UUID
	if p.Kind == reqctx.KindHuman {
		owner = &p.ID
	}
	args := append([]any{p.TenantID, id, c.v.le.ID, in.Name, c.v.choice.ledger, c.v.choice.specific, c.v.choice.group, in.DocumentType, in.Currency,
		in.Reference, in.HeaderText, p.Kind, owner, p.ID, c.next}, scheduleArgs(in.Schedule)...)
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.entry_template (tenant_id, id, legal_entity_id, name, ledger_id, ledger_specific, ledger_group_id,
			document_type, currency, reference, header_text, created_by_kind, owner_id, created_by, updated_by, next_due_on,
			frequency, day_of_month, starts_on, ends_on, occurrences)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, $15, $16, $17, $18, $19, $20)`, args...); err != nil {
		return EntryTemplate{}, fmt.Errorf("ledger: save entry template %s: %w", id, err)
	}
	return e.saved(ctx, tx, p, id, c.v.lines, nil, "create")
}

// saved writes the template's lines and records the change.
func (e *Endpoints) saved(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, lines []Line, previous *string, action string) (EntryTemplate, error) {
	if err := putTemplateLines(ctx, tx, p.TenantID, id, lines); err != nil {
		return EntryTemplate{}, err
	}
	t, err := getTemplate(ctx, tx, id, false)
	if err != nil {
		return EntryTemplate{}, err
	}
	return t, templateChanged(ctx, t, previous, action, nil)
}

// templateChanged records a change of a template: the audit record of the
// action, with extra detail, and EntryTemplateChanged with its status
// before (previous; nil unless it changed).
func templateChanged(ctx context.Context, t EntryTemplate, previous *string, action string, extra map[string]any) error {
	detail := map[string]any{"name": t.Name, "status": t.Status, "previous_status": previous, "lines": len(t.Lines),
		"next_due_on": outbox.Date(t.NextDueOn), "owner_id": t.OwnerID}
	maps.Copy(detail, extra)
	var refusedOn *time.Time
	var refusal *string
	if r := t.Refusal; r != nil {
		refusedOn, refusal = &r.On, &r.Code
	}
	return audit.Record(ctx, "ledger.entry_template."+action, "ledger.entry_template", t.ID, detail,
		outbox.Event{Context: "ledger", Name: "entry_template_changed", AggregateType: "ledger.entry_template", AggregateID: t.ID,
			Payload: map[string]any{"entry_template_id": t.ID, "legal_entity_id": t.LegalEntityID, "name": t.Name, "status": t.Status,
				"previous_status": previous, "owner_id": t.OwnerID, "amount": money.EventAmount(t.Debits, t.Currency, t.Exp),
				"next_due_on": outbox.Date(t.NextDueOn), "last_occurrence_on": outbox.Date(t.LastOccurrenceOn),
				"refused_on": outbox.Date(refusedOn), "refusal_code": refusal}})
}

// TemplateChange is a merge patch of a template: nil fields are unchanged,
// a nil *string in Reference or HeaderText clears it, a nil *Schedule in
// Schedule removes the schedule, and Lines replaces the lines as a whole.
type TemplateChange struct {
	Name, DocumentType, Currency *string
	Reference, HeaderText        **string
	Schedule                     **Schedule
	Lines                        []NewLine
}

// changedTemplate is the template with the change applied.
func changedTemplate(t EntryTemplate, ch TemplateChange) NewTemplate {
	in := NewTemplate{LegalEntityID: t.LegalEntityID, LedgerGroup: t.LedgerGroup, Name: deref(ch.Name, t.Name),
		DocumentType: deref(ch.DocumentType, t.DocumentType), Currency: deref(ch.Currency, t.Currency),
		Reference: deref(ch.Reference, t.Reference), HeaderText: deref(ch.HeaderText, t.HeaderText),
		Schedule: deref(ch.Schedule, t.Schedule), Lines: ch.Lines}
	if t.LedgerSpecific {
		in.LedgerID = &t.LedgerID
	}
	if in.Lines == nil {
		in.Lines = asNewLines(t.Lines, t.Currency, t.Exp)
	}
	return in
}

// fields names what a change sets, for the audit trail.
func (ch TemplateChange) fields() []string {
	out := []string{}
	for _, f := range []struct {
		name string
		set  bool
	}{{"name", ch.Name != nil}, {"document_type", ch.DocumentType != nil}, {"currency", ch.Currency != nil}, {"reference", ch.Reference != nil},
		{"header_text", ch.HeaderText != nil}, {"schedule", ch.Schedule != nil}, {"lines", ch.Lines != nil}} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// owned locks an active template the caller may change and returns it
// with its owner once the caller acts on it (takenOn: its owner; a person
// taking on an agent's or service's template; or the principal that made
// one no person owns).
func owned(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error) (EntryTemplate, *uuid.UUID, error) {
	t, err := getTemplate(ctx, tx, id, true)
	if err != nil {
		return EntryTemplate{}, nil, err
	}
	if err := ifMatch(t.Version); err != nil {
		return EntryTemplate{}, nil, err
	}
	if t.Status != TemplateActive {
		return EntryTemplate{}, nil, errRetired()
	}
	owner, ok := takenOn(t.OwnerID, t.CreatedBy, p)
	if !ok {
		return EntryTemplate{}, nil, problem.Forbidden("ledger.not_template_owner",
			"Only the owner of an entry template changes or retires it: the person who saved it or took it on.")
	}
	return t, owner, nil
}

func errRetired() error {
	return problem.Conflict("ledger.template_retired", "The entry template is retired; it changes no more and parks nothing.")
}

// changeTemplate changes a template and checks it again. Its schedule goes
// on after the last date the ERP dealt with.
func (e *Endpoints) changeTemplate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch TemplateChange) (EntryTemplate, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return EntryTemplate{}, err
	}
	t, owner, err := owned(ctx, tx, p, id, ifMatch)
	if err != nil {
		return EntryTemplate{}, err
	}
	in := changedTemplate(t, ch)
	c, err := e.check(ctx, tx, in, t.through())
	if err != nil {
		return EntryTemplate{}, err
	}
	args := append([]any{id, in.Name, c.v.choice.ledger, in.DocumentType, in.Currency, in.Reference, in.HeaderText, owner, p.ID, c.next},
		scheduleArgs(in.Schedule)...)
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET name = $2, ledger_id = $3, document_type = $4, currency = $5, reference = $6,
			header_text = $7, owner_id = $8, updated_by = $9, next_due_on = $10, frequency = $11, day_of_month = $12, starts_on = $13, ends_on = $14,
			occurrences = $15, version = version + 1 WHERE id = $1`, args...); err != nil {
		return EntryTemplate{}, fmt.Errorf("ledger: change entry template %s: %w", id, err)
	}
	if err := putTemplateLines(ctx, tx, p.TenantID, id, c.v.lines); err != nil {
		return EntryTemplate{}, err
	}
	updated, err := getTemplate(ctx, tx, id, false)
	if err != nil {
		return EntryTemplate{}, err
	}
	return updated, templateChanged(ctx, updated, nil, "update",
		map[string]any{"fields": ch.fields(), "taken_on": t.OwnerID == nil && owner != nil})
}

// retireTemplate retires a template for good: it changes no more and parks
// nothing. Entries parked from it stay as they are.
func retireTemplate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (EntryTemplate, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return EntryTemplate{}, err
	}
	t, owner, err := owned(ctx, tx, p, id, ifMatch)
	if err != nil {
		return EntryTemplate{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.entry_template SET status = 'retired', next_due_on = NULL, owner_id = $2, updated_by = $3,
			version = version + 1 WHERE id = $1`, id, owner, p.ID); err != nil {
		return EntryTemplate{}, fmt.Errorf("ledger: retire entry template %s: %w", id, err)
	}
	retired, err := getTemplate(ctx, tx, id, false)
	if err != nil {
		return EntryTemplate{}, err
	}
	return retired, templateChanged(ctx, retired, &t.Status, "retire", nil)
}

// TemplateUse is a use of a template once: the entry's posting date, and
// its document date (the posting date when nil) and reference (the
// template's when nil).
type TemplateUse struct {
	PostingDate  time.Time
	DocumentDate *time.Time
	Reference    *string
}

// useTemplate parks an entry from an active template at the version the
// caller read, owned as the caller
// parks any entry and naming the template; from there it is an ordinary
// parked entry.
func (e *Endpoints) useTemplate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, use TemplateUse) (ParkedEntry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	t, err := getTemplate(ctx, tx, id, false)
	if err != nil {
		return ParkedEntry{}, err
	}
	if err := ifMatch(t.Version); err != nil {
		return ParkedEntry{}, err
	}
	if t.Status != TemplateActive {
		return ParkedEntry{}, errRetired()
	}
	in := t.entry(use.PostingDate)
	in.DocumentDate = deref(use.DocumentDate, use.PostingDate)
	if use.Reference != nil {
		in.Reference = use.Reference
	}
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return ParkedEntry{}, err
	}
	return parkPrepared(ctx, tx, p, v, in, parking{template: &t.ID})
}
