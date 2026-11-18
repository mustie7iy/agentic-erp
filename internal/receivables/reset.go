package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Undoing an application (ADR-0063 §6 As built, T75i; SAP FBRA, reset
// cleared items and reverse, from memory): a payment's own application of
// its receipt, or an application of its credit on account, is undone by
// the ledger's reset of its clearing (ledger.Postings.ResetClearing),
// which opens the items and the receipt or the credit again and reverses
// the clearing entry; the reset is a row of
// receivables.incoming_payment_reset. Applications are undone last first:
// what an application left (a credit, a residual item) is what the next
// one consumed.

func init() {
	problem.RegisterConstraint("incoming_payment_reset_once", func() *problem.Problem {
		return problem.Conflict("receivables.already_reset", "The application was undone by another request in the meantime.")
	})
}

// PaymentReset is a row of receivables.incoming_payment_reset, as
// paymentSelect reads it: the application undone (0: the payment's own),
// why, the reversal of its clearing entry, when and by whom.
type PaymentReset struct {
	ApplicationNo int32     `json:"application_no"`
	Reason        string    `json:"reason"`
	ReversalYear  int16     `json:"reversal_entry_year"`
	ReversalID    uuid.UUID `json:"reversal_entry_id"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     uuid.UUID `json:"created_by"`
}

// The statuses of an incoming payment: its receipt applied, or that
// application undone (T75i).
const (
	PaymentStatusApplied = "applied"
	PaymentStatusReset   = "reset"
)

// resetOf is the reset of the application with the number (0: the
// payment's own), nil while it is in force.
func (v IncomingPayment) resetOf(no int32) *PaymentReset {
	for i := range v.Resets {
		if v.Resets[i].ApplicationNo == no {
			return &v.Resets[i]
		}
	}
	return nil
}

// Status is the payment's status: reset once its own application is
// undone.
func (v IncomingPayment) Status() string {
	if v.resetOf(0) != nil {
		return PaymentStatusReset
	}
	return PaymentStatusApplied
}

// ResetInput undoes an application of a payment, as a client asks: the
// payment's own (ApplicationNo 0) or an application of its credit on
// account, with the version the client read (IfMatch) and why.
type ResetInput struct {
	PaymentID     uuid.UUID
	ApplicationNo int32
	IfMatch       func(int32) error
	Reason        string
}

// Reset undoes the application through the ledger's reset of its
// clearing, and writes the undoing with an audit record and
// IncomingPaymentReset in the request's transaction. What may be undone
// is read here without a lock (undoable); the ledger checks it again
// under the entries' locks, which is what holds (ADR-0063 §6 As built,
// T75i).
func (e *Endpoints) Reset(ctx context.Context, in ResetInput) (IncomingPayment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return IncomingPayment{}, err
	}
	v, err := GetPayment(ctx, in.PaymentID)
	if err != nil {
		return IncomingPayment{}, err
	}
	if err := resetRequest(v, in); err != nil {
		return IncomingPayment{}, err
	}
	undone, err := v.undoable(in.ApplicationNo)
	if err != nil {
		return IncomingPayment{}, err
	}
	text := "Reset of incoming payment " + v.DocumentNumber
	if in.ApplicationNo != 0 {
		text += fmt.Sprintf(", application %d", in.ApplicationNo)
	}
	rev, err := e.Postings.ResetClearing(ctx, ledger.ClearingReset{Entry: undone.entry, HeaderText: &text, SourceContext: "receivables", SourceDocumentID: v.ID})
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: reset application %d of incoming payment %s: %w", in.ApplicationNo, v.DocumentNumber, err)
	}
	if err := insertReset(ctx, tx, p, v.ID, in, rev); err != nil {
		return IncomingPayment{}, err
	}
	after, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, v.ID))
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: read incoming payment %s: %w", v.ID, err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: company access for incoming payment %s: %w", v.ID, err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, v.LegalEntityID, "legal_entity_id") // GetPayment found it accessible
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: company of incoming payment %s: %w", v.ID, err)
	}
	return after, e.recordReset(ctx, after, le, in, undone, rev)
}

// resetRequest checks the request against the payment as read: the
// version the client read (412 request.version_mismatch) and a reason that
// is not white space alone.
func resetRequest(v IncomingPayment, in ResetInput) error {
	version, _, _ := v.version()
	if err := in.IfMatch(version); err != nil {
		return err
	}
	return problem.RequireText("reason", in.Reason)
}

// undoing is the application a reset undoes: its clearing entry and what
// it applied (the receipt, or the credit as it stood).
type undoing struct {
	entry  ledger.ItemRef
	amount money.Minor
}

// undoable is the application with the number (0: the payment's own) if
// it may be undone: one that is none of the payment's is not found (404);
// one undone already is refused, 409 receivables.already_reset; the
// payment's own while an application of its credit is in force, 409
// receivables.applications_in_force; an application while a later one is,
// 409 receivables.application_not_latest.
func (v IncomingPayment) undoable(no int32) (undoing, error) {
	found := undoing{entry: ledger.ItemRef{FiscalYear: v.EntryYear, EntryID: v.EntryID}, amount: v.Amount}
	ok := no == 0
	for _, a := range v.Applications {
		if a.No == no {
			found, ok = undoing{entry: ledger.ItemRef{FiscalYear: a.EntryYear, EntryID: a.EntryID}, amount: a.Amount}, true
		}
	}
	switch {
	case !ok:
		return undoing{}, problem.NotFoundOf("application of the incoming payment")
	case v.resetOf(no) != nil:
		return undoing{}, problem.Conflict("receivables.already_reset", "The application is undone already.")
	}
	for _, a := range v.Applications {
		switch {
		case a.No <= no || v.resetOf(a.No) != nil: // not after it, or undone
		case no == 0:
			return undoing{}, problem.Conflict("receivables.applications_in_force",
				fmt.Sprintf("Application %d of the payment's credit on account is in force: undo the applications, last first, before the payment.", a.No))
		default:
			return undoing{}, problem.Conflict("receivables.application_not_latest",
				fmt.Sprintf("Application %d, a later one, is in force: undo the applications last first.", a.No))
		}
	}
	return found, nil
}

// insertReset writes the reset with the reversal of the clearing entry.
func insertReset(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, in ResetInput, rev ledger.Entry) error {
	var application *int32
	if in.ApplicationNo != 0 {
		application = &in.ApplicationNo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment_reset (tenant_id, incoming_payment_id, application_no, reason, reversal_entry_year,
			reversal_entry_id, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.TenantID, id, application, in.Reason, rev.FiscalYear, rev.ID, p.ID); err != nil {
		return fmt.Errorf("receivables: insert the reset of application %d of incoming payment %s: %w", in.ApplicationNo, id, err)
	}
	return nil
}

// recordReset adds the audit record of the reset and publishes
// IncomingPaymentReset in the same transaction (ADR-0007, ADR-0011): the
// customer by id and number, never by name (product owner, 2026-11-05),
// for the payment's own each customer whose items open again with its
// part, and no reason, which is free text; the audit record keeps it.
func (e *Endpoints) recordReset(ctx context.Context, v IncomingPayment, le tenancy.Entity, in ResetInput, undone undoing, rev ledger.Entry) error {
	const action = "receivables.incoming_payment.reset"
	no := in.ApplicationNo
	items := 0
	for _, it := range v.Items {
		if it.ApplicationNo == no {
			items++
		}
	}
	parts := v.Parts()
	ids := make([]uuid.UUID, len(parts))
	for i, c := range parts {
		ids[i] = c.CustomerID
	}
	names, err := e.Partners.Names(ctx, ids) // the payer is the first part
	if err != nil {
		return fmt.Errorf("receivables: customers of incoming payment %s: %w", v.ID, err)
	}
	amount := money.EventAmount(undone.amount, v.Currency, v.Exponent)
	payload := map[string]any{"incoming_payment_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": names[v.CustomerID].Number, "application_no": no, "amount": amount,
		"item_count": items, "clearing_entry_id": undone.entry.EntryID, "reversal_entry_id": rev.ID, "reversal_entry_year": rev.FiscalYear,
		"reversal_entry_number": rev.DocumentNumber, "reversal_posting_date": rev.PostingDate.Format(time.DateOnly)}
	if no == 0 { // the payment's own application: whose items open again (T75h)
		payload["customers"] = v.partsPayload(parts, names)
	}
	detail := map[string]any{"document_number": v.DocumentNumber, "application_no": no, "reason": in.Reason, "amount": amount, "items": items,
		"clearing_entry_id": undone.entry.EntryID, "reversal_entry_id": rev.ID, "reversal_entry_number": rev.DocumentNumber}
	if err := audit.Record(ctx, action, "receivables.incoming_payment", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "incoming_payment_reset", AggregateType: "receivables.incoming_payment", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of incoming payment %s: %w", action, v.ID, err)
	}
	return nil
}
