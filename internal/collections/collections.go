// Package collections is the dunning and collections context (ADR-0065;
// 02-architecture §2.2, 03-data-model §3.27): dunning runs, which propose
// per customer the open items overdue on a key date and the dunning level
// each would reach. A run is a proposal: nothing changes on the items,
// which stay the ledger's, and nothing is sent. Confirming a run records
// each item's level and dunning date, which the next run reads, and
// publishes one event per customer (ADR-0065 §8, T76b). The collection
// worklist, a company's customers with overdue items, is computed on read
// from the same open items and the recorded levels and stores nothing
// (ADR-0065 §9, T76c). A dispute case is a customer's dispute of an
// amount of one of its open items, worked by a person and closed as
// accepted or rejected; closing posts nothing (ADR-0065 As built, T86a).
// A run charges its notices and their items as the company's dunning
// procedure says (T76d), and leaves out the items a dunning block of the
// item or its customer holds back (T76e) and those a customer promised to
// pay by a date on or after its key date (T76f); a promise's status is
// judged on read and shown in the worklist. A collector's contacts with
// a customer are recorded with a resubmission date, which keeps the
// customer off the worklist until then (T76g).
//
// The context is a leaf (ADR-0065 §1): it reads the ledger's customer
// open items, the partner context's names, the identity context's people,
// tenancy and the configuration through the ports below, posts nothing,
// and no context imports it.
package collections

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
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a dunning run: Proposed, its notices are a proposal;
// Confirmed, the proposal was confirmed and its items' levels recorded
// (ADR-0065 §8).
const (
	Proposed  = "proposed"
	Confirmed = "confirmed"
)

// LastKeyDate is the latest key date a request may name (src review:T76a).
// The days an item is overdue are counted through a time.Duration
// (ledger.ReceivableItem.DaysOverdue), which holds about 292 years: with
// key dates up to here, the count is right for every item due since 1809,
// and a key date beyond any use is refused instead of counted wrongly.
var LastKeyDate = time.Date(2100, 12, 31, 0, 0, 0, 0, time.UTC)

// checkKeyDate refuses a key date after LastKeyDate, 422 naming the
// field.
func checkKeyDate(d time.Time) error {
	if d.After(LastKeyDate) {
		return problem.Invalid("collections.invalid_key_date", "key_date", "The key date may be "+LastKeyDate.Format(time.DateOnly)+" at the latest.")
	}
	return nil
}

// MaxRunItems is the most overdue items one run proposes (ADR-0065 §5): a
// run is one synchronous request and one transaction, so its size is
// bounded and a larger one refused; runs as a workflow come later.
const MaxRunItems = 5000

// LegalEntities is what collections reads about legal entities (tenancy
// context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// OpenItems reads a company's open customer items, of the customers named
// or of every customer (ledger context, T75b): every currency, debit
// positive, in the order of customer and item, or one item by its key (a
// dispute case's); and which of some lines are no longer open (Closed: a
// promise to pay's items, T76f). Company access is the caller's to check.
type OpenItems interface {
	Receivable(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID) ([]ledger.ReceivableItem, error)
	ReceivableItem(ctx context.Context, legalEntity uuid.UUID, ref ledger.ItemRef) (ledger.ReceivableItem, bool, error)
	Closed(ctx context.Context, refs []ledger.ItemRef) (map[ledger.ItemRef]bool, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// Settings reads the active configuration: a company's dunning levels
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// People names principals (identity context): a dispute case's assignee
// and who closed it. An id the tenant has no principal of is absent.
type People interface {
	PersonNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Endpoints implements the collections part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	OpenItems     OpenItems
	Partners      Partners
	Settings      Settings
	People        People
	// Now is the clock a dunning block's end date is judged by, against
	// the company's today; nil: the wall clock.
	Now func() time.Time
	// MaxItems is the most overdue items one run proposes; 0: MaxRunItems.
	MaxItems int
}

// Run is a row of collections.dunning_run. OverdueAmount is minor units
// of Currency, the company's, whose minor-unit digits are Exponent.
type Run struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	KeyDate       time.Time
	Status        string
	Currency      string
	Exponent      int
	NoticeCount   int32
	ItemCount     int32
	OverdueAmount money.Minor
	// Disputed are the items the run left out for their open dispute
	// cases (T86b), by customer and item; DisputedLC what their amounts in
	// the run's currency add up to, which total checked when the run was
	// proposed.
	Disputed []DisputedItem
	// Blocked are the items the run left out for a dunning block of the
	// item or its customer (T76e), by customer and item, as Disputed.
	Blocked []BlockedItem
	// Promised are the items the run left out for a promise to pay whose
	// date is on or after its key date (T76f), by customer and item, as
	// Disputed.
	Promised []PromisedItem
	// The company's dunning procedure as the run applied it (T76d): its
	// grace days, minimum amount and annual interest rate (a decimal of
	// three places; nil: none), and what the run charges: its notices'
	// charges and interest, and the notices it left out for being below
	// the minimum amount, by count and overdue amount. Amounts are minor
	// units of Currency.
	GraceDays          int32
	MinimumAmount      money.Minor
	InterestRate       *string
	ChargeAmount       money.Minor
	InterestAmount     money.Minor
	BelowMinimumCount  int32
	BelowMinimumAmount money.Minor
	Version            int32
	CreatedAt          time.Time
	CreatedBy          uuid.UUID
	UpdatedAt          time.Time
	UpdatedBy          uuid.UUID
}

// DisputedLC is what the run's disputed items add up to in its currency.
func (r Run) DisputedLC() money.Minor { return heldLC(heldAmounts(r.Disputed, DisputedItem.held)) }

// BlockedLC is what the run's blocked items add up to in its currency.
func (r Run) BlockedLC() money.Minor { return heldLC(heldAmounts(r.Blocked, BlockedItem.held)) }

// PromisedLC is what the run's promised items add up to in its currency.
func (r Run) PromisedLC() money.Minor { return heldLC(heldAmounts(r.Promised, PromisedItem.held)) }

// heldLC adds up the amounts of items a run held back, which sums
// checked for range when the run was proposed.
func heldLC(amounts []money.Minor) money.Minor {
	sum, _ := money.Sum(amounts...) // in range: sums refused the run otherwise when it was proposed
	return sum
}

// heldAmounts are the amounts in the run's currency of items a run held
// back.
func heldAmounts[T any](items []T, held func(T) HeldItem) []money.Minor {
	amounts := make([]money.Minor, len(items))
	for i, it := range items {
		amounts[i] = held(it).AmountLC
	}
	return amounts
}

// stored is a copy of a run's list as it is written, each item changed by
// strip: a list, empty and not null without any.
func stored[T any](items []T, strip func(*T)) []T {
	out := make([]T, len(items))
	copy(out, items)
	for i := range out {
		strip(&out[i])
	}
	return out
}

// HeldItem is an item a run would have proposed but held back, with what
// the run read of it, as a run's list keeps it (dunning_run.disputed_items,
// T86b; .blocked_items, T76e): Amount is minor units of Currency, the
// item's, whose digits are Exponent (read with the run, not stored);
// AmountLC of the run's currency.
type HeldItem struct {
	CustomerID     uuid.UUID `json:"customer_id"`
	CustomerNumber string    `json:"customer_number"`
	ledger.ItemRef
	DocumentNumber string      `json:"document_number"`
	Reference      *string     `json:"reference"`
	NetDueDate     time.Time   `json:"net_due_date"`
	DaysOverdue    int32       `json:"days_overdue"`
	Currency       string      `json:"currency"`
	Exponent       int         `json:"exponent,omitempty"`
	Amount         money.Minor `json:"amount"`
	AmountLC       money.Minor `json:"amount_lc"`
}

// DisputedItem is an item a run left out for its open dispute case,
// named (T86b).
type DisputedItem struct {
	CaseID uuid.UUID `json:"dispute_case_id"`
	HeldItem
}

func (d DisputedItem) held() HeldItem { return d.HeldItem }

// BlockedItem is an item a run left out for a dunning block of the item
// or its customer, named (T76e).
type BlockedItem struct {
	BlockID uuid.UUID `json:"dunning_block_id"`
	HeldItem
}

func (b BlockedItem) held() HeldItem { return b.HeldItem }

// PromisedItem is an item a run left out for a customer's promise to pay
// it by PromisedDate, named (T76f).
type PromisedItem struct {
	PromiseID    uuid.UUID `json:"promise_to_pay_id"`
	PromisedDate time.Time `json:"promised_date"`
	HeldItem
}

func (p PromisedItem) held() HeldItem { return p.HeldItem }

// Item is a row of collections.dunning_notice_item: a customer's open
// item overdue on its run's key date, as the run read it. Amount is minor
// units of Currency, the item's, whose digits are Exponent; AmountLC of
// the run's currency.
type Item struct {
	ItemNo int32
	ledger.ItemRef
	DocumentNumber string
	Reference      *string
	DocumentDate   time.Time
	NetDueDate     time.Time
	DaysOverdue    int32
	Currency       string
	Exponent       int
	Amount         money.Minor
	AmountLC       money.Minor
	PreviousLevel  int32
	Level          int32
	// InterestAmount is the interest the item is charged at its level, in
	// the run's currency (T76d); 0 at a level that charges none.
	InterestAmount money.Minor
	// PreviousDunnings is how often the item had been dunned when the run
	// proposed it, which a confirmation compares with how often it has
	// been by then (ADR-0065 §8).
	PreviousDunnings int32
}

// Notice is a row of collections.dunning_notice with its items by item
// number: what a run proposes for one customer. OverdueAmount,
// ChargeAmount (its level's charge) and InterestAmount (its items') are
// minor units of its run's currency (T76d).
type Notice struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	Level          int32
	ItemCount      int32
	OverdueAmount  money.Minor
	ChargeAmount   money.Minor
	InterestAmount money.Minor
	Items          []Item
}

// Total is what the notice asks the customer for: its overdue amount, its
// charge and its interest, which the run checked for range when it was
// proposed.
func (n Notice) Total() money.Minor {
	sum, _ := money.Sum(n.OverdueAmount, n.ChargeAmount, n.InterestAmount) // in range: charge refused the run otherwise
	return sum
}

// Total is what the run's notices ask for, as Notice.Total adds it.
func (r Run) Total() money.Minor {
	sum, _ := money.Sum(r.OverdueAmount, r.ChargeAmount, r.InterestAmount) // in range: total refused the run otherwise
	return sum
}

// runSelect reads a run with its disputed, blocked and promised items,
// each given its currency's digits.
const runSelect = `SELECT r.id, r.legal_entity_id, r.key_date, r.status, r.currency, c.exponent, r.notice_count, r.item_count, r.overdue_amount,
		(SELECT coalesce(jsonb_agg(d || jsonb_build_object('exponent', x.exponent) ORDER BY n), '[]')
		 FROM jsonb_array_elements(r.disputed_items) WITH ORDINALITY AS e(d, n) JOIN ref.currency x ON x.code = d->>'currency'),
		(SELECT coalesce(jsonb_agg(d || jsonb_build_object('exponent', x.exponent) ORDER BY n), '[]')
		 FROM jsonb_array_elements(r.blocked_items) WITH ORDINALITY AS e(d, n) JOIN ref.currency x ON x.code = d->>'currency'),
		(SELECT coalesce(jsonb_agg(d || jsonb_build_object('exponent', x.exponent) ORDER BY n), '[]')
		 FROM jsonb_array_elements(r.promised_items) WITH ORDINALITY AS e(d, n) JOIN ref.currency x ON x.code = d->>'currency'),
		r.grace_days, r.minimum_amount, r.interest_rate_percent::text, r.charge_amount, r.interest_amount, r.below_minimum_count,
		r.below_minimum_amount, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM collections.dunning_run r JOIN ref.currency c ON c.code = r.currency`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.KeyDate, &r.Status, &r.Currency, &r.Exponent, &r.NoticeCount, &r.ItemCount, &r.OverdueAmount,
		&r.Disputed, &r.Blocked, &r.Promised, &r.GraceDays, &r.MinimumAmount, &r.InterestRate, &r.ChargeAmount, &r.InterestAmount, &r.BelowMinimumCount,
		&r.BelowMinimumAmount, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan dunning run")
}

// load reads a run whatever its company; GetRun applies company access.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, problem.NotFoundOf("dunning run")
	}
	if err != nil {
		return Run{}, fmt.Errorf("collections: read dunning run %s: %w", id, err)
	}
	return r, nil
}

// GetRun returns a dunning run of a legal entity the caller may act in;
// another's is not found, so ids cannot be probed (04 §4.9).
func GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("collections: company access for dunning run %s: %w", id, err)
	}
	r, err := load(ctx, tx, id)
	if err == nil && !access.Allows(r.LegalEntityID) {
		return Run{}, problem.NotFoundOf("dunning run")
	}
	return r, err
}

const noticeSelect = `SELECT id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount, charge_amount, interest_amount
	FROM collections.dunning_notice`

func scanNotice(r pgx.CollectableRow) (Notice, error) {
	var n Notice
	err := r.Scan(&n.ID, &n.RunID, &n.CustomerID, &n.CustomerNumber, &n.Level, &n.ItemCount, &n.OverdueAmount, &n.ChargeAmount, &n.InterestAmount)
	return n, errs.Wrapf(err, "scan dunning notice")
}

// dunned is what the confirmed runs recorded of an item
// (collections.dunned_item): the highest level it was dunned at, the
// latest dunning date, which the collection worklist shows, and how often
// it was dunned. An item no confirmed run took has none, the zero value:
// level 0, never dunned.
type dunned struct {
	Level int32
	Date  time.Time
	Times int32
}

// recorded reads what the confirmed runs recorded of the items, by item;
// an item never dunned is absent.
func recorded(ctx context.Context, tx pgx.Tx, refs []ledger.ItemRef) (map[ledger.ItemRef]dunned, error) {
	years, entries, lines := ledger.ItemColumns(refs)
	// Read into a map, so the rows' order does not matter.
	rows, _ := tx.Query(ctx, `SELECT d.fiscal_year, d.journal_entry_id, d.line_no, max(d.level), max(d.dunning_date), count(*)
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(fy, je, ln)
		JOIN collections.dunned_item d ON d.fiscal_year = x.fy AND d.journal_entry_id = x.je AND d.line_no = x.ln
		GROUP BY d.fiscal_year, d.journal_entry_id, d.line_no`, years, entries, lines)
	out := map[ledger.ItemRef]dunned{}
	var ref ledger.ItemRef
	var d dunned
	_, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo, &d.Level, &d.Date, &d.Times}, func() error {
		out[ref] = d
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collections: read the dunning levels of %d items: %w", len(refs), err)
	}
	return out, nil
}

// openCase is an item's open dispute case as dunning and the worklist
// read it: its id, its customer's number and what it disputes of the item
// in the company's currency.
type openCase struct {
	ID             uuid.UUID
	CustomerNumber string
	AmountLC       money.Minor
}

// openCases reads the open dispute cases of a company's items, by item,
// each item with what its case disputes of it (T86c); an item without one
// is absent. An item is in one open case at most
// (dispute_case_item_one_open); the read uses the index of a company's
// open cases (dispute_case_open_by_company) and their items by key. A
// case whose item was cleared is read too: the caller matches the cases
// with the open items it holds.
func openCases(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) (map[ledger.ItemRef]openCase, error) {
	rows, _ := tx.Query(ctx, `SELECT i.fiscal_year, i.journal_entry_id, i.line_no, d.id, d.customer_number, i.disputed_amount_lc
		FROM collections.dispute_case d JOIN collections.dispute_case_item i ON i.tenant_id = d.tenant_id AND i.dispute_case_id = d.id
		WHERE d.legal_entity_id = $1 AND d.status = 'open'`, legalEntity)
	out := map[ledger.ItemRef]openCase{}
	var ref ledger.ItemRef
	var c openCase
	_, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo, &c.ID, &c.CustomerNumber, &c.AmountLC}, func() error {
		out[ref] = c
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collections: read the open dispute cases of company %s: %w", legalEntity, err)
	}
	return out, nil
}

// items reads the items of the notices, each notice's by item number, and
// gives them to their notices.
func items(ctx context.Context, tx pgx.Tx, notices []Notice) error {
	at := make(map[uuid.UUID]int, len(notices))
	ids := make([]uuid.UUID, len(notices))
	for i, n := range notices {
		at[n.ID], ids[i] = i, n.ID
	}
	rows, _ := tx.Query(ctx, `SELECT i.dunning_notice_id, i.item_no, i.fiscal_year, i.journal_entry_id, i.line_no, i.document_number, i.reference,
			i.document_date, i.net_due_date, i.days_overdue, i.currency, c.exponent, i.amount, i.amount_lc, i.previous_level, i.level,
			i.previous_dunnings, i.interest_amount
		FROM collections.dunning_notice_item i JOIN ref.currency c ON c.code = i.currency
		WHERE i.dunning_notice_id = ANY($1) ORDER BY i.dunning_notice_id, i.item_no`, ids)
	var notice uuid.UUID
	var it Item
	_, err := pgx.ForEachRow(rows, []any{&notice, &it.ItemNo, &it.FiscalYear, &it.EntryID, &it.LineNo, &it.DocumentNumber, &it.Reference,
		&it.DocumentDate, &it.NetDueDate, &it.DaysOverdue, &it.Currency, &it.Exponent, &it.Amount, &it.AmountLC, &it.PreviousLevel, &it.Level,
		&it.PreviousDunnings, &it.InterestAmount},
		func() error {
			n := &notices[at[notice]]
			n.Items = append(n.Items, it)
			return nil
		})
	if err != nil {
		return fmt.Errorf("collections: read the items of %d dunning notices: %w", len(notices), err)
	}
	return nil
}
