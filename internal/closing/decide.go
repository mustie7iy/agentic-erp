package closing

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of a parked entry (the ledger's, T35) a proposal's status is
// derived from.
const (
	entryParked    = "parked"
	entryPending   = "pending_approval"
	entryRejected  = "rejected"
	entryCancelled = "cancelled"
)

// accrualStatuses is the order totals are shown in.
var accrualStatuses = []string{AccrualProposed, AccrualPending, AccrualPosted, AccrualReversed, AccrualRejected}

// derived is a proposal's status derived from its parked entry (ADR-0058
// §8): modified when it was posted with a debit total other than the one
// proposed (or in another currency), with the entry as read.
type derived struct {
	status   string
	modified bool
	entry    EntryStatus
}

// derive is a proposal's status: rejected here, or its entry cancelled or
// its approval rejected in the ledger; proposed while its entry is parked;
// pending approval; reversed once its posted entry is reversed; else
// posted, the one other status a parked entry has.
func derive(a accrual, s EntryStatus) derived {
	d := derived{entry: s}
	switch {
	case a.Status == AccrualRejected || s.Status == entryCancelled || s.Status == entryRejected:
		d.status = AccrualRejected
	case s.Status == entryParked:
		d.status = AccrualProposed
	case s.Status == entryPending:
		d.status = AccrualPending
	default:
		d.status = AccrualPosted
		if s.ReversalID != nil {
			d.status = AccrualReversed
		}
		d.modified = s.Debits != a.Amount || s.Currency != a.Currency
	}
	return d
}

// posted reports whether the proposal's entry was posted (and maybe
// reversed since).
func (d derived) posted() bool { return d.status == AccrualPosted || d.status == AccrualReversed }

// statusesOf derives the proposals' statuses, by proposal, reading their
// parked entries in one call; an entry the ledger does not hold for the
// caller is broken data.
func (e *Endpoints) statusesOf(ctx context.Context, as []accrual) (map[uuid.UUID]derived, error) {
	if len(as) == 0 {
		return map[uuid.UUID]derived{}, nil
	}
	ids := make([]uuid.UUID, len(as))
	for i, a := range as {
		ids[i] = a.ParkedEntryID
	}
	entries, err := e.Ledger.Statuses(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("closing: parked entries of %d accrual proposals: %w", len(as), err)
	}
	out := make(map[uuid.UUID]derived, len(as))
	for _, a := range as {
		s, ok := entries[a.ParkedEntryID]
		if !ok {
			return nil, fmt.Errorf("closing: parked entry %s of accrual proposal %s is not held by the ledger context", a.ParkedEntryID, a.ID)
		}
		out[a.ID] = derive(a, s)
	}
	return out, nil
}

// RejectAccrualProposal implements POST /v1/accrual-proposals/{id}:rejectProposal
// (ADR-0058 §8): a person rejects a proposal whose entry is still parked,
// for a reason; the entry is cancelled through the ledger under its owner
// rule, and the proposal is rejected, audited and published.
func (e *Endpoints) RejectAccrualProposal(ctx context.Context, req *gen.ReasonInput, params gen.RejectAccrualProposalParams) (gen.RejectAccrualProposalRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if err := problem.RequireText("reason", req.Reason); err != nil {
		return nil, err
	}
	a, unlocked, err := e.openAccrual(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	if err := reopen(ctx, tx, periodCloses, p, a.CloseID, unlocked); err != nil {
		return nil, err
	}
	if err := e.Ledger.Cancel(ctx, a.ParkedEntryID, req.Reason); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE closing.accrual_proposal SET status = 'rejected', decided_by = $2, decided_at = now(), decision_reason = $3,
			version = version + 1, updated_by = $2 WHERE id = $1 AND status = 'proposed'`, a.ID, p.ID, req.Reason); err != nil {
		return nil, fmt.Errorf("closing: reject accrual proposal %s: %w", a.ID, err)
	}
	if a, err = readAccrual(ctx, tx, a.ID); err != nil {
		return nil, err
	}
	if err := recordRejection(ctx, tx, a); err != nil {
		return nil, err
	}
	out, err := e.accrualOut(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	return &gen.RejectAccrualProposalOKHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Response: out}, nil
}

// openAccrual reads a proposal under its close's lock (ADR-0058 §5),
// checks If-Match against its version and that its close takes writes
// (§7; the ledgers to reopen it for, if any), and refuses it unless its
// entry is still parked: checked here first, so the ledger's own guard
// (ledger.invalid_parked_status) settles only a race.
func (e *Endpoints) openAccrual(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch string) (accrual, []string, error) {
	a, err := readAccrual(ctx, tx, id)
	if err != nil {
		return accrual{}, nil, err
	}
	if _, err := lockClose(ctx, tx, periodCloses, a.CloseID); err != nil {
		return accrual{}, nil, err
	}
	if a, err = readAccrual(ctx, tx, id); err != nil {
		return accrual{}, nil, err
	}
	if err := etag.IfMatch(ifMatch)(a.Version); err != nil {
		return accrual{}, nil, err
	}
	unlocked, err := e.writable(ctx, tx, periodCloses, a.CloseID)
	if err != nil {
		return accrual{}, nil, err
	}
	statuses, err := e.statusesOf(ctx, []accrual{a})
	if err != nil {
		return accrual{}, nil, err
	}
	if s := statuses[a.ID].status; s != AccrualProposed {
		return accrual{}, nil, problem.Conflict("closing.accrual_not_open", "Accrual proposal "+a.ID.String()+" is "+s+
			"; only a proposal whose entry is still parked is rejected (a posted one is reversed in the ledger).")
	}
	return a, unlocked, nil
}

// recordRejection audits a rejected proposal and publishes it as
// AccrualProposalRejected.
func recordRejection(ctx context.Context, tx pgx.Tx, a accrual) error {
	exp, err := currency.Exponent(ctx, tx, a.Currency, nil)
	if err != nil {
		return fmt.Errorf("closing: currency of accrual proposal %s: %w", a.ID, err)
	}
	amount := money.EventAmount(a.Amount, a.Currency, exp)
	return audit.Record(ctx, "closing.accrual_proposal.reject", "closing.accrual_proposal", a.ID,
		map[string]any{"period_close_id": a.CloseID, "legal_entity_id": a.LegalEntityID, "parked_entry_id": a.ParkedEntryID, "amount": amount,
			"reason": a.DecisionReason, "status": a.Status, "version": a.Version},
		outbox.Event{Context: "closing", Name: "accrual_proposal_rejected", AggregateType: "closing.accrual_proposal", AggregateID: a.ID,
			Payload: map[string]any{"accrual_proposal_id": a.ID, "period_close_id": a.CloseID, "legal_entity_id": a.LegalEntityID, "fiscal_year": a.FiscalYear,
				"fiscal_period": a.FiscalPeriod, "account_id": a.AccountID, "supplier_id": a.SupplierID, "amount": amount, "parked_entry_id": a.ParkedEntryID,
				"decided_by": a.DecidedBy, "reason": a.DecisionReason, "status": a.Status, "version": a.Version}})
}

// foundAccrual is a proposal an accruals_decided or accruals_reversed
// check found, with its amount (posted, once posted) as a decimal in its
// currency's digits.
type foundAccrual struct {
	ID            uuid.UUID  `json:"id"`
	AccountID     uuid.UUID  `json:"account_id"`
	AccountNumber string     `json:"account_number"`
	AccountName   string     `json:"account_name"`
	Amount        string     `json:"amount"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status"`
	ParkedEntryID uuid.UUID  `json:"parked_entry_id"`
	JournalEntry  *uuid.UUID `json:"journal_entry_id,omitempty"`
	EntryNumber   *string    `json:"journal_entry_number,omitempty"`
}

// toAPI shows a proposal a check found.
func (f foundAccrual) toAPI() gen.CloseCheckAccrual {
	out := gen.CloseCheckAccrual{ID: f.ID, AccountID: f.AccountID, AccountNumber: f.AccountNumber, AccountName: f.AccountName,
		Amount: gen.Money{Amount: f.Amount, Currency: f.Currency}, Status: f.Status, ParkedEntryID: f.ParkedEntryID}
	if f.JournalEntry != nil {
		out.JournalEntryID.SetTo(*f.JournalEntry)
	}
	if f.EntryNumber != nil {
		out.JournalEntryNumber.SetTo(*f.EntryNumber)
	}
	return out
}

// accrualsOf reads a close's proposals, oldest first, with their statuses.
func (e *Endpoints) accrualsOf(ctx context.Context, tx pgx.Tx, closeID uuid.UUID) ([]accrual, map[uuid.UUID]derived, error) {
	rows, _ := tx.Query(ctx, accrualSelect+` WHERE a.close_id = $1 ORDER BY a.created_at, a.id`, closeID)
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accrual, error) { return scanAccrual(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("closing: accrual proposals of close %s: %w", closeID, err)
	}
	statuses, err := e.statusesOf(ctx, as)
	return as, statuses, err
}

// findAccruals records in f the proposals among as whose status is one of
// want, naming the first maxNamed.
func findAccruals(ctx context.Context, tx pgx.Tx, f *found, as []accrual, statuses map[uuid.UUID]derived, want ...string) error {
	for _, a := range as {
		d := statuses[a.ID]
		if !slices.Contains(want, d.status) {
			continue
		}
		if f.AccrualsCount++; len(f.Accruals) < maxNamed {
			amount, cur := a.Amount, a.Currency
			if d.posted() {
				amount, cur = d.entry.Debits, d.entry.Currency
			}
			exp, err := currency.Exponent(ctx, tx, cur, nil)
			if err != nil {
				return fmt.Errorf("closing: currency of accrual proposal %s: %w", a.ID, err)
			}
			f.Accruals = append(f.Accruals, foundAccrual{ID: a.ID, AccountID: a.AccountID, AccountNumber: a.AccountNumber, AccountName: a.AccountName,
				Amount: amount.Plain(exp), Currency: cur, Status: d.status, ParkedEntryID: a.ParkedEntryID, JournalEntry: d.entry.JournalEntryID,
				EntryNumber: d.entry.DocumentNumber})
		}
	}
	return nil
}

// accrualsDecided passes when no accrual proposal of the close is still
// proposed or pending approval (ADR-0058 §6), and names the first of those
// that are.
func (e *Endpoints) accrualsDecided(ctx context.Context, tx pgx.Tx, t task) (bool, found, error) {
	as, statuses, err := e.accrualsOf(ctx, tx, t.CloseID)
	if err != nil {
		return false, found{}, err
	}
	f := found{Accrual: true}
	if err := findAccruals(ctx, tx, &f, as, statuses, AccrualProposed, AccrualPending); err != nil {
		return false, found{}, err
	}
	switch f.AccrualsCount {
	case 0:
		f.Summary = "Every accrual proposal of the close is decided: posted or rejected."
	case 1:
		f.Summary = "1 accrual proposal of the close is proposed or pending approval; post or reject it."
	default:
		f.Summary = fmt.Sprintf("%d accrual proposals of the close are proposed or pending approval; post or reject them.", f.AccrualsCount)
	}
	return f.AccrualsCount == 0, f, nil
}

// accrualsReversed passes when every accrual posted for the company's
// close of the previous regular period is reversed (ADR-0058 §6, §8), or
// there is no such close, and names the first of those that are not.
func (e *Endpoints) accrualsReversed(ctx context.Context, tx pgx.Tx, t task) (bool, found, error) {
	year, period := t.FiscalYear, t.FiscalPeriod-1
	if period == 0 {
		year, period = year-1, maxRegular
	}
	var previous uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM closing.period_close WHERE legal_entity_id = $1 AND fiscal_year = $2 AND fiscal_period = $3`,
		t.LegalEntityID, year, period).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, found{Accrual: true, Previous: &[2]int16{year, period},
			Summary: fmt.Sprintf("The company has no close of %d/%d, so no accrual of it to reverse.", year, period)}, nil
	}
	if err != nil {
		return false, found{}, errs.Wrapf(err, "closing: close of %d/%d of %s", year, period, t.LegalEntityID)
	}
	as, statuses, err := e.accrualsOf(ctx, tx, previous)
	if err != nil {
		return false, found{}, err
	}
	f := found{Accrual: true, PreviousClose: &previous, Previous: &[2]int16{year, period}}
	if err := findAccruals(ctx, tx, &f, as, statuses, AccrualPosted); err != nil {
		return false, found{}, err
	}
	of := fmt.Sprintf("%d/%d", year, period)
	switch f.AccrualsCount {
	case 0:
		f.Summary = "Every accrual posted for the close of " + of + " is reversed."
	case 1:
		f.Summary = "1 accrual posted for the close of " + of + " is not reversed; reverse its journal entry in this period."
	default:
		f.Summary = fmt.Sprintf("%d accruals posted for the close of %s are not reversed; reverse their journal entries in this period.", f.AccrualsCount, of)
	}
	return f.AccrualsCount == 0, f, nil
}
