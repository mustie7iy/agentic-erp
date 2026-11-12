// Package collections is the dunning and collections context (ADR-0065;
// 02-architecture §2.2, 03-data-model §3.27): dunning runs, which propose
// per customer the open items overdue on a key date and the dunning level
// each would reach. A run is a proposal: nothing changes on the items,
// which stay the ledger's, and nothing is sent. Confirming a run records
// each item's level and dunning date, which the next run reads, and
// publishes one event per customer (ADR-0065 §8, T76b). The collection
// worklist follows in T76c (ADR-0065 §9).
//
// The context is a leaf (ADR-0065 §1): it reads the ledger's customer
// open items, the partner context's names, tenancy and the configuration
// through the ports below, posts nothing, and no context imports it.
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
// positive, in the order of customer and item. Company access is the
// caller's to check.
type OpenItems interface {
	Receivable(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID) ([]ledger.ReceivableItem, error)
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

// Endpoints implements the collections part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	OpenItems     OpenItems
	Partners      Partners
	Settings      Settings
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
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
}

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
	// PreviousDunnings is how often the item had been dunned when the run
	// proposed it, which a confirmation compares with how often it has
	// been by then (ADR-0065 §8).
	PreviousDunnings int32
}

// Notice is a row of collections.dunning_notice with its items by item
// number: what a run proposes for one customer. OverdueAmount is minor
// units of its run's currency.
type Notice struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	Level          int32
	ItemCount      int32
	OverdueAmount  money.Minor
	Items          []Item
}

const runSelect = `SELECT r.id, r.legal_entity_id, r.key_date, r.status, r.currency, c.exponent, r.notice_count, r.item_count, r.overdue_amount,
		r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM collections.dunning_run r JOIN ref.currency c ON c.code = r.currency`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.KeyDate, &r.Status, &r.Currency, &r.Exponent, &r.NoticeCount, &r.ItemCount, &r.OverdueAmount,
		&r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
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

const noticeSelect = `SELECT id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount FROM collections.dunning_notice`

func scanNotice(r pgx.CollectableRow) (Notice, error) {
	var n Notice
	err := r.Scan(&n.ID, &n.RunID, &n.CustomerID, &n.CustomerNumber, &n.Level, &n.ItemCount, &n.OverdueAmount)
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
	years, entries, lines := make([]int16, len(refs)), make([]uuid.UUID, len(refs)), make([]int32, len(refs))
	for i, r := range refs {
		years[i], entries[i], lines[i] = r.FiscalYear, r.EntryID, r.LineNo
	}
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
			i.previous_dunnings
		FROM collections.dunning_notice_item i JOIN ref.currency c ON c.code = i.currency
		WHERE i.dunning_notice_id = ANY($1) ORDER BY i.dunning_notice_id, i.item_no`, ids)
	var notice uuid.UUID
	var it Item
	_, err := pgx.ForEachRow(rows, []any{&notice, &it.ItemNo, &it.FiscalYear, &it.EntryID, &it.LineNo, &it.DocumentNumber, &it.Reference,
		&it.DocumentDate, &it.NetDueDate, &it.DaysOverdue, &it.Currency, &it.Exponent, &it.Amount, &it.AmountLC, &it.PreviousLevel, &it.Level,
		&it.PreviousDunnings},
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
