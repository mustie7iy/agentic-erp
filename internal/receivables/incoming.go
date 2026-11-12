package receivables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Incoming payments applied (ADR-0063 §6, T75c): a receipt a bank
// statement posted to a house bank's incoming payments clearing account
// (ADR-0047) is applied to a customer's open items in full and exactly.
// The application is a receivables document, written once; the clearing
// is the ledger's (ledger.Postings.ApplyReceipt).

// PaymentNumberRange is the document type incoming payments draw their
// numbers from (configuration number_ranges).
const PaymentNumberRange = "incoming_payment"

// PaymentDocumentType is the journal document type of the entry that
// clears a receipt and the customer's items (SAP's DZ, customer payment);
// its entries draw their numbers from the journal_dz number range.
const PaymentDocumentType = "DZ"

// PaymentItem is an open item an incoming payment was applied to, with
// the amount it stood at: debit positive, a credit of the customer's
// negative.
type PaymentItem struct {
	ItemNo     int32       `json:"item_no"`
	FiscalYear int16       `json:"fiscal_year"`
	EntryID    uuid.UUID   `json:"journal_entry_id"`
	LineNo     int32       `json:"line_no"`
	Amount     money.Minor `json:"amount"`
}

// IncomingPayment is a row of receivables.incoming_payment with its
// items. Amounts are minor units of Currency, whose minor-unit digits are
// Exponent; Amount is the receipt's, the sum of the items'.
type IncomingPayment struct {
	ID               uuid.UUID
	DocumentNumber   string
	LegalEntityID    uuid.UUID
	CustomerID       uuid.UUID
	ReceiptYear      int16
	ReceiptEntryID   uuid.UUID
	ReceiptAccountID uuid.UUID
	PostingDate      time.Time
	Currency         string
	Exponent         int
	Amount           money.Minor
	EntryYear        int16
	EntryID          uuid.UUID
	Items            []PaymentItem
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
}

// PaymentInput is an incoming payment to apply, as a client sends it.
type PaymentInput struct {
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	ReceiptEntryID uuid.UUID
	PostingDate    *time.Time
	Items          []ledger.ItemRef
}

// paymentSelect reads incoming payments with their items by item number.
const paymentSelect = `SELECT p.id, p.document_number, p.legal_entity_id, p.customer_id, p.receipt_entry_year, p.receipt_entry_id, p.receipt_account_id,
		p.posting_date, p.currency, c.exponent, p.amount, p.clearing_entry_year, p.clearing_entry_id,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'fiscal_year', t.fiscal_year, 'journal_entry_id', t.journal_entry_id,
			'line_no', t.line_no, 'amount', t.amount) ORDER BY t.item_no)
		 FROM receivables.incoming_payment_item t WHERE t.tenant_id = p.tenant_id AND t.incoming_payment_id = p.id), '[]'),
		p.created_at, p.created_by
	FROM receivables.incoming_payment p JOIN ref.currency c ON c.code = p.currency`

func scanPayment(row pgx.Row) (IncomingPayment, error) {
	var v IncomingPayment
	err := row.Scan(&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.CustomerID, &v.ReceiptYear, &v.ReceiptEntryID, &v.ReceiptAccountID, &v.PostingDate,
		&v.Currency, &v.Exponent, &v.Amount, &v.EntryYear, &v.EntryID, &v.Items, &v.CreatedAt, &v.CreatedBy)
	return v, errs.Wrapf(err, "scan incoming payment")
}

// GetPayment returns an incoming payment of a legal entity the caller may
// act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetPayment(ctx context.Context, id uuid.UUID) (IncomingPayment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return IncomingPayment{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: company access for incoming payment %s: %w", id, err)
	}
	v, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(v.LegalEntityID)) {
		return IncomingPayment{}, problem.NotFoundOf("incoming payment")
	}
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: read incoming payment %s: %w", id, err)
	}
	return v, nil
}

// ApplyPayment applies the receipt to the customer's items (ADR-0063 §6):
// the ledger posts one entry that offsets the receipt and every item at
// their booked amounts and clears them all, refusing items that are not
// the customer's open items in the company or do not equal the receipt
// exactly in its currency; the application is then written as a document
// with its items, its audit record and IncomingPaymentApplied, all in the
// request's transaction.
//
// The ledger is called before the document takes its number: the ledger
// takes the entries' locks before any number range (ledger.lockEntries),
// and under them reads the receipt and the items as still open, so a
// receipt or an item applied, cleared, reversed or reserved meanwhile is
// refused there (ADR-0008). The receipt read here first only names the
// account it waits on and answers a receipt that is no longer open.
func (e *Endpoints) ApplyPayment(ctx context.Context, in PaymentInput) (IncomingPayment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return IncomingPayment{}, err
	}
	le, customer, err := e.payer(ctx, in)
	if err != nil {
		return IncomingPayment{}, err
	}
	receipt, err := e.openReceipt(ctx, tx, le, in.ReceiptEntryID)
	if err != nil {
		return IncomingPayment{}, err
	}
	date, year, err := e.paymentDate(ctx, le, in.PostingDate)
	if err != nil {
		return IncomingPayment{}, err
	}
	id := uuid.Must(uuid.NewV7())
	text := "Incoming payment of customer " + customer.Number
	applied, err := e.Postings.ApplyReceipt(ctx, ledger.ReceiptApplication{LegalEntityID: le.ID, DocumentType: PaymentDocumentType, PostingDate: date,
		Currency: receipt.Currency, ReceiptAccountID: receipt.AccountID, Receipt: receipt.Item, CustomerID: in.CustomerID, Items: in.Items,
		HeaderText: &text, SourceContext: "receivables", SourceDocumentID: id})
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: apply receipt %s to customer %s: %w", receipt.Number, customer.Number, err)
	}
	number, err := e.Numbers.Next(ctx, le.ID, PaymentNumberRange, year)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: number an incoming payment: %w", err)
	}
	if err := insertPayment(ctx, tx, p, id, number, in, receipt, date, applied); err != nil {
		return IncomingPayment{}, err
	}
	v, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, id))
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: read incoming payment %s: %w", id, err)
	}
	return v, recordApplied(ctx, v, le, customer, applied.Entry)
}

// payer resolves the legal entity, which the caller must be allowed to
// act in, and the customer, a business partner. Whether the items are the
// customer's is the ledger's check; a customer blocked for posting may
// still pay what it owes.
func (e *Endpoints) payer(ctx context.Context, in PaymentInput) (tenancy.Entity, partner.Named, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: legal entity of an incoming payment: %w", err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{in.CustomerID})
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: customer %s: %w", in.CustomerID, err)
	}
	named, ok := names[in.CustomerID]
	if !ok {
		return tenancy.Entity{}, partner.Named{}, problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	}
	return le, named, nil
}

// incomingAccounts are the incoming payments clearing accounts of the
// company's house banks (configuration incoming_gl_account, ADR-0047), by
// id, each with the code of its house bank: the first that names it.
func (e *Endpoints) incomingAccounts(ctx context.Context, le tenancy.Entity) (map[uuid.UUID]string, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: house banks of company %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	if co == nil {
		return nil, nil
	}
	return e.bankAccounts(ctx, le, co.AllHouseBanks())
}

// bankAccounts are the banks' incoming payments clearing accounts by id,
// each with its house bank's code.
func (e *Endpoints) bankAccounts(ctx context.Context, le tenancy.Entity, houseBanks []config.HouseBank) (map[uuid.UUID]string, error) {
	banks := map[string]string{}
	var numbers []string
	for _, hb := range houseBanks {
		if _, named := banks[hb.IncomingGLAccount]; hb.IncomingGLAccount != "" && !named {
			banks[hb.IncomingGLAccount] = hb.Name
			numbers = append(numbers, hb.IncomingGLAccount)
		}
	}
	ids, err := e.References.Accounts(ctx, le.ID, numbers)
	if err != nil {
		return nil, fmt.Errorf("receivables: incoming payments clearing accounts of company %s: %w", le.Code, err)
	}
	out := map[uuid.UUID]string{}
	for number, id := range ids {
		out[id] = banks[number]
	}
	return out, nil
}

// accountIDs are the ids of the accounts of the house bank with the code,
// or of every house bank for "".
func accountIDs(accounts map[uuid.UUID]string, houseBank string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(accounts))
	for id, code := range accounts {
		if houseBank == "" || code == houseBank {
			ids = append(ids, id)
		}
	}
	return ids
}

// openReceipt is the receipt the entry posted, waiting on an incoming
// payments clearing account of the company. One that is not: 409
// receivables.receipt_already_applied when an incoming payment applied
// it, else 422 receivables.receipt_not_open naming the request's field.
// The read takes no lock: a receipt another request applies meanwhile is
// still found here, and the ledger refuses the second application, 409
// ledger.item_already_cleared.
func (e *Endpoints) openReceipt(ctx context.Context, tx pgx.Tx, le tenancy.Entity, entry uuid.UUID) (ledger.Receipt, error) {
	accounts, err := e.incomingAccounts(ctx, le)
	if err != nil {
		return ledger.Receipt{}, err
	}
	receipts, err := e.Receipts.Receipts(ctx, nil, le.ID, accountIDs(accounts, ""), ledger.ReceiptFilter{Entry: &entry})
	if err != nil {
		return ledger.Receipt{}, fmt.Errorf("receivables: receipt %s: %w", entry, err)
	}
	if len(receipts) == 1 { // one row per entry (ledger.OpenItems.Receipts)
		return receipts[0], nil
	}
	var number string
	err = tx.QueryRow(ctx, `SELECT document_number FROM receivables.incoming_payment WHERE receipt_entry_id = $1 AND legal_entity_id = $2`, entry, le.ID).Scan(&number)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ledger.Receipt{}, problem.Invalid("receivables.receipt_not_open", "receipt_journal_entry_id",
			"The journal entry is not a receipt waiting on an incoming payments clearing account of the company's house banks.")
	case err != nil:
		return ledger.Receipt{}, fmt.Errorf("receivables: the incoming payment that applied receipt %s: %w", entry, err)
	}
	return ledger.Receipt{}, problem.Conflict("receivables.receipt_already_applied", "Incoming payment "+number+" applied the receipt.")
}

// paymentDate is the date the application posts on, the request's, else
// the company's date now in its time zone (product owner 2026-10-31), and
// that date's fiscal year, which numbers the document.
func (e *Endpoints) paymentDate(ctx context.Context, le tenancy.Entity, requested *time.Time) (time.Time, int16, error) {
	var date time.Time
	if requested != nil {
		date = *requested
	} else {
		day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("receivables: posting date of an incoming payment: %w", err)
		}
		date = day.Date
	}
	year, err := e.Calendar.FiscalYear(ctx, le.ID, date)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("receivables: fiscal year of %s: %w", date.Format(time.DateOnly), err)
	}
	return date, year, nil
}

// insertPayment writes the application and its items, in the request's
// order, each with the amount the ledger cleared it at.
func insertPayment(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, number string, in PaymentInput, receipt ledger.Receipt,
	date time.Time, applied ledger.AppliedReceipt) error {
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		p.TenantID, id, number, in.LegalEntityID, in.CustomerID, receipt.Item.FiscalYear, receipt.Item.EntryID, receipt.AccountID, date, receipt.Currency,
		applied.Receipt, applied.Entry.FiscalYear, applied.Entry.ID, p.ID); err != nil {
		return fmt.Errorf("receivables: insert incoming payment %s: %w", id, err)
	}
	n := len(in.Items)
	nos, years, entries, lines := make([]int32, n), make([]int16, n), make([]uuid.UUID, n), make([]int32, n)
	for i, it := range in.Items {
		nos[i], years[i], entries[i], lines[i] = int32(i+1), it.FiscalYear, it.EntryID, it.LineNo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount)
		SELECT $1, $2, n, y, je, l, a FROM unnest($3::int[], $4::smallint[], $5::uuid[], $6::int[], $7::bigint[]) AS x(n, y, je, l, a)`,
		p.TenantID, id, nos, years, entries, lines, applied.Items); err != nil {
		return fmt.Errorf("receivables: write items of incoming payment %s: %w", id, err)
	}
	return nil
}

// recordApplied adds the audit record of the application and publishes
// IncomingPaymentApplied in the same transaction (ADR-0007, ADR-0011).
// The event names the customer by id and number, never by name (product
// owner, 2026-11-05), and carries the amount and the number of items, not
// the items.
func recordApplied(ctx context.Context, v IncomingPayment, le tenancy.Entity, customer partner.Named, entry ledger.Entry) error {
	const action = "receivables.incoming_payment.apply"
	amount := money.EventAmount(v.Amount, v.Currency, v.Exponent)
	payload := map[string]any{"incoming_payment_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": customer.Number,
		"posting_date": v.PostingDate.Format(time.DateOnly), "amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID,
		"journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber, "item_count": len(v.Items)}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "posting_date": v.PostingDate.Format(time.DateOnly),
		"amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID, "journal_entry_id": entry.ID, "journal_entry_number": entry.DocumentNumber,
		"items": len(v.Items)}
	if err := audit.Record(ctx, action, "receivables.incoming_payment", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "incoming_payment_applied", AggregateType: "receivables.incoming_payment", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of incoming payment %s: %w", action, v.ID, err)
	}
	return nil
}
