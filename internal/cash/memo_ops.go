package cash

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CreateMemo creates an active memo record, or an override of a series'
// occurrence (RecurrenceOf, OccurrenceDate).
func (e *Endpoints) CreateMemo(ctx context.Context, in MemoInput) (MemoRecord, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MemoRecord{}, err
	}
	c, err := e.checker(ctx, tx, in.LegalEntityID)
	if err != nil {
		return MemoRecord{}, err
	}
	v, err := c.values(ctx, in)
	if err != nil {
		return MemoRecord{}, err
	}
	if err := overrides(ctx, tx, in, v); err != nil {
		return MemoRecord{}, err
	}
	id, err := insertMemo(ctx, tx, p, in.LegalEntityID, v, memoMeta{status: MemoActive, origin: OriginManual})
	if err != nil {
		return MemoRecord{}, err
	}
	return written(ctx, tx, id, "create", "created")
}

// written reads a record just written back, and audits and publishes the
// change.
func written(ctx context.Context, tx pgx.Tx, id uuid.UUID, action, change string) (MemoRecord, error) {
	m, err := memo(ctx, tx, id, false)
	if err != nil {
		return MemoRecord{}, err
	}
	return m, record(ctx, m, action, change)
}

// occurrenceInvalid refuses an override of an occurrence a series does not
// have.
func occurrenceInvalid(detail string) error {
	return invalid("cash.occurrence_invalid", detail, "occurrence_date", "an occurrence of an active series of the legal entity in the record's currency")
}

// overrides checks an override: both its series and occurrence, the
// series an active one of the legal entity and currency, the day one of
// its occurrences. A second override of it is refused by the database
// (memo_record_override_once).
func overrides(ctx context.Context, tx pgx.Tx, in MemoInput, v memoValues) error {
	if (in.RecurrenceOf == nil) != (in.OccurrenceDate == nil) {
		return occurrenceInvalid("An override names its series (recurrence_of) and the occurrence (occurrence_date), both.")
	}
	if in.RecurrenceOf == nil {
		return nil
	}
	s, err := memo(ctx, tx, *in.RecurrenceOf, true)
	if problem.From(err).Status == 404 {
		return occurrenceInvalid("No series with this id.")
	}
	if err != nil {
		return err
	}
	if s.Recurrence == nil || s.Status != MemoActive || s.LegalEntityID != in.LegalEntityID || s.Currency != v.currency ||
		!s.Recurrence.has(s.ValueDate, *in.OccurrenceDate) {
		return occurrenceInvalid("The record overrides an occurrence of an active series of its legal entity and currency.")
	}
	return nil
}

// ProposeMemo records the calling agent's proposed memo record, in its
// running session, with its reason and the statement lines it rests on:
// counted nowhere until a person accepts it.
func (e *Endpoints) ProposeMemo(ctx context.Context, in MemoInput, session uuid.UUID, reason string, evidence []LineRef) (MemoRecord, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MemoRecord{}, err
	}
	if err := e.Sessions.Running(ctx, session); err != nil {
		return MemoRecord{}, err
	}
	c, err := e.checker(ctx, tx, in.LegalEntityID)
	if err != nil {
		return MemoRecord{}, err
	}
	v, err := c.values(ctx, in)
	if err != nil {
		return MemoRecord{}, err
	}
	for i, ref := range evidence {
		if _, err := e.line(ctx, ref, fmt.Sprintf("evidence[%d]", i), func(l StatementLine) bool { return l.LegalEntityID == in.LegalEntityID }); err != nil {
			return MemoRecord{}, err
		}
	}
	id, err := insertMemo(ctx, tx, p, in.LegalEntityID, v, memoMeta{status: MemoProposed, origin: OriginAgent, session: &session, reason: &reason, evidence: evidence})
	if err != nil {
		return MemoRecord{}, err
	}
	return written(ctx, tx, id, "propose", "proposed")
}

// line reads a statement line the request names at field, which must be
// one fits accepts.
func (e *Endpoints) line(ctx context.Context, ref LineRef, field string, fits func(StatementLine) bool) (StatementLine, error) {
	l, ok, err := e.Payments.StatementLine(ctx, ref.StatementID, ref.LineNo)
	if err != nil {
		return StatementLine{}, fmt.Errorf("cash: line %d of statement %s: %w", ref.LineNo, ref.StatementID, err)
	}
	if !ok || !fits(l) {
		return StatementLine{}, invalid("cash.statement_line_invalid", fmt.Sprintf("Line %d of statement %s is not a line of the record's legal entity, currency, bank and direction.",
			ref.LineNo, ref.StatementID), field, "a statement line of the record's legal entity, currency, house bank and direction")
	}
	return l, nil
}

// MemoPatch is a merge patch of a memo record: nil fields are unchanged; a
// HouseBankID, Description or Reference pointing to nil clears it.
type MemoPatch struct {
	HouseBankID            **uuid.UUID
	Amount, Currency       *string
	ValueDate, ExpiresOn   *time.Time
	Certainty, Item        *string
	Description, Reference **string
	Recurrence             *Recurrence
}

// input is the record as an input, with the patch applied: a new value
// date without an expiry keeps the days between them.
func (m MemoRecord) input(ch MemoPatch) MemoInput {
	in := MemoInput{LegalEntityID: m.LegalEntityID, HouseBankID: m.HouseBankID, Amount: m.Amount.Plain(m.Exponent), Currency: m.Currency,
		ValueDate: m.ValueDate, ExpiresOn: &m.ExpiresOn, Certainty: m.Certainty, Item: m.Item, Description: m.Description, Reference: m.Reference,
		Recurrence: m.Recurrence, RecurrenceOf: m.RecurrenceOf, OccurrenceDate: m.OccurrenceDate}
	set(&in.HouseBankID, ch.HouseBankID)
	set(&in.Amount, ch.Amount)
	set(&in.Currency, ch.Currency)
	if ch.ValueDate != nil {
		expires := ch.ValueDate.AddDate(0, 0, m.expiryDays())
		in.ValueDate, in.ExpiresOn = *ch.ValueDate, &expires
	}
	if ch.ExpiresOn != nil {
		in.ExpiresOn = ch.ExpiresOn
	}
	set(&in.Certainty, ch.Certainty)
	set(&in.Item, ch.Item)
	set(&in.Description, ch.Description)
	set(&in.Reference, ch.Reference)
	if ch.Recurrence != nil {
		in.Recurrence = ch.Recurrence
	}
	return in
}

// set sets *dst to *v when the patch has v.
func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// changeable refuses a change of a record that is closed.
func changeable(m MemoRecord, statuses ...string) error {
	for _, s := range statuses {
		if m.Status == s {
			return nil
		}
	}
	if m.Status == MemoProposed {
		return problem.Conflict("cash.memo_record_proposed", "The memo record is an agent's proposal; accept or reject it first.")
	}
	return problem.Conflict("cash.memo_record_closed", "The memo record is "+m.Status+"; it does not change any more.")
}

// UpdateMemo changes a proposed or active memo record, checked as a new
// one is. A series stays a series, a one-off record a one-off.
func (e *Endpoints) UpdateMemo(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch MemoPatch) (MemoRecord, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MemoRecord{}, err
	}
	m, err := memo(ctx, tx, id, true)
	if err != nil {
		return MemoRecord{}, err
	}
	if err := ifMatch(m.Version); err != nil {
		return MemoRecord{}, err
	}
	if err := changeable(m, MemoActive, MemoProposed); err != nil {
		return MemoRecord{}, err
	}
	if ch.Recurrence != nil && m.Recurrence == nil {
		return MemoRecord{}, invalid("request.validation_failed", "A one-off memo record stays one; create a series instead.", "recurrence", "only on a series")
	}
	c, err := e.checker(ctx, tx, m.LegalEntityID)
	if err != nil {
		return MemoRecord{}, err
	}
	v, err := c.values(ctx, m.input(ch))
	if err != nil {
		return MemoRecord{}, err
	}
	if err := keepsIdentity(ctx, tx, m, v); err != nil {
		return MemoRecord{}, err
	}
	if err := updateValues(ctx, tx, p, id, v, false); err != nil {
		return MemoRecord{}, err
	}
	return written(ctx, tx, id, "update", "updated")
}

// keepsIdentity refuses a change that would part a series' occurrences
// from their overrides (its value date, rule or currency, while it has
// overrides: the override's occurrence would no longer be one, and both
// would count), an override from its series' currency, and an uploaded
// record's reference, by which its batch knows it. The series is locked,
// so no override is created meanwhile.
func keepsIdentity(ctx context.Context, tx pgx.Tx, m MemoRecord, v memoValues) error {
	switch {
	case m.RecurrenceOf != nil && v.currency != m.Currency:
		return occurrenceInvalid("An override keeps its series' currency.")
	case m.Origin == OriginUpload && !eq(m.Reference, v.reference):
		return invalid("request.validation_failed", "An uploaded record's reference is how its batch knows it; it does not change.", "reference",
			"the reference of the upload")
	case m.Recurrence == nil || (m.ValueDate.Equal(v.valueDate) && m.Currency == v.currency && sameRule(*m.Recurrence, *v.recurrence)):
		return nil
	}
	var overridden bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM cash.memo_record WHERE recurrence_of = $1)`, m.ID).Scan(&overridden); err != nil {
		return fmt.Errorf("cash: overrides of series %s: %w", m.ID, err)
	}
	if overridden {
		return problem.Conflict("cash.series_has_overrides", "The series has overrides of its occurrences; its value date, rule and currency stay while they do.")
	}
	return nil
}

// sameRule reports whether two series' rules give the same occurrences.
func sameRule(a, b Recurrence) bool {
	return a.Frequency == b.Frequency && eq(a.Count, b.Count) && (a.Until == nil) == (b.Until == nil) && (a.Until == nil || a.Until.Equal(*b.Until))
}

// Closing is how a person closes an active memo record, or one occurrence
// of a series: cancelled, or realised by a statement line (Line) or by the
// person.
type Closing struct {
	Occurrence *time.Time
	Line       *LineRef
	Comment    *string
}

// CancelMemo cancels an active memo record, or one occurrence of a series
// (created true: the occurrence's new cancelled override is returned).
func (e *Endpoints) CancelMemo(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, c Closing) (MemoRecord, bool, error) {
	return e.close(ctx, id, ifMatch, c, MemoCancelled)
}

// RealiseMemo marks an active memo record realised, or one occurrence of a
// series (created true: the occurrence's new realised override is
// returned), by a statement line of its legal entity, currency, bank and
// direction, or by the person.
func (e *Endpoints) RealiseMemo(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, c Closing) (MemoRecord, bool, error) {
	return e.close(ctx, id, ifMatch, c, MemoRealised)
}

// close cancels or realises a record or an occurrence of a series.
func (e *Endpoints) close(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, c Closing, status string) (MemoRecord, bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MemoRecord{}, false, err
	}
	m, err := memo(ctx, tx, id, true)
	if err != nil {
		return MemoRecord{}, false, err
	}
	if err := ifMatch(m.Version); err != nil {
		return MemoRecord{}, false, err
	}
	if err := changeable(m, MemoActive); err != nil {
		return MemoRecord{}, false, err
	}
	if err := e.realisedBy(ctx, m, c.Line); err != nil {
		return MemoRecord{}, false, err
	}
	action := map[string]string{MemoCancelled: "cancel", MemoRealised: "realise"}[status]
	if m.Recurrence != nil && (c.Occurrence != nil || status == MemoRealised) {
		return closeOccurrence(ctx, tx, p, m, c, status, action)
	}
	if c.Occurrence != nil {
		return MemoRecord{}, false, invalid("request.validation_failed", "Only a series has occurrences.", "occurrence_date", "only on a series")
	}
	if err := decide(ctx, tx, p, id, status, c.Comment, c.Line); err != nil {
		return MemoRecord{}, false, err
	}
	out, err := written(ctx, tx, id, action, status)
	return out, false, err
}

// realisedBy checks the statement line that realises the record, if any:
// a line of its legal entity and currency, of its house bank when it names
// one, in its direction.
func (e *Endpoints) realisedBy(ctx context.Context, m MemoRecord, ref *LineRef) error {
	if ref == nil {
		return nil
	}
	_, err := e.line(ctx, *ref, "statement_id", func(l StatementLine) bool {
		return l.LegalEntityID == m.LegalEntityID && l.Currency == m.Currency && (m.HouseBankID == nil || *m.HouseBankID == l.HouseBankID) &&
			(l.Amount > 0) == (m.Amount > 0)
	})
	return err
}

// closeOccurrence cancels or realises one occurrence of a series by a new
// override of it, the person's, closed at once.
func closeOccurrence(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m MemoRecord, c Closing, status, action string) (MemoRecord, bool, error) {
	if c.Occurrence == nil {
		return MemoRecord{}, false, invalid("request.validation_failed", "A series is realised one occurrence at a time.", "occurrence_date", "required for a series")
	}
	day := clock.Day(*c.Occurrence)
	if !m.Recurrence.has(m.ValueDate, day) {
		return MemoRecord{}, false, occurrenceInvalid("The series has no occurrence on " + day.Format(time.DateOnly) + ".")
	}
	expires := day.AddDate(0, 0, m.expiryDays())
	v := memoValues{houseBank: m.HouseBankID, currency: m.Currency, amount: m.Amount, valueDate: day, expiresOn: expires, countsUntil: &expires,
		certainty: m.Certainty, item: m.Item, description: m.Description, reference: m.Reference, recurrenceOf: &m.ID, occurrenceDate: &day}
	id, err := insertMemo(ctx, tx, p, m.LegalEntityID, v, memoMeta{status: status, origin: OriginManual, realisedBy: c.Line, decided: true, comment: c.Comment})
	if err != nil {
		return MemoRecord{}, false, err
	}
	out, err := written(ctx, tx, id, action, status)
	return out, true, err
}

// AcceptMemo makes an agent's proposed record active, as the person's
// decision; its item and bank are checked again.
func (e *Endpoints) AcceptMemo(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment *string) (MemoRecord, error) {
	return e.decideProposal(ctx, id, ifMatch, MemoActive, comment)
}

// RejectMemo turns down an agent's proposed record, with the person's
// reason: it never counts.
func (e *Endpoints) RejectMemo(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (MemoRecord, error) {
	return e.decideProposal(ctx, id, ifMatch, MemoRejected, &reason)
}

func (e *Endpoints) decideProposal(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, status string, comment *string) (MemoRecord, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MemoRecord{}, err
	}
	m, err := memo(ctx, tx, id, true)
	if err != nil {
		return MemoRecord{}, err
	}
	if err := ifMatch(m.Version); err != nil {
		return MemoRecord{}, err
	}
	if m.Status != MemoProposed {
		return MemoRecord{}, problem.Conflict("cash.memo_record_not_proposed", "The memo record is "+m.Status+", not an agent's proposal to decide.")
	}
	if status == MemoActive {
		c, err := e.checker(ctx, tx, m.LegalEntityID)
		if err != nil {
			return MemoRecord{}, err
		}
		if _, err := c.values(ctx, m.input(MemoPatch{})); err != nil {
			return MemoRecord{}, err
		}
	}
	if err := decide(ctx, tx, p, id, status, comment, nil); err != nil {
		return MemoRecord{}, err
	}
	action := map[string]string{MemoActive: "accept", MemoRejected: "reject"}[status]
	return written(ctx, tx, id, action, action+"ed")
}
