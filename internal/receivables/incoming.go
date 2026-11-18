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
// (ADR-0047) is applied to a customer's open items: in full and exactly,
// or with residual items and a difference within the company's tolerance
// (T75f, residual.go), what is left put on the customer's account (T75g,
// onaccount.go).
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
	// The residual item the payment left of the item, paid in part
	// (T75f): what stayed open, its line in the clearing entry and why;
	// nil for an item paid in full.
	ResidualAmount *money.Minor `json:"residual_amount"`
	ResidualLineNo *int32       `json:"residual_line_no"`
	ResidualReason *string      `json:"residual_reason"`
	ResidualText   *string      `json:"residual_reason_text"`
	// ApplicationNo is the application of the payment's credit on account
	// that applied the item (T75g); 0 for the payment itself.
	ApplicationNo int32 `json:"application_no"`
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
	// Difference is the payment difference written off (T75f): what the
	// items, net of their residuals, exceed the receipt by.
	Difference money.Minor
	// OnAccount is what the payment left on the customer's account (T75g),
	// a credit on OnAccountAccount, line OnAccountLine of the clearing
	// entry; zero and nil for none. Applications are the applications of
	// that credit, by number.
	OnAccount        money.Minor
	OnAccountLine    *int32
	OnAccountAccount *uuid.UUID
	Applications     []PaymentApplication
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
	// Residuals, one per item or none, are what stays open of the items
	// paid in part (T75f); nil for an item paid in full.
	Residuals []*ResidualInput
	// OnAccount leaves what the receipt has left after the items on the
	// customer's account as a credit (T75g).
	OnAccount bool
}

// paymentSelect reads incoming payments with their items by item number.
const paymentSelect = `SELECT p.id, p.document_number, p.legal_entity_id, p.customer_id, p.receipt_entry_year, p.receipt_entry_id, p.receipt_account_id,
		p.posting_date, p.currency, c.exponent, p.amount, p.difference, p.clearing_entry_year, p.clearing_entry_id,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'fiscal_year', t.fiscal_year, 'journal_entry_id', t.journal_entry_id,
			'line_no', t.line_no, 'amount', t.amount, 'residual_amount', t.residual_amount, 'residual_line_no', t.residual_line_no,
			'residual_reason', t.residual_reason, 'residual_reason_text', t.residual_reason_text, 'application_no', t.application_no) ORDER BY t.item_no)
		 FROM receivables.incoming_payment_item t WHERE t.tenant_id = p.tenant_id AND t.incoming_payment_id = p.id), '[]'),
		p.on_account_amount, p.on_account_line_no, p.on_account_account_id,
		coalesce((SELECT json_agg(json_build_object('application_no', a.application_no, 'posting_date', a.posting_date, 'credit_entry_year', a.credit_entry_year,
			'credit_entry_id', a.credit_entry_id, 'credit_line_no', a.credit_line_no, 'amount', a.amount, 'difference', a.difference,
			'on_account_amount', a.on_account_amount, 'on_account_line_no', a.on_account_line_no, 'clearing_entry_year', a.clearing_entry_year,
			'clearing_entry_id', a.clearing_entry_id, 'created_at', a.created_at, 'created_by', a.created_by) ORDER BY a.application_no)
		 FROM receivables.incoming_payment_application a WHERE a.tenant_id = p.tenant_id AND a.incoming_payment_id = p.id), '[]'),
		p.created_at, p.created_by
	FROM receivables.incoming_payment p JOIN ref.currency c ON c.code = p.currency`

func scanPayment(row pgx.Row) (IncomingPayment, error) {
	var v IncomingPayment
	err := row.Scan(&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.CustomerID, &v.ReceiptYear, &v.ReceiptEntryID, &v.ReceiptAccountID, &v.PostingDate,
		&v.Currency, &v.Exponent, &v.Amount, &v.Difference, &v.EntryYear, &v.EntryID, &v.Items, &v.OnAccount, &v.OnAccountLine, &v.OnAccountAccount,
		&v.Applications, &v.CreatedAt, &v.CreatedBy)
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
// the customer's open items in the company or do not, net of their
// residuals, equal the receipt in its currency but for a difference within
// the tolerance or what goes on account; the application is then written
// as a document
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
	id := uuid.Must(uuid.NewV7())
	app, err := e.receiptApplication(ctx, tx, le, customer, in, receipt, id)
	if err != nil {
		return IncomingPayment{}, err
	}
	date, year, err := e.paymentDate(ctx, le, in.PostingDate)
	if err != nil {
		return IncomingPayment{}, err
	}
	app.ledger.PostingDate = date
	applied, err := e.Postings.ApplyReceipt(ctx, app.ledger)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: apply receipt %s to customer %s: %w", receipt.Number, customer.Number, err)
	}
	if in.OnAccount && applied.OnAccount == 0 {
		return IncomingPayment{}, problem.Unprocessable("receivables.nothing_left_on_account",
			"The items take the whole receipt: nothing is left to put on the customer's account.",
			problem.FieldError{Field: "on_account", Code: "nothing_left", Message: "false, or fewer items"})
	}
	number, err := e.Numbers.Next(ctx, le.ID, PaymentNumberRange, year)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: number an incoming payment: %w", err)
	}
	if err := insertPayment(ctx, tx, p, id, number, in, app, receipt, date, applied); err != nil {
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
func insertPayment(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, number string, in PaymentInput, app receiptApplication,
	receipt ledger.Receipt, date time.Time, applied ledger.AppliedReceipt) error {
	var line *int32
	var account *uuid.UUID
	if applied.OnAccountLine != 0 {
		line, account = &applied.OnAccountLine, &app.ledger.OnAccount
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, difference, on_account_amount, on_account_line_no, on_account_account_id,
			clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		p.TenantID, id, number, in.LegalEntityID, in.CustomerID, receipt.Item.FiscalYear, receipt.Item.EntryID, receipt.AccountID, date, receipt.Currency,
		applied.Receipt, applied.Difference, applied.OnAccount, line, account, applied.Entry.FiscalYear, applied.Entry.ID, p.ID); err != nil {
		return fmt.Errorf("receivables: insert incoming payment %s: %w", id, err)
	}
	return insertPaymentItems(ctx, tx, p.TenantID, id, 0, 0, applying{items: in.Items, inputs: in.Residuals, residuals: app.ledger.Residuals}, applied)
}

// receiptApplication is the ledger's application of a receipt as the
// request asks for it, but its posting date: the items with their
// residuals (residualsOf), the company's tolerance, and with on_account
// the customer's reconciliation account what is left goes to.
type receiptApplication struct {
	ledger ledger.ReceiptApplication
}

// receiptApplication reads what the request asks for into the ledger's
// application of the receipt; the payment's id is the clearing entry's
// source document.
func (e *Endpoints) receiptApplication(ctx context.Context, tx pgx.Tx, le tenancy.Entity, customer partner.Named, in PaymentInput, receipt ledger.Receipt,
	id uuid.UUID) (receiptApplication, error) {
	residuals, err := residualsOf(in.Residuals, len(in.Items), receipt.Currency, receipt.Exponent)
	if err != nil {
		return receiptApplication{}, err
	}
	under, over, err := e.tolerance(ctx, tx, le)
	if err != nil {
		return receiptApplication{}, err
	}
	text := "Incoming payment of customer " + customer.Number
	app := ledger.ReceiptApplication{LegalEntityID: le.ID, DocumentType: PaymentDocumentType, Currency: receipt.Currency, ReceiptAccountID: receipt.AccountID,
		Receipt: receipt.Item, CustomerID: in.CustomerID, Items: in.Items, Residuals: residuals, Under: under, Over: over, HeaderText: &text,
		SourceContext: "receivables", SourceDocumentID: id}
	if in.OnAccount {
		if app.OnAccount, err = e.customerAccount(ctx, in.CustomerID, le); err != nil {
			return receiptApplication{}, err
		}
	}
	return receiptApplication{ledger: app}, nil
}

// customerAccount is the customer's reconciliation account in the company
// (its company data, T74a), where a payment on account is posted: a
// customer without company data there is refused, 422
// receivables.customer_not_usable naming customer_id. A customer blocked
// for posting may still pay (ApplyPayment).
func (e *Endpoints) customerAccount(ctx context.Context, customer uuid.UUID, le tenancy.Entity) (uuid.UUID, error) {
	c, _, err := e.CustomerData.Customer(ctx, customer, le.ID) // the customer exists (payer)
	if err != nil {
		return uuid.Nil, fmt.Errorf("receivables: customer %s in company %s: %w", customer, le.Code, err)
	}
	if !c.IsCustomer || !c.SetUp {
		return uuid.Nil, problem.Invalid("receivables.customer_not_usable", "customer_id",
			"A payment is put on account only for a customer set up in the company, on its reconciliation account.")
	}
	return c.ReconciliationID, nil
}

// applying is what an application applies: the items, with the residuals
// as the request gave them and as read (residualsOf), each one per item or
// none.
type applying struct {
	items     []ledger.ItemRef
	inputs    []*ResidualInput
	residuals []ledger.Residual
}

// insertPaymentItems writes the items of an application, numbered from after
// the first, in the request's order, each with the amount the ledger
// cleared it at and its residual item, if it left one.
func insertPaymentItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, first, application int32, a applying, applied ledger.AppliedReceipt) error {
	n := len(a.items)
	nos, years, entries, lines := make([]int32, n), make([]int16, n), make([]uuid.UUID, n), make([]int32, n)
	amounts, residualLines, reasons, texts := make([]*money.Minor, n), make([]*int32, n), make([]*string, n), make([]*string, n)
	for i, it := range a.items {
		nos[i], years[i], entries[i], lines[i] = first+int32(i+1), it.FiscalYear, it.EntryID, it.LineNo
		if line := applied.ResidualLines[i]; line != 0 { // a residual the request gave (residualsOf)
			amount := a.residuals[i].Amount
			amounts[i], residualLines[i], reasons[i], texts[i] = &amount, &line, &a.inputs[i].Reason, a.inputs[i].Text
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			residual_amount, residual_line_no, residual_reason, residual_reason_text, application_no)
		SELECT $1, $2, n, y, je, l, a, ra, rl, rr, rt, $12
		FROM unnest($3::int[], $4::smallint[], $5::uuid[], $6::int[], $7::bigint[], $8::bigint[], $9::int[], $10::text[], $11::text[])
			AS x(n, y, je, l, a, ra, rl, rr, rt)`,
		tenant, id, nos, years, entries, lines, applied.Items, amounts, residualLines, reasons, texts, application); err != nil {
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
	amount, residuals := money.EventAmount(v.Amount, v.Currency, v.Exponent), 0
	for _, it := range v.Items {
		if it.ResidualAmount != nil {
			residuals++
		}
	}
	payload := map[string]any{"incoming_payment_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": customer.Number,
		"posting_date": v.PostingDate.Format(time.DateOnly), "amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID,
		"journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber, "item_count": len(v.Items),
		"residual_count": residuals, "difference": money.EventAmount(v.Difference, v.Currency, v.Exponent),
		"on_account_amount": money.EventAmount(v.OnAccount, v.Currency, v.Exponent)}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "posting_date": v.PostingDate.Format(time.DateOnly),
		"amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID, "journal_entry_id": entry.ID, "journal_entry_number": entry.DocumentNumber,
		"items": len(v.Items), "residuals": residuals, "difference": money.EventAmount(v.Difference, v.Currency, v.Exponent),
		"on_account": money.EventAmount(v.OnAccount, v.Currency, v.Exponent)}
	if err := audit.Record(ctx, action, "receivables.incoming_payment", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "incoming_payment_applied", AggregateType: "receivables.incoming_payment", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of incoming payment %s: %w", action, v.ID, err)
	}
	return nil
}
