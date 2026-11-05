package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// postParked posts a parked entry, or asks for its approval (T35e): the
// approval policy measures its debits against its company's
// manual_entry_approval_limit (approvals.ManualEntry). Within the limit no
// request is opened and the entry is posted at once; above it the entry
// waits for the request, which the caller raises and so can never approve,
// and is posted by its approval (ManualEntries). The caller must own the
// entry; a person posting an agent's proposal takes it on.
func (e *Endpoints) postParked(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (ParkedEntry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	pe, owner, err := editable(ctx, tx, p, id, ifMatch)
	if err != nil {
		return ParkedEntry{}, err
	}
	in := sourced(pe)
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return ParkedEntry{}, err
	}
	if err := checkSpecial(ctx, tx, v, in); err != nil {
		return ParkedEntry{}, err
	}
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.ManualEntry, ObjectID: id, Title: title(pe),
		Amount: &pe.Debits, Currency: &pe.Currency, LegalEntityID: v.le.ID, Company: v.le.Code, Date: pe.PostingDate})
	if err != nil {
		return ParkedEntry{}, err
	}
	if needed {
		return submitParked(ctx, tx, p, pe, owner, request)
	}
	je, err := e.postPrepared(ctx, tx, p, v, in, nil)
	if err != nil {
		return ParkedEntry{}, err
	}
	return markPosted(ctx, tx, p, pe, owner, je)
}

// sourced is the parked entry as the manual entry posted for it, naming it
// as its source document.
func sourced(pe ParkedEntry) NewEntry {
	in := changed(pe, EntryChange{})
	in.SourceContext, in.SourceDocumentID = "ledger", &pe.ID
	return in
}

// title is what approvers see of an entry: its type, date and amount.
func title(pe ParkedEntry) string {
	t := fmt.Sprintf("Manual entry %s of %s, %s %s", pe.DocumentType, pe.PostingDate.Format(time.DateOnly), pe.Debits.Plain(pe.Exp), pe.Currency)
	if pe.HeaderText != nil {
		t += ": " + *pe.HeaderText
	}
	return t
}

// submitParked has the entry wait for its approval request.
func submitParked(ctx context.Context, tx pgx.Tx, p reqctx.Principal, pe ParkedEntry, owner *uuid.UUID, request uuid.UUID) (ParkedEntry, error) {
	if _, err := tx.Exec(ctx, `UPDATE ledger.parked_entry SET status = 'pending_approval', approval_request_id = $2, owner_id = $3,
			version = version + 1, updated_by = $4 WHERE id = $1`, pe.ID, request, owner, p.ID); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: submit parked entry %s: %w", pe.ID, err)
	}
	return changedTo(ctx, tx, pe, "submit", map[string]any{"approval_request_id": request})
}

// markPosted records the journal entry a parked entry was posted as.
func markPosted(ctx context.Context, tx pgx.Tx, p reqctx.Principal, pe ParkedEntry, owner *uuid.UUID, je Entry) (ParkedEntry, error) {
	if _, err := tx.Exec(ctx, `UPDATE ledger.parked_entry SET status = 'posted', owner_id = $2, journal_fiscal_year = $3, journal_entry_id = $4,
			version = version + 1, updated_by = $5 WHERE id = $1`, pe.ID, owner, je.FiscalYear, je.ID, p.ID); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: mark parked entry %s posted: %w", pe.ID, err)
	}
	return changedTo(ctx, tx, pe, "post", map[string]any{"journal_entry_id": je.ID, "document_number": je.DocumentNumber,
		"approval_request_id": pe.ApprovalRequestID})
}

// changedTo reads back an entry whose status changed from pe's and records
// the change. It reads without the caller's company filter: the caller
// already acted on the entry, or decided its request.
func changedTo(ctx context.Context, tx pgx.Tx, pe ParkedEntry, action string, extra map[string]any) (ParkedEntry, error) {
	updated, err := scanParked(tx.QueryRow(ctx, parkedSelect+` WHERE p.id = $1`, pe.ID))
	if err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: read parked entry %s: %w", pe.ID, err)
	}
	return updated, parkedChanged(ctx, updated, &pe.Status, action, extra)
}

// ManualEntries moves a parked entry on when its manual_entry approval
// request completes (approvals.Handler), in the deciding transaction. Each
// acts only while the entry waits for that request, so a request the entry
// no longer names changes nothing and fails the decision: a second
// callback for the same request posts nothing.
type ManualEntries struct {
	LegalEntities LegalEntities
	Partners      Partners
	Numbers       Numbers
	TaxReturns    TaxReturns // holds the entry's tax date to open returns (P6)
}

func (m ManualEntries) endpoints() *Endpoints {
	return &Endpoints{LegalEntities: m.LegalEntities, Partners: m.Partners, Numbers: m.Numbers, TaxReturns: m.TaxReturns}
}

// Approved posts the entry as approved, checked again: a period closed or
// an account blocked since it was submitted refuses the decision, and the
// request stays pending. The refusal's fields name the parked entry's, which the approver
// reads with the request. The approver need not act in the entry's legal
// entity: the request authorises the posting.
func (m ManualEntries) Approved(ctx context.Context, entry, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	pe, err := namingEntry(ctx, tx, entry, request)
	if err != nil {
		return err
	}
	e := m.endpoints()
	le, err := e.postableEntity(ctx, pe.LegalEntityID)
	if err != nil {
		return err
	}
	in := sourced(pe)
	v, err := e.prepareFor(ctx, tx, le, in)
	if err != nil {
		return err
	}
	if err := checkSpecial(ctx, tx, v, in); err != nil {
		return err
	}
	je, err := e.postPrepared(ctx, tx, p, v, in, nil)
	if err != nil {
		return err
	}
	_, err = markPosted(ctx, tx, p, pe, pe.OwnerID, je)
	return err
}

// Rejected rejects the entry for good; nothing is posted.
func (ManualEntries) Rejected(ctx context.Context, entry, request uuid.UUID, comment string) error {
	return endRequest(ctx, entry, request, EntryRejected, "reject", comment)
}

// Cancelled returns the entry to parked: its requester withdrew the
// request, to change the entry or let it be.
func (ManualEntries) Cancelled(ctx context.Context, entry, request uuid.UUID, comment string) error {
	return endRequest(ctx, entry, request, EntryParked, "return", comment)
}

func endRequest(ctx context.Context, entry, request uuid.UUID, status, action, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	pe, err := namingEntry(ctx, tx, entry, request)
	if err != nil {
		return err
	}
	// A rejected entry keeps its request; a returned one is parked and names
	// none (parked_entry_request_when_decided).
	if _, err := tx.Exec(ctx, `UPDATE ledger.parked_entry SET status = $3,
			approval_request_id = CASE WHEN $3 = 'parked' THEN NULL ELSE approval_request_id END, version = version + 1, updated_by = $4
		WHERE id = $1 AND approval_request_id = $2`, entry, request, status, p.ID); err != nil {
		return fmt.Errorf("ledger: %s parked entry %s on request %s: %w", action, entry, request, err)
	}
	_, err = changedTo(ctx, tx, pe, action, map[string]any{"approval_request_id": request, "comment": comment})
	return err
}

// namingEntry locks the entry and returns it if it waits for the request.
// The decider may not see the entry's company: the request is what
// authorises the change, so the entry is read without the company filter.
func namingEntry(ctx context.Context, tx pgx.Tx, entry, request uuid.UUID) (ParkedEntry, error) {
	pe, err := scanParked(tx.QueryRow(ctx, parkedSelect+` WHERE p.id = $1 FOR UPDATE OF p`, entry))
	if err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: read parked entry %s of request %s: %w", entry, request, err)
	}
	if pe.Status != EntryPendingApproval || pe.ApprovalRequestID == nil || *pe.ApprovalRequestID != request {
		return ParkedEntry{}, fmt.Errorf("ledger: parked entry %s (%s) does not wait for request %s", entry, pe.Status, request)
	}
	return pe, nil
}

// postableEntity is a legal entity postings may be made in, whoever the
// caller: an approval authorises the posting, not the approver's companies.
func (e *Endpoints) postableEntity(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	return e.activeEntity(ctx, authz.Access{All: true}, id, "legal_entity_id")
}

// withinLimit refuses a manual entry posted directly whose debits exceed
// its company's manual_entry_approval_limit: it is parked and posted with
// :post, which asks for someone else's approval. The policy is the one
// :post asks (approvals.ManualEntry), so the two never disagree.
func (e *Endpoints) withinLimit(ctx context.Context, tx pgx.Tx, v prepared, in NewEntry) error {
	// A period that is not open refuses the posting first: an approval could
	// not post it either. post reads the period again, FOR SHARE as here.
	if _, _, _, err := postingPeriod(ctx, tx, v.le, v.choice.ledgers, in.PostingDate, in.specialNumber()); err != nil {
		return err
	}
	debits, err := debitTotal(v.lines)
	if err != nil {
		return err
	}
	required, err := e.Approvals.Required(ctx, approvals.NewRequest{ObjectType: approvals.ManualEntry, Amount: &debits, Currency: &in.Currency,
		LegalEntityID: v.le.ID, Company: v.le.Code, Date: in.PostingDate})
	if err != nil {
		return err
	}
	if required {
		return problem.Conflict("ledger.approval_required", "The entry's debits exceed the manual entry approval limit of company "+v.le.Code+
			"; park it (POST /v1/parked-journal-entries) and post it with :post, which asks for approval.")
	}
	return nil
}

// Resources is where the API reads each parked entry (approvals.Locator).
func (ManualEntries) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/parked-journal-entries/%s", ids), nil
}
