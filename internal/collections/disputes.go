package collections

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a dispute case (ADR-0065 As built, T86a): Open, being
// worked; Accepted and Rejected, closed with that outcome.
const (
	Open     = "open"
	Accepted = "accepted"
	Rejected = "rejected"
)

func init() {
	// A second open case of an item, whether it comes after the first or
	// races it (migration 000324).
	problem.RegisterConstraint("dispute_case_one_open", errDisputeExists)
}

// Case is a row of collections.dispute_case with its items: a customer
// disputes Amount, minor units of Currency (its items', Exponent digits),
// of one or more of its open items (T86c), each with its own disputed
// amount. AmountLC is its items' share of their amounts in CurrencyLC,
// the company's (ExponentLC digits), as they were booked. The case's own
// journal line, DocumentNumber and Reference are its first item's.
// CustomerNumber is the customer's when the case was opened. A closed case
// has its outcome as Status, with CloseReason, ClosedAt and ClosedBy.
type Case struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	ledger.ItemRef
	DocumentNumber string
	Reference      *string
	Currency       string
	Exponent       int
	Amount         money.Minor
	CurrencyLC     string
	ExponentLC     int
	AmountLC       money.Minor
	Reason         string
	Assignee       *uuid.UUID
	Note           *string
	Status         string
	CloseReason    *string
	ClosedAt       *time.Time
	ClosedBy       *uuid.UUID
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
	// Resolution is how an accepted case was resolved (T86d), nil for
	// any other case; Items are the case's items by number, from 1.
	Resolution *Resolution
	Items      []CaseItem
}

const caseSelect = `SELECT d.id, d.legal_entity_id, d.customer_id, d.customer_number, d.fiscal_year, d.journal_entry_id, d.line_no, d.document_number,
		d.reference, d.currency, c.exponent, d.disputed_amount, d.currency_lc, l.exponent, d.disputed_amount_lc, d.reason, d.assignee_id, d.note, d.status,
		d.close_reason, d.closed_at, d.closed_by, d.version, d.created_at, d.created_by, d.updated_at, d.updated_by,
		d.resolution, d.accepted_amount, d.accepted_amount_lc, d.posting_date, d.write_off_entry_year, d.write_off_entry_id, d.write_off_entry_number
	FROM collections.dispute_case d JOIN ref.currency c ON c.code = d.currency JOIN ref.currency l ON l.code = d.currency_lc`

func scanCase(row pgx.Row) (Case, error) {
	var c Case
	var r resolutionRow
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.CustomerID, &c.CustomerNumber, &c.FiscalYear, &c.EntryID, &c.LineNo, &c.DocumentNumber,
		&c.Reference, &c.Currency, &c.Exponent, &c.Amount, &c.CurrencyLC, &c.ExponentLC, &c.AmountLC, &c.Reason, &c.Assignee, &c.Note, &c.Status,
		&c.CloseReason, &c.ClosedAt, &c.ClosedBy, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy,
		&r.kind, &r.accepted, &r.acceptedLC, &r.postingDate, &r.entryYear, &r.entryID, &r.entryNumber)
	c.Resolution = r.resolution()
	return c, errs.Wrapf(err, "scan dispute case")
}

// readCase reads a dispute case of a legal entity the caller may act in,
// under its row lock when forUpdate (ADR-0008); another company's case is
// not found, so ids cannot be probed (04 §4.9).
func readCase(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Case, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Case{}, fmt.Errorf("collections: company access for dispute case %s: %w", id, err)
	}
	q := caseSelect + ` WHERE d.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF d`
	}
	c, err := scanCase(tx.QueryRow(ctx, q, id))
	if err := authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("dispute case"), "collections: read dispute case %s", id); err != nil {
		return Case{}, err
	}
	cases := []Case{c}
	if err := caseItems(ctx, tx, cases); err != nil {
		return Case{}, err
	}
	return cases[0], nil
}

// GetCase returns a dispute case of a legal entity the caller may act in.
func GetCase(ctx context.Context, id uuid.UUID) (Case, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Case{}, err
	}
	return readCase(ctx, tx, id, false)
}

// CaseInput is a dispute case as a client opens it: the company, its
// items (one or more open items of one customer, T86c), the reason, and an
// assignee and a note if wanted.
type CaseInput struct {
	LegalEntityID uuid.UUID
	Items         []CaseItemInput
	Reason        string
	Assignee      *uuid.UUID
	Note          *string
}

// OpenCase opens a dispute case on open customer items of a company the
// caller may act in (ADR-0065 As built, T86a, T86c): each item is one the
// case's customer owes, read from the ledger's open items now, disputed
// in part or in full (disputedItems); an assignee is an active person who
// may act in the company. An item in another open case is refused by the
// database, 409 collections.dispute_exists. The audit record and
// DisputeCaseChanged are written in the request's transaction; nothing is
// posted and the items do not change.
func (e *Endpoints) OpenCase(ctx context.Context, in CaseInput) (Case, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Case{}, err
	}
	if err := texts(map[string]*string{"reason": &in.Reason, "note": in.Note}); err != nil {
		return Case{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Case{}, fmt.Errorf("collections: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Case{}, fmt.Errorf("collections: legal entity of a dispute case: %w", err)
	}
	items, err := e.disputedItems(ctx, le, in.Items)
	if err != nil {
		return Case{}, err
	}
	if in.Assignee != nil {
		if err := assignable(ctx, *in.Assignee, le.ID); err != nil {
			return Case{}, err
		}
	}
	number, err := e.customerNumber(ctx, items[0].PartnerID)
	if err != nil {
		return Case{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := insertCase(ctx, tx, p, id, le, in, number, items); err != nil {
		return Case{}, err
	}
	c, err := readCase(ctx, tx, id, false)
	if err != nil {
		return Case{}, err
	}
	return c, recordCase(ctx, "collections.dispute_case.open", "opened", c, le, nil, map[string]any{"reason_given": true, "note_given": c.Note != nil})
}

// openItem is the company's open customer item at the journal line, one
// the customer owes (a debit); any other line is refused, naming the
// journal entry's field.
func (e *Endpoints) openItem(ctx context.Context, le tenancy.Entity, ref ledger.ItemRef, field string) (ledger.ReceivableItem, error) {
	it, ok, err := e.OpenItems.ReceivableItem(ctx, le.ID, ref)
	if err != nil {
		return ledger.ReceivableItem{}, fmt.Errorf("collections: open customer item of company %s: %w", le.Code, err)
	}
	if ok && it.Amount > 0 {
		return it, nil
	}
	return ledger.ReceivableItem{}, problem.Invalid("collections.item_not_open", field, fmt.Sprintf("Line %d of the journal entry "+
		"of fiscal year %d is no open item a customer owes company %s: a dispute case, an item's dunning block and a promise to pay name an uncleared "+
		"debit on a customer reconciliation account in the leading ledger.", ref.LineNo, ref.FiscalYear, le.Code))
}

// share is the disputed amount's share of the item's amount in the
// company's currency, as the item was booked: its local amount × the
// disputed amount ÷ its amount, rounded half away from zero. All of the
// item disputed is all of its local amount.
func share(it ledger.ReceivableItem, amount money.Minor) money.Minor {
	return shareOf(it.AmountLC, amount, it.Amount)
}

// shareOf is part of whole's share of lc: lc × part ÷ whole, rounded half
// away from zero; part is from zero to whole, which is above zero.
func shareOf(lc, part, whole money.Minor) money.Minor {
	out, _ := money.Round(ratio(lc, part, whole), 0) // never out of range: at most lc, as part is at most whole
	return out
}

// ratio is a × b ÷ c exactly; c is above zero.
func ratio(a, b, c money.Minor) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(b))), big.NewInt(int64(c)))
}

// customerNumber is the business partner number of the item's customer.
func (e *Endpoints) customerNumber(ctx context.Context, customer uuid.UUID) (string, error) {
	names, err := e.Partners.Names(ctx, []uuid.UUID{customer})
	if err != nil {
		return "", fmt.Errorf("collections: customer %s of a dispute case: %w", customer, err)
	}
	named, ok := names[customer]
	if !ok { // the ledger posts customer items to business partners only, and none is deleted
		return "", fmt.Errorf("collections: customer %s of an open item is no business partner", customer)
	}
	return named.Number, nil
}

// assignable refuses an assignee that is not an active person of the
// tenant who may act in the case's company: a case is people's work, and
// a person without the company's access could not see it.
func assignable(ctx context.Context, id, legalEntity uuid.UUID) error {
	switch a, err := authz.AssignableTo(ctx, id, legalEntity); {
	case err != nil:
		return fmt.Errorf("collections: assignee %s: %w", id, err)
	case a == authz.NotActivePerson:
		return assigneeInvalid("an active person of the tenant")
	case a == authz.NoCompanyAccess:
		return assigneeInvalid("a person with access to the case's company")
	}
	return nil
}

// assigneeInvalid refuses an assignee, the field's message naming what it
// must be (want), so a consumer tells which rule the id broke.
func assigneeInvalid(want string) error {
	return problem.Unprocessable("collections.assignee_invalid", "A dispute case is assigned to an active person of the tenant who may act in "+
		"the case's company, not to an agent, a service, a blocked or an unknown principal, or a person without the company's access.",
		problem.FieldError{Field: "assignee_id", Code: "invalid", Message: want})
}

// texts refuses a text given as white space alone, naming its field; a
// field not given (nil) is not checked.
func texts(fields map[string]*string) error {
	for _, f := range []string{"reason", "note"} {
		if v, ok := fields[f]; ok && v != nil {
			if err := problem.RequireText(f, *v); err != nil {
				return err
			}
		}
	}
	return nil
}

// CasePatch is a merge patch of an open case: each field, when Set, is
// changed to its value, nil clearing it.
type CasePatch struct {
	AssigneeSet bool
	Assignee    *uuid.UUID
	NoteSet     bool
	Note        *string
}

// errClosed refuses a change of a case that is not open.
func errClosed() *problem.Problem {
	return problem.Conflict("collections.dispute_closed", "The dispute case is closed; a closed case is not changed.")
}

// UpdateCase changes an open case's assignee or note as the patch asks,
// with the caller's version of it (ADR-0008): the case is read under its
// row lock, its version compared, and a closed case refused, 409
// collections.dispute_closed. A patch that changes nothing is refused,
// 422 collections.nothing_to_change.
func (e *Endpoints) UpdateCase(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, patch CasePatch) (Case, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Case{}, err
	}
	if err := texts(map[string]*string{"note": patch.Note}); err != nil {
		return Case{}, err
	}
	c, err := lockOpen(ctx, tx, id, ifMatch)
	if err != nil {
		return Case{}, err
	}
	assignee, note := c.Assignee, c.Note
	if patch.AssigneeSet {
		assignee = patch.Assignee
	}
	if patch.NoteSet {
		note = patch.Note
	}
	if apitypes.SamePtr(assignee, c.Assignee) && apitypes.SamePtr(note, c.Note) {
		return Case{}, problem.Unprocessable("collections.nothing_to_change", "The change would leave the dispute case as it is.")
	}
	if assignee != nil && !apitypes.SamePtr(assignee, c.Assignee) {
		if err := assignable(ctx, *assignee, c.LegalEntityID); err != nil {
			return Case{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE collections.dispute_case SET assignee_id = $2, note = $3, version = version + 1, updated_by = $4 WHERE id = $1`,
		id, assignee, note, p.ID); err != nil {
		return Case{}, fmt.Errorf("collections: change dispute case %s: %w", id, err)
	}
	return e.changed(ctx, tx, id, "collections.dispute_case.update", "updated", map[string]any{"assignee_id": assignee, "note_given": note != nil})
}

// CloseCase closes an open case once with its outcome, Accepted or
// Rejected, and the reason, with the caller's version of it: the case is
// read under its row lock and moved by an update guarded by its status
// (ADR-0008), so a second close is refused, 409 collections.dispute_closed.
// Another outcome is refused, 422 collections.unknown_outcome. Nothing is
// posted.
func (e *Endpoints) CloseCase(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, outcome, reason string) (Case, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Case{}, err
	}
	if outcome != Accepted && outcome != Rejected {
		return Case{}, problem.Invalid("collections.unknown_outcome", "outcome", fmt.Sprintf("%q is no outcome of a dispute case; the outcomes are %s and %s.",
			outcome, Accepted, Rejected))
	}
	if err := texts(map[string]*string{"reason": &reason}); err != nil {
		return Case{}, err
	}
	if _, err := lockOpen(ctx, tx, id, ifMatch); err != nil {
		return Case{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE collections.dispute_case SET status = $2, close_reason = $3, closed_at = now(), closed_by = $4,
			version = version + 1, updated_by = $4
		WHERE id = $1 AND status = 'open'`, id, outcome, reason, p.ID); err != nil {
		return Case{}, fmt.Errorf("collections: close dispute case %s: %w", id, err)
	}
	return e.changed(ctx, tx, id, "collections.dispute_case.close", "closed", map[string]any{"outcome": outcome, "reason_given": true})
}

// lockOpen reads a case under its row lock, compares the caller's
// version with its own and refuses one that is not open.
func lockOpen(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Case, error) {
	c, err := readCase(ctx, tx, id, true)
	if err != nil {
		return Case{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Case{}, err
	}
	if c.Status != Open {
		return Case{}, errClosed()
	}
	return c, nil
}

// changed reads a case after a change of an open case and records it.
func (e *Endpoints) changed(ctx context.Context, tx pgx.Tx, id uuid.UUID, action, change string, detail map[string]any) (Case, error) {
	c, err := readCase(ctx, tx, id, false)
	if err != nil {
		return Case{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored case's legal entity exists
	if err != nil {
		return Case{}, fmt.Errorf("collections: company of dispute case %s: %w", id, err)
	}
	before := Open
	return c, recordCase(ctx, action, change, c, le, &before, detail)
}

// recordCase adds the audit record of a change of a case (detail: what
// the change set) and publishes DisputeCaseChanged in the same
// transaction (ADR-0007, ADR-0011). before is the case's status before,
// nil for a case just opened. The event names the customer by id and
// number, never by name (product owner, 2026-11-05), and carries no free
// text: the reason and the note may name people. Nor do the audit details,
// which a deletion run cannot blank (ADR-0079): they record that a reason
// or a note was given, not what it says.
func recordCase(ctx context.Context, action, change string, c Case, le tenancy.Entity, before *string, detail map[string]any) error {
	var was any
	if before != nil {
		was = *before
	}
	items, resolution := itemsDetail(c), resolutionDetail(c)
	payload := map[string]any{"dispute_case_id": c.ID, "change": change, "legal_entity_id": c.LegalEntityID, "legal_entity_code": le.Code,
		"customer_id": c.CustomerID, "customer_number": c.CustomerNumber, "fiscal_year": c.FiscalYear, "journal_entry_id": c.EntryID,
		"line_no": c.LineNo, "document_number": c.DocumentNumber, "disputed_amount": money.EventAmount(c.Amount, c.Currency, c.Exponent),
		"assignee_id": c.Assignee, "status": c.Status, "status_before": was, "version": c.Version, "items": items, "resolution": resolution}
	detail["status"], detail["status_before"], detail["items"], detail["resolution"] = c.Status, was, items, resolution
	if err := audit.Record(ctx, action, "collections.dispute_case", c.ID, detail, outbox.Event{Context: "collections", Name: "dispute_case_changed",
		AggregateType: "collections.dispute_case", AggregateID: c.ID, Payload: payload}); err != nil {
		return fmt.Errorf("collections: record %s of dispute case %s: %w", action, c.ID, err)
	}
	return nil
}
