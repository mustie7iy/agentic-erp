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
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Payments on account (ADR-0063 §6 As built, T75g): what a receipt leaves
// after its items is a credit of the customer's on its reconciliation
// account (ApplyPayment with OnAccount); an application applies that
// credit to the customer's open items as a receipt is applied, leaving
// what they do not take on account again, a credit line of its own entry.

// PaymentApplication is a row of receivables.incoming_payment_application,
// as paymentSelect reads it: the credit line it consumed and what that
// stood at, the difference written off, what it left on account and its
// line in the clearing entry (nil for nothing), the clearing entry.
type PaymentApplication struct {
	No            int32       `json:"application_no"`
	PostingDate   string      `json:"posting_date"`
	CreditYear    int16       `json:"credit_entry_year"`
	CreditEntryID uuid.UUID   `json:"credit_entry_id"`
	CreditLine    int32       `json:"credit_line_no"`
	Amount        money.Minor `json:"amount"`
	Difference    money.Minor `json:"difference"`
	OnAccount     money.Minor `json:"on_account_amount"`
	OnAccountLine *int32      `json:"on_account_line_no"`
	EntryYear     int16       `json:"clearing_entry_year"`
	EntryID       uuid.UUID   `json:"clearing_entry_id"`
	CreatedAt     time.Time   `json:"created_at"`
	CreatedBy     uuid.UUID   `json:"created_by"`
}

// credit is the payment's credit on account now: the line the last
// application left, else the payment's own, and what it stands at; ok is
// false when nothing was left on account.
func (v IncomingPayment) credit() (ref ledger.ItemRef, amount money.Minor, ok bool) {
	if n := len(v.Applications); n > 0 {
		last := v.Applications[n-1]
		if last.OnAccountLine == nil {
			return ledger.ItemRef{}, 0, false
		}
		return ledger.ItemRef{FiscalYear: last.EntryYear, EntryID: last.EntryID, LineNo: *last.OnAccountLine}, last.OnAccount, true
	}
	if v.OnAccountLine == nil {
		return ledger.ItemRef{}, 0, false
	}
	return ledger.ItemRef{FiscalYear: v.EntryYear, EntryID: v.EntryID, LineNo: *v.OnAccountLine}, v.OnAccount, true
}

// version is the payment's version: 1 when it was applied, and one more
// for each application of its credit on account; updated is when and by
// whom it last changed.
func (v IncomingPayment) version() (version int32, at time.Time, by uuid.UUID) {
	if n := len(v.Applications); n > 0 {
		last := v.Applications[n-1]
		return int32(n + 1), last.CreatedAt, last.CreatedBy
	}
	return 1, v.CreatedAt, v.CreatedBy
}

// CreditInput applies a payment's credit on account, as a client sends
// it: the posting date (nil for the company's date now) and the items
// with their residuals, one per item or none.
type CreditInput struct {
	PaymentID   uuid.UUID
	PostingDate *time.Time
	Items       []ledger.ItemRef
	Residuals   []*ResidualInput
}

// ApplyOnAccount applies the payment's credit on account to the
// customer's items: the ledger clears the credit and the items with one
// entry, leaving what they do not take on account as a new credit line,
// at the credit's rate, and writing an underpayment within the company's
// tolerance off. The application is written with its items, an audit
// record and IncomingPaymentOnAccountApplied in the request's transaction.
//
// A credit no longer open, all applied or cleared otherwise, is refused,
// 409 receivables.no_credit_on_account. That read takes no lock
// (ledger.OpenItems.Closed), so the entries' locks are taken once, in the
// ledger's one sorted call before any number range: two applications
// racing for one credit both find it open, and the ledger, marking it
// cleared under those locks, refuses the second (409
// ledger.item_already_cleared), which rolls back whole.
func (e *Endpoints) ApplyOnAccount(ctx context.Context, in CreditInput) (IncomingPayment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return IncomingPayment{}, err
	}
	v, err := GetPayment(ctx, in.PaymentID)
	if err != nil {
		return IncomingPayment{}, err
	}
	credit, err := e.openCredit(ctx, v)
	if err != nil {
		return IncomingPayment{}, err
	}
	app, le, number, err := e.creditApplication(ctx, tx, v, in, credit)
	if err != nil {
		return IncomingPayment{}, err
	}
	applied, err := e.Postings.ApplyReceipt(ctx, app)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: apply the credit on account of incoming payment %s: %w", v.DocumentNumber, err)
	}
	no := int32(len(v.Applications) + 1)
	if err := insertApplication(ctx, tx, p, v, no, app, applied); err != nil {
		return IncomingPayment{}, err
	}
	if err := insertPaymentItems(ctx, tx, p.TenantID, v.ID, int32(len(v.Items)), no, applying{items: in.Items, inputs: in.Residuals, residuals: app.Residuals},
		applied); err != nil {
		return IncomingPayment{}, err
	}
	after, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, v.ID))
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: read incoming payment %s: %w", v.ID, err)
	}
	return after, recordCreditApplied(ctx, after, le, number, applied, len(in.Items))
}

// openCredit is the payment's credit on account while it is open in the
// ledger: none left, or one cleared already, is refused.
func (e *Endpoints) openCredit(ctx context.Context, v IncomingPayment) (ledger.ItemRef, error) {
	credit, _, ok := v.credit()
	if ok {
		closed, err := e.OpenItems.Closed(ctx, []ledger.ItemRef{credit})
		if err != nil {
			return ledger.ItemRef{}, fmt.Errorf("receivables: credit on account of incoming payment %s: %w", v.DocumentNumber, err)
		}
		ok = !closed[credit]
	}
	if !ok {
		return ledger.ItemRef{}, problem.Conflict("receivables.no_credit_on_account",
			"Incoming payment "+v.DocumentNumber+" has no credit open on the customer's account: it left none, it was applied in full, or it was cleared otherwise.")
	}
	return credit, nil
}

// creditApplication is the ledger's application of the payment's credit
// as the request asks for it, with the company and the customer's number
// it is posted and published with: the credit on its account is the
// receipt, what is left goes back there, and an underpayment within the
// company's tolerance is written off; an overpayment is not, it stays on
// account.
func (e *Endpoints) creditApplication(ctx context.Context, tx pgx.Tx, v IncomingPayment, in CreditInput, credit ledger.ItemRef) (ledger.ReceiptApplication,
	tenancy.Entity, string, error) {
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored payment's legal entity exists
	if err != nil {
		return ledger.ReceiptApplication{}, tenancy.Entity{}, "", fmt.Errorf("receivables: company of incoming payment %s: %w", v.ID, err)
	}
	residuals, err := residualsOf(in.Residuals, len(in.Items), v.Currency, v.Exponent)
	if err != nil {
		return ledger.ReceiptApplication{}, tenancy.Entity{}, "", err
	}
	under, _, err := e.tolerance(ctx, tx, le)
	if err != nil {
		return ledger.ReceiptApplication{}, tenancy.Entity{}, "", err
	}
	date, _, err := e.paymentDate(ctx, le, in.PostingDate)
	if err != nil {
		return ledger.ReceiptApplication{}, tenancy.Entity{}, "", err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{v.CustomerID})
	if err != nil {
		return ledger.ReceiptApplication{}, tenancy.Entity{}, "", fmt.Errorf("receivables: customer of incoming payment %s: %w", v.ID, err)
	}
	number := names[v.CustomerID].Number // a stored payment's customer exists: business partners are never deleted
	text := "Incoming payment " + v.DocumentNumber + " on account of customer " + number + " applied"
	account := *v.OnAccountAccount // a payment with a credit names its account (incoming_payment_on_account)
	return ledger.ReceiptApplication{LegalEntityID: le.ID, DocumentType: PaymentDocumentType, PostingDate: date, Currency: v.Currency, ReceiptAccountID: account,
		Receipt: credit, Credit: true, CustomerID: v.CustomerID, Items: in.Items, Residuals: residuals, Under: under, OnAccount: account, HeaderText: &text,
		SourceContext: "receivables", SourceDocumentID: v.ID}, le, number, nil
}

// insertApplication writes the application: the credit it consumed and
// what that stood at, what it wrote off and left on account, its entry.
func insertApplication(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v IncomingPayment, no int32, app ledger.ReceiptApplication,
	applied ledger.AppliedReceipt) error {
	var line *int32
	if applied.OnAccountLine != 0 {
		line = &applied.OnAccountLine
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment_application (tenant_id, incoming_payment_id, application_no, posting_date,
			credit_entry_year, credit_entry_id, credit_line_no, amount, difference, on_account_amount, on_account_line_no, clearing_entry_year, clearing_entry_id,
			created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		p.TenantID, v.ID, no, app.PostingDate, app.Receipt.FiscalYear, app.Receipt.EntryID, app.Receipt.LineNo, applied.Receipt, applied.Difference,
		applied.OnAccount, line, applied.Entry.FiscalYear, applied.Entry.ID, p.ID); err != nil {
		return fmt.Errorf("receivables: insert application %d of incoming payment %s: %w", no, v.ID, err)
	}
	return nil
}

// recordCreditApplied adds the audit record of the application and
// publishes IncomingPaymentOnAccountApplied in the same transaction
// (ADR-0007, ADR-0011): the customer by id and number, never by name
// (product owner, 2026-11-05), the amounts and the number of items.
func recordCreditApplied(ctx context.Context, v IncomingPayment, le tenancy.Entity, customer string, applied ledger.AppliedReceipt, items int) error {
	const action = "receivables.incoming_payment.apply_on_account"
	a := v.Applications[len(v.Applications)-1] // the one just written
	residuals := 0
	for _, it := range v.Items {
		if it.ApplicationNo == a.No && it.ResidualAmount != nil {
			residuals++
		}
	}
	amount := func(m money.Minor) map[string]string { return money.EventAmount(m, v.Currency, v.Exponent) }
	entry := applied.Entry
	payload := map[string]any{"incoming_payment_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": customer, "application_no": a.No, "posting_date": a.PostingDate,
		"amount": amount(a.Amount), "difference": amount(a.Difference), "on_account_amount": amount(a.OnAccount), "journal_entry_id": entry.ID,
		"journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber, "item_count": items, "residual_count": residuals}
	detail := map[string]any{"document_number": v.DocumentNumber, "application_no": a.No, "posting_date": a.PostingDate, "amount": amount(a.Amount),
		"difference": amount(a.Difference), "on_account": amount(a.OnAccount), "journal_entry_id": entry.ID, "journal_entry_number": entry.DocumentNumber,
		"items": items, "residuals": residuals}
	if err := audit.Record(ctx, action, "receivables.incoming_payment", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "incoming_payment_on_account_applied", AggregateType: "receivables.incoming_payment", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of incoming payment %s: %w", action, v.ID, err)
	}
	return nil
}
