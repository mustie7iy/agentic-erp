// Package payments is the payments context (03-data-model §3.10): house
// banks from the configuration document, payment runs (SAP F110) proposed
// as an operation and released or rejected by a person other than the
// proposer, payments posted to the ledger that clear the items they pay,
// and ISO 20022 bank files transmitted through a connector. A run also
// pays due down payment requests (ADR-0037), each a payment of its own,
// whose release posts the down payment as a special G/L item (T46c).
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
// A run whose total, in its company's currency at the run date's rate, is
// above the company's release_dual_limit is released only after a second
// person approved its release (dualrelease.go, ADR-0038).
//
// :release posts one entry per payment (document type ZP, on the run date)
// that offsets the supplier's items, credits the house bank's account and
// clears the items, then writes the pain.001 bank file. :reject frees the
// items. Bank files are rebuilt from the run when transmitted and checked
// against the hash stored at release, until object storage exists.
//
// Bank statements (ADR-0047) are imported from the bank's camt.053 or MT940
// file per house bank, once per file and statement, each compared with the
// bank's statement before it (statements.go); their lines are matched by
// the company's rules (matching.go), decided by people (matches.go),
// proposed for people by the reconciliation agent (proposals.go) and
// posted (posting.go).
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
	FileToSend       = "to_send" // asked for, waiting for the transmitter (T44e)
	FileSending      = "sending" // claimed by the transmitter, the upload under way
	FileFailed       = "failed"  // refused, or out of attempts; may be asked for again
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
	// TransmitKind is the operation of a bank file's transmission, which
	// the transmitter completes (T44e).
	TransmitKind = "payments.transmit"
)

// LegalEntities is what payments reads about legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// OpenItems is what payments reads about open items (ledger context).
type OpenItems interface {
	Payable(ctx context.Context, legalEntity uuid.UUID, currency string, documentDateTo time.Time) ([]ledger.PayableItem, error)
	References(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error)
	Numbers(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error) // what a payment's item shows beside its entry's id
	Unpayable(ctx context.Context, items []ledger.ItemRef) (map[ledger.ItemRef]string, error)
}

// Postings posts payments and reserves the open items a live run holds
// (ledger context): the reservation makes the ledger refuse to clear or
// reverse them by hand while the run is about to pay them. It posts
// matched bank statement lines too.
type Postings interface {
	Settle(ctx context.Context, in ledger.Settlement) (ledger.Entry, error)
	PayDownPayment(ctx context.Context, in ledger.DownPaymentPayment) (ledger.Entry, error)
	Reserve(ctx context.Context, owner ledger.Owner, items []ledger.ItemRef) error
	Release(ctx context.Context, owner uuid.UUID, items []ledger.ItemRef) error
	PostBank(ctx context.Context, in ledger.BankPosting) (ledger.Entry, error) // a matched bank statement line (T49f)
	Postable(ctx context.Context, legalEntity uuid.UUID, date time.Time) error // its date, before it waits for approval
}

// Payees is what payments reads about the suppliers it pays (partner context).
type Payees interface {
	Payees(ctx context.Context, legalEntity uuid.UUID, partners []uuid.UUID, on time.Time) (map[uuid.UUID]partner.Payee, error)
	BankDetails(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.BankDetail, error)
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)                     // what a payment shows beside its supplier's id
	BankAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.BankAccount, error) // masked: what a payment shows beside its bank account's id
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

// Roles answers who holds a role (identity context): the release step is
// decided by holders of payments.release_role alone.
type Roles interface {
	RoleByCode(ctx context.Context, code string) (uuid.UUID, bool, error)
	HoldsRole(ctx context.Context, principal, role uuid.UUID) (bool, error)
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
	Roles         Roles               // who may decide the release step (payments.release_role)
	Keys          *fieldcrypt.Keyring // opens house bank IBANs
	Connector     Connector           // transmits the files of house banks without a channel; nil: those answer 503
	Channels      BankChannels        // house banks' own channels (T44b)
	Objects       blobstore.Store     // keeps bank files as written at release (nil: rendered again when needed) and statement files (nil: none imported)
	Retention     time.Duration       // how long statement files are locked against deletion (ERP_OBJECT_RETENTION); 0: not locked
	Accounts      Accounts            // the cash discount account a run with discounts posts to
	DownPayments  DownPayments        // the down payment requests runs pay (T46c)
	Approvals     Approvals           // the approval of statement lines' postings (T49f) and of large runs' releases (ADR-0038)
	Sessions      Sessions            // the agent sessions proposals are made in (T49g)
	Now           func() time.Time    // the release's day; nil: time.Now
}

// DownPayments is what payments reads and records about down payment
// requests (payables context, through the adapter in internal/app, so
// neither context imports the other): the open requests due by a date,
// one request, the requests' document numbers, and that a released run's
// payment paid it.
type DownPayments interface {
	Due(ctx context.Context, legalEntity uuid.UUID, currency string, by time.Time) ([]DownPayment, error)
	Request(ctx context.Context, id uuid.UUID) (DownPayment, error)
	Numbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) // what a payment or an exclusion shows beside its request's id
	Paid(ctx context.Context, id uuid.UUID, fiscalYear int16, entryID uuid.UUID) error
}

// DownPayment is a down payment request as a run pays it: Amount, tax
// included, in the request's currency.
type DownPayment struct {
	ID, SupplierID uuid.UUID
	DocumentNumber string
	Reference      *string
	Amount         money.Minor
	TaxCode        *string
	Tax            money.Minor
	DueDate        time.Time
}

// Accounts is what payments reads about account determination (ledger
// context).
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
	Chart(ctx context.Context, legalEntity uuid.UUID) (map[string]ledger.ChartAccount, error) // a posting rule's account (T49f)
	// Balances are the main bank account's on statements' closing dates
	// (T49h), in the house bank's currency (T53a).
	Balances(ctx context.Context, legalEntity, account uuid.UUID, currency string, dates []time.Time) ([]money.Minor, error)
}

// Run is a row of payments.payment_run. Amounts are minor units of
// Currency, whose minor-unit digits are Exponent.
type Run struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	HouseBankID    *uuid.UUID // the bank of a run proposed for one; nil when each payment names its own (T45c)
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
	// Release is the latest request for a second person's approval of the
	// release (ADR-0038), or nil when none was asked for.
	Release   *ReleaseApproval
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

const runSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.house_bank_id, r.run_date, r.due_by, r.currency, c.exponent, r.total_amount,
		r.status, r.proposed_by, r.decided_by, r.decided_at, r.operation_id, r.release_operation_id, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by,
		r.release_request_id, r.release_approval, r.release_request_total, r.release_approved_by, r.release_approved_at
	FROM payments.payment_run r JOIN ref.currency c ON c.code = r.currency`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var a ReleaseApproval
	var request *uuid.UUID
	var status *string
	var total *money.Minor
	err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.HouseBankID, &r.RunDate, &r.DueBy, &r.Currency, &r.Exponent, &r.Total,
		&r.Status, &r.ProposedBy, &r.DecidedBy, &r.DecidedAt, &r.OperationID, &r.ReleaseOpID, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy,
		&request, &status, &total, &a.ApprovedBy, &a.ApprovedAt)
	if err == nil && request != nil { // the three are set together (payment_run_release_request)
		a.RequestID, a.Status, a.Total = *request, *status, *total
		r.Release = &a
	}
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
	Discount money.Minor `json:"discount_amount"`
	// Withholding is the withholding tax deducted from the item (T46f), of
	// its sign: the item pays Amount less Discount less Withholding.
	Withholding money.Minor          `json:"withholding_amount"`
	DueDate     partner.Date         `json:"due_date"`
	Active      bool                 `json:"is_active"`
	terms       *ledger.ItemTerms    // the terms it was posted with, while proposing
	withholding []ledger.Withholding // its withholding taxes deducted at payment, while proposing
	until       *time.Time           // the last day of the tier its discount is taken in, while proposing
}

// Payment is a row of payments.payment with its items.
type Payment struct {
	ID               uuid.UUID
	RunID            uuid.UUID
	SupplierID       uuid.UUID
	BankAccountID    uuid.UUID
	Amount           money.Minor // paid, after the items' discounts and withholding
	Discount         money.Minor // the items' cash discounts
	Withholding      money.Minor // the withholding taxes deducted from the items (T46f)
	DiscountUntil    *time.Time  // the last day they are earned, or nil
	EndToEndID       string
	JournalEntryYear *int16
	JournalEntryID   *uuid.UUID
	Items            []Item
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	Excluded         bool       // dropped by the release (dropPayment): the supplier is excluded with the reason
	Kind             string     // KindItems or KindDownPayment (T46c)
	RequestID        *uuid.UUID // the down payment request a KindDownPayment payment pays
	// HouseBankID and Method are the house bank that pays it and the
	// payment method, in a run proposed without a house bank (T45c); nil
	// in one proposed for a bank, whose bank pays it (payingBank).
	HouseBankID *uuid.UUID
	Method      *string
}

// Payment kinds: a supplier's open items, netted, or one down payment
// request (T46c, ADR-0037), paid whole and never netted. The SQL here
// names them as literals too ('items', 'down_payment'), the values of
// payments.payment.kind's check (000080).
const (
	KindItems       = "items"
	KindDownPayment = "down_payment"
)

// dropped says whether payment p was dropped by its run's release: a
// payment of open items whose supplier has an exclusion, which such a
// payment otherwise never has (a proposal pays or excludes each supplier's
// items). It relies on one items payment per supplier and run, which the
// unique index payment_items_once holds (000083). A down payment's payment
// is never dropped: the release drops payments whose items are no longer
// open, and it has none; it is paid even when the supplier's items are
// excluded for a debit balance (payRequests). A request left out has its
// exclusion in payments.request_exclusion, which leaves the items paid.
const dropped = `(p.kind = 'items' AND EXISTS (SELECT FROM payments.payment_exclusion x WHERE x.tenant_id = p.tenant_id
		AND x.payment_run_id = p.payment_run_id AND x.supplier_id = p.supplier_id))`

const paymentSelect = `SELECT p.id, p.payment_run_id, p.supplier_id, p.bank_account_id, p.amount, p.discount_amount, p.withholding_amount, p.discount_until, p.end_to_end_id, p.journal_entry_year,
		p.journal_entry_id, (SELECT json_agg(json_build_object('fiscal_year', i.item_fiscal_year, 'journal_entry_id', i.item_entry_id,
			'line_no', i.item_line_no, 'amount', i.amount, 'discount_amount', i.discount_amount, 'withholding_amount', i.withholding_amount, 'due_date', i.due_date,
			'is_active', i.is_active)
			ORDER BY i.item_fiscal_year, i.item_entry_id, i.item_line_no)
		 FROM payments.payment_item i WHERE i.tenant_id = p.tenant_id AND i.payment_id = p.id), p.created_at, p.created_by,
		` + dropped + ` AS is_dropped, p.kind, p.down_payment_request_id, p.house_bank_id, p.payment_method
	FROM payments.payment p`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.RunID, &p.SupplierID, &p.BankAccountID, &p.Amount, &p.Discount, &p.Withholding, &p.DiscountUntil, &p.EndToEndID, &p.JournalEntryYear, &p.JournalEntryID,
		&p.Items, &p.CreatedAt, &p.CreatedBy, &p.Excluded, &p.Kind, &p.RequestID, &p.HouseBankID, &p.Method)
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
	ID         uuid.UUID // the supplier, or the request of a request's exclusion: the listing's key
	SupplierID uuid.UUID
	RequestID  *uuid.UUID // the down payment request left out, or nil for the supplier's items
	Reason     string
	Amount     money.Minor
}

// houseBank is a row of payments.house_bank.
type houseBank struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	Code          string
	IBANCipher    []byte // the IBAN, or a Canadian bank's account number
	BIC           string
	Currency      string
	GLAccountID   uuid.UUID
	Status        string
	// Institution and Transit are a Canadian house bank's routing (T45d),
	// empty for an IBAN.
	Institution, Transit string
}

func loadHouseBank(ctx context.Context, tx pgx.Tx, id uuid.UUID) (houseBank, bool, error) {
	var h houseBank
	err := tx.QueryRow(ctx, `SELECT id, legal_entity_id, code, iban_cipher, bic, currency, gl_account_id, status,
		coalesce(institution_number, ''), coalesce(transit_number, '') FROM payments.house_bank WHERE id = $1`, id).
		Scan(&h.ID, &h.LegalEntityID, &h.Code, &h.IBANCipher, &h.BIC, &h.Currency, &h.GLAccountID, &h.Status, &h.Institution, &h.Transit)
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

// decider is the control on the release step (:release and :reject):
// whoever proposed a run can never decide it (the database's
// payment_run_sod constraint says the same), and the decider holds the
// active configuration's payments.release_role today. A release
// (release = true) is refused while the configuration asks for more than
// one releaser (payments.release_approvals): one person releases a run, a
// second approving the release of a large one first (release_dual_limit,
// dualrelease.go), and one person's release must not stand in for several.
func (e *Endpoints) decider(ctx context.Context, r Run, p reqctx.Principal, release bool) error {
	if p.ID == r.ProposedBy {
		return problem.Forbidden("identity.sod_conflict", "Segregation of duties: whoever proposed a payment run can never release or reject it.")
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return err
	}
	if n := doc.Payments.ReleaseApprovals; release && n > 1 {
		return problem.Unprocessable("payments.release_approvals_unsupported",
			fmt.Sprintf("The configuration asks for %d people to release a payment run (payments.release_approvals), and one person releases a run; set it to 1, and companies[].release_dual_limit for a second person's approval of large runs.", n))
	}
	code := doc.Payments.ReleaseRole
	role, ok, err := e.Roles.RoleByCode(ctx, code)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Unprocessable("workflow.unknown_role",
			fmt.Sprintf("The configuration's payments.release_role names role %s, which is not an active role; create it or change the configuration.", code))
	}
	holds, err := e.Roles.HoldsRole(ctx, p.ID, role)
	if err != nil {
		return err
	}
	if !holds {
		return problem.Forbidden("payments.release_role_missing", fmt.Sprintf("Releasing or rejecting a payment run needs role %s (payments.release_role).", code))
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
			AND `+liveRun+` LIMIT 1`,
		fiscalYear, entryID).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return number, false, fmt.Errorf("payments: find the run holding entry %d/%s: %w", fiscalYear, entryID, err)
	}
	return number, true, nil
}

// liveRun is when run r holding payment p still holds what p pays: the
// run is proposing or proposed, or releasing with p not posted yet (a
// release under way, or stopped with some payments posted, to be resumed).
const liveRun = `(r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL))`

// RequestRunOf returns the document number of the live run that pays the
// legal entity's down payment request (liveRun). It takes the legal
// entity's proposals lock first, held until the caller's transaction ends,
// so a proposal under way is waited for and none chooses the request until
// the caller (cancelling it) commits.
func (Reservations) RequestRunOf(ctx context.Context, legalEntity, request uuid.UUID) (string, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", false, err
	}
	if err := lockProposals(ctx, tx, legalEntity); err != nil {
		return "", false, err
	}
	var number string
	err = tx.QueryRow(ctx, `SELECT r.document_number FROM payments.payment p
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		WHERE p.kind = 'down_payment' AND p.down_payment_request_id = $1 AND `+liveRun+` LIMIT 1`, request).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return number, false, fmt.Errorf("payments: find the run paying down payment request %s: %w", request, err)
	}
	return number, true, nil
}
