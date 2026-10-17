// Package payments is the payments context (03-data-model §3.10): house
// banks from the configuration document, payment runs (SAP F110) proposed
// as an operation and released or rejected by a person other than the
// proposer, payments posted to the ledger that clear the items they pay,
// and ISO 20022 bank files transmitted through a connector.
//
// payment-runs:propose creates the run in status proposing with its
// operation; the Proposer worker builds it as the principal that proposed
// it: the legal entity's open supplier items in the house bank's currency
// that are due by the run's due date (document date plus the supplier's
// payment terms) and not in another live run, netted per supplier.
// Suppliers that are blocked, have no bank account valid on the run date,
// have a bank change pending or changed bank details within the
// configuration's hold days, or whose items net to a debit, are excluded
// with the reason. The partial unique index on active payment items
// reserves the paid items, and the run reserves them in the ledger too
// (ledger.item_reservation), which refuses to clear or reverse them by hand
// until the run pays or frees them.
//
// :release posts one entry per payment (document type ZP, on the run date)
// that offsets the supplier's items, credits the house bank's account and
// clears the items, then writes the pain.001 bank file. :reject frees the
// items. Bank files are rebuilt from the run when transmitted and checked
// against the hash stored at release, until object storage exists.
package payments

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
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Payment run statuses.
const (
	Proposing = "proposing"
	Proposed  = "proposed"
	Releasing = "releasing"
	Released  = "released"
	Rejected  = "rejected"
	Failed    = "failed"
)

// Bank file statuses.
const (
	FileCreated      = "created"
	FileTransmitted  = "transmitted"
	FileAcknowledged = "acknowledged"
	FileRejected     = "rejected"
)

const (
	// DocumentType is the journal document type of payments (SAP ZP); they
	// draw numbers from the journal_zp number range.
	DocumentType = "ZP"
	// RunNumbers is the number range of payment run document numbers.
	RunNumbers = "payment_run"
	// Format is the ISO 20022 message of bank files.
	Format = "pain.001.001.03"
	// ProposeKind is the operation kind of proposals.
	ProposeKind = "payments.propose"
	// ReleaseKind is the operation of a release: one posting per payment,
	// then the bank file (T27, ADR-0012).
	ReleaseKind = "payments.release"
)

// LegalEntities is what payments reads about legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// OpenItems is what payments reads about open items (ledger context).
type OpenItems interface {
	Payable(ctx context.Context, legalEntity uuid.UUID, currency string, documentDateTo time.Time) ([]ledger.PayableItem, error)
	References(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error)
	Unpayable(ctx context.Context, items []ledger.ItemRef) (map[ledger.ItemRef]string, error)
}

// Postings posts payments and reserves the open items a live run holds
// (ledger context): the reservation makes the ledger refuse to clear or
// reverse them by hand while the run is about to pay them.
type Postings interface {
	Settle(ctx context.Context, in ledger.Settlement) (ledger.Entry, error)
	Reserve(ctx context.Context, owner ledger.Owner, items []ledger.ItemRef) error
	Release(ctx context.Context, owner uuid.UUID, items []ledger.ItemRef) error
}

// Payees is what payments reads about the suppliers it pays (partner context).
type Payees interface {
	Payees(ctx context.Context, legalEntity uuid.UUID, partners []uuid.UUID, on time.Time) (map[uuid.UUID]partner.Payee, error)
	BankDetails(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.BankDetail, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Actors reads who a run's deferred work acts as: its proposer or
// releaser (identity context).
type Actors interface {
	Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error)
}

// Endpoints implements the payments part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	OpenItems     OpenItems
	Postings      Postings
	Payees        Payees
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Actors        Actors
	Keys          *fieldcrypt.Keyring // opens house bank IBANs
	Connector     Connector           // transmits bank files; nil: transmission answers 503
	Objects       blobstore.Store     // keeps bank files as written at release; nil: files are rendered again when needed
	Accounts      Accounts            // the cash discount account a run with discounts posts to
	Now           func() time.Time    // the release's day; nil: time.Now
}

// Accounts is what payments reads about account determination (ledger
// context).
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
}

// Run is a row of payments.payment_run. Amounts are minor units of
// Currency, whose minor-unit digits are Exponent.
type Run struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	HouseBankID    uuid.UUID
	RunDate        time.Time
	DueBy          time.Time
	Currency       string
	Exponent       int
	Total          money.Minor
	Status         string
	ProposedBy     uuid.UUID
	DecidedBy      *uuid.UUID
	DecidedAt      *time.Time
	OperationID    *uuid.UUID // the proposal's operation
	ReleaseOpID    *uuid.UUID // the latest release operation, once a release was asked for
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const runSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.house_bank_id, r.run_date, r.due_by, r.currency, c.exponent, r.total_amount,
		r.status, r.proposed_by, r.decided_by, r.decided_at, r.operation_id, r.release_operation_id, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM payments.payment_run r JOIN ref.currency c ON c.code = r.currency`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.HouseBankID, &r.RunDate, &r.DueBy, &r.Currency, &r.Exponent, &r.Total,
		&r.Status, &r.ProposedBy, &r.DecidedBy, &r.DecidedAt, &r.OperationID, &r.ReleaseOpID, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan payment run")
}

// load reads a run without company access; lock takes the row lock for a
// change.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Run, error) {
	q := runSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRun(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, problem.NotFoundOf("payment run")
	}
	if err != nil {
		return Run{}, fmt.Errorf("payments: read run %s: %w", id, err)
	}
	return r, nil
}

// get reads a run of a legal entity the caller may act in.
func get(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Run, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := load(ctx, tx, id, lock)
	if err == nil && !access.Allows(r.LegalEntityID) {
		return Run{}, problem.NotFoundOf("payment run")
	}
	return r, err
}

// GetRun returns a payment run.
func GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	return get(ctx, tx, id, false)
}

// Item is an open item a payment settles.
type Item struct {
	FiscalYear int16       `json:"fiscal_year"`
	EntryID    uuid.UUID   `json:"journal_entry_id"`
	LineNo     int32       `json:"line_no"`
	Amount     money.Minor `json:"amount"` // paid: what the item owes, so a credit memo is negative
	// Discount is the cash discount taken on the item (T36c), of its sign:
	// the item pays Amount less Discount.
	Discount money.Minor       `json:"discount_amount"`
	DueDate  partner.Date      `json:"due_date"`
	Active   bool              `json:"is_active"`
	terms    *ledger.ItemTerms // the terms it was posted with, while proposing
	until    *time.Time        // the last day of the tier its discount is taken in, while proposing
}

// Payment is a row of payments.payment with its items.
type Payment struct {
	ID               uuid.UUID
	RunID            uuid.UUID
	SupplierID       uuid.UUID
	BankAccountID    uuid.UUID
	Amount           money.Minor // paid, after the items' discounts
	Discount         money.Minor // the items' cash discounts
	DiscountUntil    *time.Time  // the last day they are earned, or nil
	EndToEndID       string
	JournalEntryYear *int16
	JournalEntryID   *uuid.UUID
	Items            []Item
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	Excluded         bool // dropped by the release (dropPayment): the supplier is excluded with the reason
}

// dropped says whether payment p was dropped by its run's release: its
// supplier has an exclusion, which a payment of the run otherwise never
// has (a proposal pays or excludes each supplier). It relies on one payment
// per supplier and run, which UNIQUE (tenant_id, payment_run_id,
// supplier_id) on payments.payment holds (000008).
const dropped = `EXISTS (SELECT FROM payments.payment_exclusion x WHERE x.tenant_id = p.tenant_id AND x.payment_run_id = p.payment_run_id
		AND x.supplier_id = p.supplier_id)`

const paymentSelect = `SELECT p.id, p.payment_run_id, p.supplier_id, p.bank_account_id, p.amount, p.discount_amount, p.discount_until, p.end_to_end_id, p.journal_entry_year,
		p.journal_entry_id, (SELECT json_agg(json_build_object('fiscal_year', i.item_fiscal_year, 'journal_entry_id', i.item_entry_id,
			'line_no', i.item_line_no, 'amount', i.amount, 'discount_amount', i.discount_amount, 'due_date', i.due_date,
			'is_active', i.is_active)
			ORDER BY i.item_fiscal_year, i.item_entry_id, i.item_line_no)
		 FROM payments.payment_item i WHERE i.tenant_id = p.tenant_id AND i.payment_id = p.id), p.created_at, p.created_by,
		` + dropped + ` AS is_dropped
	FROM payments.payment p`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.RunID, &p.SupplierID, &p.BankAccountID, &p.Amount, &p.Discount, &p.DiscountUntil, &p.EndToEndID, &p.JournalEntryYear, &p.JournalEntryID,
		&p.Items, &p.CreatedAt, &p.CreatedBy, &p.Excluded)
	return p, errs.Wrapf(err, "scan payment")
}

// paymentsOf reads the payments a run makes, in end-to-end id order, the
// order of the bank file; payments its release dropped are left out.
func paymentsOf(ctx context.Context, tx pgx.Tx, run uuid.UUID) ([]Payment, error) {
	rows, _ := tx.Query(ctx, paymentSelect+` WHERE p.payment_run_id = $1 AND NOT `+dropped+` ORDER BY p.end_to_end_id`, run)
	pays, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Payment, error) { return scanPayment(r) })
	if err != nil {
		return pays, fmt.Errorf("payments: read the payments of run %s: %w", run, err)
	}
	return pays, nil
}

// Exclusion is a supplier left out of a run.
type Exclusion struct {
	SupplierID uuid.UUID
	Reason     string
	Amount     money.Minor
}

// houseBank is a row of payments.house_bank.
type houseBank struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	Code          string
	IBANCipher    []byte
	BIC           string
	Currency      string
	GLAccountID   uuid.UUID
	Status        string
}

func loadHouseBank(ctx context.Context, tx pgx.Tx, id uuid.UUID) (houseBank, bool, error) {
	var h houseBank
	err := tx.QueryRow(ctx, `SELECT id, legal_entity_id, code, iban_cipher, bic, currency, gl_account_id, status FROM payments.house_bank WHERE id = $1`, id).
		Scan(&h.ID, &h.LegalEntityID, &h.Code, &h.IBANCipher, &h.BIC, &h.Currency, &h.GLAccountID, &h.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("payments: read house bank %s: %w", id, err)
	}
	return h, true, nil
}

// errNoEntity is entityOf's error for a run whose legal entity the tenancy
// context does not find.
var errNoEntity = errors.New("no legal entity")

// entityOf reads the legal entity of run r. A missing one is broken data
// (payment_run.legal_entity_id has no foreign key into tenancy, and legal
// entities are never deleted), so it is errNoEntity, never an entity with
// an empty code that would begin the run's ids and file names with "-".
// Each caller says what errNoEntity means to it.
func (e *Endpoints) entityOf(ctx context.Context, r Run) (tenancy.Entity, error) {
	le, found, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("payments: legal entity %s of run %s: %w", r.LegalEntityID, r.ID, err)
	}
	if !found {
		return tenancy.Entity{}, fmt.Errorf("payments: run %s: %w %s", r.ID, errNoEntity, r.LegalEntityID)
	}
	return le, nil
}

// noEntity is the problem a proposal fails with for entityOf's errNoEntity,
// naming the entity. It has no field error: the entity was there when the
// request was accepted, so no field of the request is wrong.
func noEntity(r Run) *problem.Problem {
	return problem.MissingReference("Legal entity " + r.LegalEntityID.String() + " of payment run " + r.DocumentNumber + " does not exist.")
}

func requireProposed(r Run, done string) error {
	if r.Status != Proposed {
		return problem.Conflict("payments.invalid_status", "Only a proposed payment run can be "+done+"; this one is "+r.Status+".")
	}
	return nil
}

// decider refuses the proposer: whoever proposed a run can never release
// or reject it (the database's payment_run_sod constraint says the same).
func decider(r Run, p reqctx.Principal) error {
	if p.ID == r.ProposedBy {
		return problem.Forbidden("identity.sod_conflict", "Segregation of duties: whoever proposed a payment run can never release or reject it.")
	}
	return nil
}

// record publishes the events and adds the audit entry of a change; the
// action names the object type (payments.payment_run.release).
func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	return audit.Record(ctx, action, audit.ObjectTypeOf(action), id, detail, events...)
}

func runEvent(r Run, name string, payload map[string]any) outbox.Event {
	payload["payment_run_id"], payload["document_number"], payload["legal_entity_id"], payload["status"] = r.ID, r.DocumentNumber, r.LegalEntityID, r.Status
	return outbox.Event{Context: "payments", Name: name, AggregateType: "payments.payment_run", AggregateID: r.ID, Payload: payload}
}

func fileEvent(f BankFile, name string, payload map[string]any) outbox.Event {
	payload["bank_file_id"], payload["payment_run_id"] = f.ID, f.RunID
	return outbox.Event{Context: "payments", Name: name, AggregateType: "payments.bank_file", AggregateID: f.ID, Payload: payload}
}

// Reservations answers whether an entry's open items are in a live payment
// run, for contexts that would otherwise change what the run pays.
type Reservations struct{}

// RunOf returns the document number of the live run that holds an open
// item of the entry of the legal entity: a proposing or proposed run, or a
// releasing one whose payment of the item is not posted yet. A releasing
// run is under way, or stopped (failed or cancelled) with some payments
// posted, to be resumed; the release posts the rest, so their items are as
// reserved as in a proposal. It takes the legal entity's proposals lock
// first (lockProposals), held until the caller's transaction ends: a
// proposal under way is waited for, and none can choose the entry's items
// until the caller (reversing the entry) commits.
func (Reservations) RunOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16, entryID uuid.UUID) (string, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", false, err
	}
	if err := lockProposals(ctx, tx, legalEntity); err != nil {
		return "", false, err
	}
	var number string
	err = tx.QueryRow(ctx, `SELECT r.document_number FROM payments.payment_item i
		JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		WHERE i.item_fiscal_year = $1 AND i.item_entry_id = $2 AND i.is_active
			AND (r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL)) LIMIT 1`,
		fiscalYear, entryID).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return number, false, fmt.Errorf("payments: find the run holding entry %d/%s: %w", fiscalYear, entryID, err)
	}
	return number, true, nil
}
