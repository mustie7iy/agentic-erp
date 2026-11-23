package receivables

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// onaccount.go), an invoice paid within its discount period taking its
// cash discount in a company whose country corrects the output tax on it
// (discount.go).
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
	ItemNo int32 `json:"item_no"`
	// CustomerID is the customer whose item it is: the payment's, or one
	// of the other customers its receipt paid (T75h).
	CustomerID uuid.UUID   `json:"customer_id"`
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
	// What the item's reason wrote off of it instead of leaving a residual
	// item (T75o): the amount, its line in the clearing entry, the account
	// and the reason; nil for none.
	WriteOffAmount  *money.Minor `json:"write_off_amount"`
	WriteOffLineNo  *int32       `json:"write_off_line_no"`
	WriteOffAccount *uuid.UUID   `json:"write_off_account_id"`
	WriteOffReason  *string      `json:"write_off_reason"`
	WriteOffText    *string      `json:"write_off_reason_text"`
	// CashDiscount is the cash discount the item took (discount.go), in
	// the payment's currency; nil for none.
	CashDiscount *money.Minor `json:"cash_discount"`
	// An item in another currency than the payment's (T75n): its own
	// currency, with that currency's minor-unit digits, its own amount,
	// signed as Amount, and the rate it was translated at; nil for an item
	// in the payment's currency.
	ItemCurrency *string      `json:"item_currency"`
	ItemExponent *int         `json:"item_exponent"`
	ItemAmount   *money.Minor `json:"item_amount"`
	ExchangeRate *string      `json:"exchange_rate"`
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
	// Resets are the applications undone (T75i), the payment's own
	// included, in the order they were.
	Resets []PaymentReset
	// Recharges are the bad debt reliefs its applications and undoings
	// charged again (recharge.go), in the order they were posted.
	Recharges []PaymentRecharge
	EntryYear int16
	EntryID   uuid.UUID
	Items     []PaymentItem
	CreatedAt time.Time
	CreatedBy uuid.UUID
}

// PaymentInput is an incoming payment to apply, as a client sends it.
type PaymentInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	// OtherCustomers are the other customers whose items the receipt pays
	// besides CustomerID's, the payer's (T75h).
	OtherCustomers []uuid.UUID
	ReceiptEntryID uuid.UUID
	PostingDate    *time.Time
	Items          []ledger.ItemRef
	// Residuals, one per item or none, are what stays open of the items
	// paid in part (T75f); nil for an item paid in full.
	Residuals []*ResidualInput
	// OnAccount leaves what the receipt has left after the items on the
	// customer's account as a credit (T75g).
	OnAccount bool
	// CashDiscounts, one per item or none, say which items take the cash
	// discount their payment terms grant (discount.go).
	CashDiscounts []bool
}

// paymentSelect reads incoming payments with their items by item number.
const paymentSelect = `SELECT p.id, p.document_number, p.legal_entity_id, p.customer_id, p.receipt_entry_year, p.receipt_entry_id, p.receipt_account_id,
		p.posting_date, p.currency, c.exponent, p.amount, p.difference, p.clearing_entry_year, p.clearing_entry_id,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'customer_id', coalesce(t.customer_id, p.customer_id), 'fiscal_year', t.fiscal_year, 'journal_entry_id', t.journal_entry_id,
			'line_no', t.line_no, 'amount', t.amount, 'residual_amount', t.residual_amount, 'residual_line_no', t.residual_line_no,
			'residual_reason', t.residual_reason, 'residual_reason_text', t.residual_reason_text, 'application_no', t.application_no,
			'item_currency', t.item_currency, 'item_exponent', (SELECT x.exponent FROM ref.currency x WHERE x.code = t.item_currency), 'item_amount', t.item_amount,
			'exchange_rate', t.exchange_rate::text, 'write_off_amount', t.write_off_amount, 'write_off_line_no', t.write_off_line_no,
			'write_off_account_id', t.write_off_account_id, 'write_off_reason', t.write_off_reason, 'write_off_reason_text', t.write_off_reason_text,
			'cash_discount', t.cash_discount) ORDER BY t.item_no)
		 FROM receivables.incoming_payment_item t WHERE t.tenant_id = p.tenant_id AND t.incoming_payment_id = p.id), '[]'),
		p.on_account_amount, p.on_account_line_no, p.on_account_account_id,
		coalesce((SELECT json_agg(json_build_object('application_no', a.application_no, 'posting_date', a.posting_date, 'credit_entry_year', a.credit_entry_year,
			'credit_entry_id', a.credit_entry_id, 'credit_line_no', a.credit_line_no, 'amount', a.amount, 'difference', a.difference,
			'on_account_amount', a.on_account_amount, 'on_account_line_no', a.on_account_line_no, 'clearing_entry_year', a.clearing_entry_year,
			'clearing_entry_id', a.clearing_entry_id, 'created_at', a.created_at, 'created_by', a.created_by) ORDER BY a.application_no)
		 FROM receivables.incoming_payment_application a WHERE a.tenant_id = p.tenant_id AND a.incoming_payment_id = p.id), '[]'),
		coalesce((SELECT json_agg(json_build_object('application_no', coalesce(r.application_no, 0), 'reason', r.reason, 'reversal_entry_year', r.reversal_entry_year,
			'reversal_entry_id', r.reversal_entry_id, 'created_at', r.created_at, 'created_by', r.created_by) ORDER BY r.created_at, coalesce(r.application_no, 0))
		 FROM receivables.incoming_payment_reset r WHERE r.tenant_id = p.tenant_id AND r.incoming_payment_id = p.id), '[]'),
		` + rechargeSelect + `, p.created_at, p.created_by
	FROM receivables.incoming_payment p JOIN ref.currency c ON c.code = p.currency`

func scanPayment(row pgx.Row) (IncomingPayment, error) {
	var v IncomingPayment
	err := row.Scan(&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.CustomerID, &v.ReceiptYear, &v.ReceiptEntryID, &v.ReceiptAccountID, &v.PostingDate,
		&v.Currency, &v.Exponent, &v.Amount, &v.Difference, &v.EntryYear, &v.EntryID, &v.Items, &v.OnAccount, &v.OnAccountLine, &v.OnAccountAccount,
		&v.Applications, &v.Resets, &v.Recharges, &v.CreatedAt, &v.CreatedBy)
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
// account it waits on and answers a receipt that is no longer open. The
// customers are held (RechargeLock) before the ledger's entries, and what
// the payment pays of their invoices a bad debt relief relieved charges
// the relieved tax again (recharge.go).
func (e *Endpoints) ApplyPayment(ctx context.Context, in PaymentInput) (IncomingPayment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return IncomingPayment{}, err
	}
	le, names, err := e.payer(ctx, in)
	if err != nil {
		return IncomingPayment{}, err
	}
	payer := names[in.CustomerID]
	receipt, err := e.openReceipt(ctx, tx, le, in.ReceiptEntryID)
	if err != nil {
		return IncomingPayment{}, err
	}
	id := uuid.Must(uuid.NewV7())
	app, err := e.receiptApplication(ctx, tx, le, payer, in, receipt, id)
	if err != nil {
		return IncomingPayment{}, err
	}
	date, year, err := e.paymentDate(ctx, le, in.PostingDate)
	if err != nil {
		return IncomingPayment{}, err
	}
	app.ledger.PostingDate = date
	relieved, err := relievedOf(ctx, tx, le.ID, append([]uuid.UUID{in.CustomerID}, in.OtherCustomers...))
	if err != nil {
		return IncomingPayment{}, err
	}
	applied, err := e.Postings.ApplyReceipt(ctx, app.ledger)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: apply receipt %s to customer %s: %w", receipt.Number, payer.Number, err)
	}
	if err := requireApplied(in, app.ledger.Residuals, applied); err != nil {
		return IncomingPayment{}, err
	}
	number, err := e.Numbers.Next(ctx, le.ID, PaymentNumberRange, year)
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: number an incoming payment: %w", err)
	}
	if err := insertPayment(ctx, tx, p, id, number, in, app, receipt, date, applied); err != nil {
		return IncomingPayment{}, err
	}
	if err := e.recharge(ctx, tx, p, relieved, recharging{payment: id, date: date}); err != nil {
		return IncomingPayment{}, err
	}
	v, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1`, id))
	if err != nil {
		return IncomingPayment{}, fmt.Errorf("receivables: read incoming payment %s: %w", id, err)
	}
	return v, recordApplied(ctx, v, le, names, applied.Entry)
}

// requireApplied refuses what the ledger posted when it is not what the
// request asked for, which rolls the posting back (T75g, T75h).
func requireApplied(in PaymentInput, residuals []ledger.Residual, applied ledger.AppliedReceipt) error {
	if err := requireLeftOnAccount(in, applied); err != nil {
		return err
	}
	if err := requireEveryCustomerPaid(in, applied); err != nil {
		return err
	}
	return requirePartsInRange(in, residuals, applied)
}

// requireLeftOnAccount refuses on_account with nothing left on account.
func requireLeftOnAccount(in PaymentInput, applied ledger.AppliedReceipt) error {
	if in.OnAccount && applied.OnAccount == 0 {
		return problem.Unprocessable("receivables.nothing_left_on_account",
			"The items take the whole receipt: nothing is left to put on the customer's account.",
			problem.FieldError{Field: "on_account", Code: "nothing_left", Message: "false, or fewer items"})
	}
	return nil
}

// requireEveryCustomerPaid refuses an other customer none of whose items
// was applied.
func requireEveryCustomerPaid(in PaymentInput, applied ledger.AppliedReceipt) error {
	paid := map[uuid.UUID]bool{}
	for _, c := range applied.Customers {
		paid[c] = true
	}
	for i, c := range in.OtherCustomers {
		if !paid[c] {
			return problem.Invalid("receivables.customer_without_items", fmt.Sprintf("other_customer_ids[%d]", i),
				"None of the items is an open item of this customer: name only the other customers whose items the receipt pays.")
		}
	}
	return nil
}

// requirePartsInRange refuses a customer's part out of range, which the
// ledger does not check, holding only all the items together to the
// receipt.
func requirePartsInRange(in PaymentInput, residuals []ledger.Residual, applied ledger.AppliedReceipt) error {
	customers, amounts := slices.Clone(applied.Customers), slices.Clone(applied.Items)
	for i, r := range residuals {
		if r.Amount != 0 {
			customers, amounts = append(customers, applied.Customers[i]), append(amounts, -r.Amount)
		}
	}
	for i, d := range applied.Discounts {
		if d != 0 {
			customers, amounts = append(customers, applied.Customers[i]), append(amounts, -d)
		}
	}
	if _, ok := customerParts(in.CustomerID, customers, amounts); !ok {
		return problem.Unprocessable("receivables.amount_too_large", "The items of a customer the payment applies add up out of range.",
			problem.FieldError{Field: "items", Code: "too_large", Message: "items whose total per customer is in range"})
	}
	return nil
}

// payer resolves the legal entity, which the caller must be allowed to
// act in, and the customer and the other customers the receipt pays for
// (T75h), by id, each a business partner, none named twice or the payer
// again. Whether the items are theirs is the ledger's check; a customer
// blocked for posting may still pay what it owes.
func (e *Endpoints) payer(ctx context.Context, in PaymentInput) (tenancy.Entity, map[uuid.UUID]partner.Named, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, nil, fmt.Errorf("receivables: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, nil, fmt.Errorf("receivables: legal entity of an incoming payment: %w", err)
	}
	seen := map[uuid.UUID]bool{in.CustomerID: true}
	for i, c := range in.OtherCustomers {
		if seen[c] {
			return tenancy.Entity{}, nil, problem.Invalid("receivables.duplicate_customer", fmt.Sprintf("other_customer_ids[%d]", i),
				"An other customer is named once, and is not the payer, customer_id.")
		}
		seen[c] = true
	}
	names, err := e.Partners.Names(ctx, append([]uuid.UUID{in.CustomerID}, in.OtherCustomers...))
	if err != nil {
		return tenancy.Entity{}, nil, fmt.Errorf("receivables: customers of an incoming payment: %w", err)
	}
	if _, ok := names[in.CustomerID]; !ok {
		return tenancy.Entity{}, nil, problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	}
	for i, c := range in.OtherCustomers {
		if _, ok := names[c]; !ok {
			return tenancy.Entity{}, nil, problem.ReferenceNotFound(fmt.Sprintf("other_customer_ids[%d]", i), "business partner",
				"no business partner with this id")
		}
	}
	return le, names, nil
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
	// The payment in force: a receipt applied, undone (T75i) and applied
	// again is one cleared line at a time, so one payment at most.
	err = tx.QueryRow(ctx, `SELECT p.document_number FROM receivables.incoming_payment p WHERE p.receipt_entry_id = $1 AND p.legal_entity_id = $2
		AND NOT EXISTS (SELECT FROM receivables.incoming_payment_reset r WHERE r.tenant_id = p.tenant_id AND r.incoming_payment_id = p.id AND r.application_no IS NULL)
		ORDER BY p.created_at DESC LIMIT 1`, entry, le.ID).Scan(&number)
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
// application of the receipt, its residuals read by the company's
// payment-difference rules (differenceRules); the payment's id is the
// clearing entry's source document.
func (e *Endpoints) receiptApplication(ctx context.Context, tx pgx.Tx, le tenancy.Entity, customer partner.Named, in PaymentInput, receipt ledger.Receipt,
	id uuid.UUID) (receiptApplication, error) {
	rules, err := e.differenceRules(ctx, tx, le)
	if err != nil {
		return receiptApplication{}, err
	}
	residuals, err := residualsOf(in.Residuals, len(in.Items), receipt.Currency, receipt.Exponent, rules)
	if err != nil {
		return receiptApplication{}, err
	}
	text := "Incoming payment of customer " + customer.Number
	app := ledger.ReceiptApplication{LegalEntityID: le.ID, DocumentType: PaymentDocumentType, Currency: receipt.Currency, ReceiptAccountID: receipt.AccountID,
		Receipt: receipt.Item, CustomerID: in.CustomerID, OtherCustomers: in.OtherCustomers, Items: in.Items, Residuals: residuals, Under: rules.under, Over: rules.over, HeaderText: &text,
		SourceContext: "receivables", SourceDocumentID: id}
	if in.OnAccount {
		if app.OnAccount, err = e.customerAccount(ctx, in.CustomerID, le); err != nil {
			return receiptApplication{}, err
		}
	}
	if app.CashDiscounts, err = cashDiscounts(le, in.CashDiscounts); err != nil {
		return receiptApplication{}, err
	}
	app.DiscountDate = receipt.PostingDate
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

// writeOffs are the columns of what the items' reasons wrote off (T75o),
// one per item, nil for none.
type writeOffs struct {
	amounts  []*money.Minor
	lines    []*int32
	accounts []*uuid.UUID
	reasons  []*string
	texts    []*string
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
// cleared it at, its residual item or what its reason wrote off (T75o),
// if either, and for an item in another currency its own amount and rate
// (T75n).
func insertPaymentItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, first, application int32, a applying, applied ledger.AppliedReceipt) error {
	n := len(a.items)
	nos, years, entries, lines := make([]int32, n), make([]int16, n), make([]uuid.UUID, n), make([]int32, n)
	amounts, residualLines, reasons, texts := make([]*money.Minor, n), make([]*int32, n), make([]*string, n), make([]*string, n)
	currencies, owns, rates, discounts := make([]*string, n), make([]*money.Minor, n), make([]*string, n), make([]*money.Minor, n)
	off := writeOffs{amounts: make([]*money.Minor, n), lines: make([]*int32, n), accounts: make([]*uuid.UUID, n), reasons: make([]*string, n), texts: make([]*string, n)}
	for i, it := range a.items {
		nos[i], years[i], entries[i], lines[i] = first+int32(i+1), it.FiscalYear, it.EntryID, it.LineNo
		if line := applied.ResidualLines[i]; line != 0 { // a residual the request gave (residualsOf)
			amount := a.residuals[i].Amount
			amounts[i], residualLines[i], reasons[i], texts[i] = &amount, &line, &a.inputs[i].Reason, a.inputs[i].Text
		}
		if line := applied.WriteOffLines[i]; line != 0 { // a residual its reason wrote off (residualsOf, T75o)
			amount, account := a.residuals[i].Amount, a.residuals[i].WriteOff
			off.amounts[i], off.lines[i], off.accounts[i], off.reasons[i], off.texts[i] = &amount, &line, &account, &a.inputs[i].Reason, a.inputs[i].Text
		}
		if tr := applied.Translations[i]; tr.Currency != "" { // translated into the payment's currency (T75n)
			currencies[i], owns[i], rates[i] = &tr.Currency, &tr.Amount, &tr.Rate
		}
		if len(applied.Discounts) != 0 && applied.Discounts[i] != 0 { // the cash discount it took (discount.go)
			discounts[i] = &applied.Discounts[i]
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			residual_amount, residual_line_no, residual_reason, residual_reason_text, application_no, customer_id, item_currency, item_amount, exchange_rate,
			write_off_amount, write_off_line_no, write_off_account_id, write_off_reason, write_off_reason_text, cash_discount)
		SELECT $1, $2, n, y, je, l, a, ra, rl, rr, rt, $12, c, ic, ia, xr, wa, wl, wc, wr, wt, cd
		FROM unnest($3::int[], $4::smallint[], $5::uuid[], $6::int[], $7::bigint[], $8::bigint[], $9::int[], $10::text[], $11::text[], $13::uuid[],
			$14::text[], $15::bigint[], $16::numeric[], $17::bigint[], $18::int[], $19::uuid[], $20::text[], $21::text[], $22::bigint[])
			AS x(n, y, je, l, a, ra, rl, rr, rt, c, ic, ia, xr, wa, wl, wc, wr, wt, cd)`,
		tenant, id, nos, years, entries, lines, applied.Items, amounts, residualLines, reasons, texts, application, applied.Customers,
		currencies, owns, rates, off.amounts, off.lines, off.accounts, off.reasons, off.texts, discounts); err != nil {
		return fmt.Errorf("receivables: write items of incoming payment %s: %w", id, err)
	}
	return nil
}

// CustomerPart is a customer whose items a payment applied with its
// receipt, and its part: those items net of their residuals (T75h).
type CustomerPart struct {
	CustomerID uuid.UUID
	Amount     money.Minor
}

// Parts are the customers whose items the payment applied with its
// receipt, the payer first (its part zero when none of the items was its
// own), then the others in the order of their first item, each with its
// part, net of residuals, of what reasons wrote off (T75o) and of the cash
// discounts taken. The parts, less the difference and with what was left
// on account, are the receipt.
func (v IncomingPayment) Parts() []CustomerPart {
	var customers []uuid.UUID
	var amounts []money.Minor
	for _, it := range v.Items {
		if it.ApplicationNo != 0 {
			continue // applied with a credit on account, the payer's own
		}
		customers, amounts = append(customers, it.CustomerID), append(amounts, it.Amount)
		for _, r := range []*money.Minor{it.ResidualAmount, it.WriteOffAmount, it.CashDiscount} {
			if r != nil {
				customers, amounts = append(customers, it.CustomerID), append(amounts, -*r)
			}
		}
	}
	parts, _ := customerParts(v.CustomerID, customers, amounts) // in range: ApplyPayment refused a part out of range (requireApplied)
	return parts
}

// customerParts adds up the amounts by customer, the payer first, then
// the others in the order they come in; ok is false when a part is out of
// range (money.Sum's only error).
func customerParts(payer uuid.UUID, customers []uuid.UUID, amounts []money.Minor) (parts []CustomerPart, ok bool) {
	order := []uuid.UUID{payer}
	by := map[uuid.UUID][]money.Minor{payer: nil}
	for i, c := range customers {
		if _, ok := by[c]; !ok {
			order = append(order, c)
		}
		by[c] = append(by[c], amounts[i])
	}
	out := make([]CustomerPart, len(order))
	for i, c := range order {
		part, err := money.Sum(by[c]...)
		if err != nil {
			return nil, false
		}
		out[i] = CustomerPart{CustomerID: c, Amount: part}
	}
	return out, true
}

// partsPayload is the customers' parts as an event carries them: each by
// id and number, never by name (product owner, 2026-11-05), with its part.
func (v IncomingPayment) partsPayload(parts []CustomerPart, names map[uuid.UUID]partner.Named) []map[string]any {
	out := make([]map[string]any, len(parts))
	for i, c := range parts {
		out[i] = map[string]any{"customer_id": c.CustomerID, "customer_number": names[c.CustomerID].Number,
			"amount": money.EventAmount(c.Amount, v.Currency, v.Exponent)}
	}
	return out
}

// CashDiscount is what the items the payment applied with its receipt
// took off as cash discounts (discount.go), zero for none.
func (v IncomingPayment) CashDiscount() money.Minor {
	var discounts []money.Minor
	for _, it := range v.Items {
		if it.ApplicationNo == 0 && it.CashDiscount != nil {
			discounts = append(discounts, *it.CashDiscount)
		}
	}
	total, _ := money.Sum(discounts...) // in range: the ledger added them up before it posted them (discountsTaken)
	return total
}

// residualCounts counts the residual items the application with the
// number left (0: the payment's own) and the items its reasons wrote off
// (T75o).
func (v IncomingPayment) residualCounts(application int32) (residuals, writeOffs int) {
	for _, it := range v.Items {
		switch {
		case it.ApplicationNo != application:
		case it.ResidualAmount != nil:
			residuals++
		case it.WriteOffAmount != nil:
			writeOffs++
		}
	}
	return residuals, writeOffs
}

// recordApplied adds the audit record of the application and publishes
// IncomingPaymentApplied in the same transaction (ADR-0007, ADR-0011).
// The event names the customers by id and number, never by name (product
// owner, 2026-11-05), and carries the amount, each customer's part and the
// number of items, not the items.
func recordApplied(ctx context.Context, v IncomingPayment, le tenancy.Entity, names map[uuid.UUID]partner.Named, entry ledger.Entry) error {
	const action = "receivables.incoming_payment.apply"
	amount := money.EventAmount(v.Amount, v.Currency, v.Exponent)
	residuals, writeOffs := v.residualCounts(0)
	customers := v.partsPayload(v.Parts(), names)
	customer := names[v.CustomerID]
	payload := map[string]any{"incoming_payment_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": customer.Number,
		"posting_date": v.PostingDate.Format(time.DateOnly), "amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID,
		"journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber, "item_count": len(v.Items),
		"residual_count": residuals, "difference": money.EventAmount(v.Difference, v.Currency, v.Exponent),
		"on_account_amount": money.EventAmount(v.OnAccount, v.Currency, v.Exponent), "cash_discount": money.EventAmount(v.CashDiscount(), v.Currency, v.Exponent),
		"customers": customers}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "posting_date": v.PostingDate.Format(time.DateOnly),
		"amount": amount, "receipt_journal_entry_id": v.ReceiptEntryID, "journal_entry_id": entry.ID, "journal_entry_number": entry.DocumentNumber,
		"items": len(v.Items), "residuals": residuals, "write_offs": writeOffs, "difference": money.EventAmount(v.Difference, v.Currency, v.Exponent),
		"on_account": money.EventAmount(v.OnAccount, v.Currency, v.Exponent), "cash_discount": money.EventAmount(v.CashDiscount(), v.Currency, v.Exponent),
		"customers": customers}
	if err := audit.Record(ctx, action, "receivables.incoming_payment", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "incoming_payment_applied", AggregateType: "receivables.incoming_payment", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of incoming payment %s: %w", action, v.ID, err)
	}
	return nil
}
