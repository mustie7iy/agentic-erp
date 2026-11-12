package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// errTooLarge refuses a run whose amounts are out of range.
func errTooLarge() *problem.Problem {
	return problem.Unprocessable("collections.amount_too_large", "The overdue items add up to more than an amount can hold.")
}

// reach is the dunning level an item days overdue would reach in a run
// (ADR-0065 §4): the last of the levels whose days overdue it has
// reached, one above the level it was last dunned at (previous) at most,
// and never below that. 0: the item is not dunned, being overdue by fewer
// days than the first level asks for and never dunned before.
func reach(levels []config.DunningLevel, days, previous int) int {
	byDays := 0
	for _, l := range levels { // validated: numbered from 1, their days ascending
		if days >= l.DaysOverdue {
			byDays = l.Level
		}
	}
	return max(previous, min(byDays, previous+1))
}

// RunInput is a dunning run as a client starts it.
type RunInput struct {
	LegalEntityID uuid.UUID
	KeyDate       time.Time
}

// ProposeRun starts a dunning run for a company at a key date (ADR-0065
// §3-§5, T76a): it reads the company's open customer items from the
// ledger and stores, per customer, a notice with the items overdue on the
// key date and the level each would reach, then publishes
// DunningRunChanged with its audit record. Nothing changes on the items.
// A company without dunning levels is refused, 422
// collections.no_dunning_levels; a run of more items than one request
// proposes, 422 collections.run_too_large.
func (e *Endpoints) ProposeRun(ctx context.Context, in RunInput) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	if err := checkKeyDate(in.KeyDate); err != nil {
		return Run{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("collections: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Run{}, fmt.Errorf("collections: legal entity of a dunning run: %w", err)
	}
	levels, err := e.levels(ctx, le)
	if err != nil {
		return Run{}, err
	}
	open, err := e.OpenItems.Receivable(ctx, le.ID, nil)
	if err != nil {
		return Run{}, fmt.Errorf("collections: open customer items of company %s: %w", le.Code, err)
	}
	due := overdue(open, in.KeyDate)
	previous, err := recorded(ctx, tx, refsOf(due))
	if err != nil {
		return Run{}, err
	}
	notices := propose(due, levels, previous)
	run := Run{ID: uuid.Must(uuid.NewV7()), LegalEntityID: le.ID, KeyDate: in.KeyDate}
	if err := e.total(ctx, &run, notices); err != nil {
		return Run{}, err
	}
	if err := insertRun(ctx, tx, p, le, run, notices); err != nil {
		return Run{}, err
	}
	stored, err := load(ctx, tx, run.ID)
	if err != nil {
		return Run{}, err
	}
	return stored, record(ctx, "collections.dunning_run.propose", stored, le, nil)
}

// levels are the company's dunning levels in the active configuration; a
// company without any has no dunning and its run is refused.
func (e *Endpoints) levels(ctx context.Context, le tenancy.Entity) ([]config.DunningLevel, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: dunning levels of company %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	if co == nil || len(co.Dunning.Levels) == 0 {
		return nil, problem.Unprocessable("collections.no_dunning_levels", "Company "+le.Code+
			" has no dunning levels in the configuration (companies[].dunning.levels), so it has no dunning.")
	}
	return co.Dunning.Levels, nil
}

// overdueItem is an open item its customer owes that is past its net due
// date on a key date, by days.
type overdueItem struct {
	ledger.ReceivableItem
	days int
}

// overdue are, of the open items, in their order, those overdue on the
// key date: the ones the customer owes (a debit: credit items are not
// dunned and reduce nothing) that were posted with payment terms (an item
// without has no due date) and are past their net due date by a day at
// least.
func overdue(open []ledger.ReceivableItem, keyDate time.Time) []overdueItem {
	var out []overdueItem
	for _, it := range open {
		if days, ok := it.DaysOverdue(keyDate); ok && days > 0 && it.Amount > 0 {
			out = append(out, overdueItem{ReceivableItem: it, days: days})
		}
	}
	return out
}

// refsOf names the items by their journal lines.
func refsOf(items []overdueItem) []ledger.ItemRef {
	refs := make([]ledger.ItemRef, len(items))
	for i, it := range items {
		refs[i] = it.ItemRef
	}
	return refs
}

// propose is the proposal of a run: of the overdue items, in the ledger's
// order of customer and item, those that reach a dunning level, each from
// the level the confirmed runs recorded for it (previous; none: never
// dunned, level 0), one notice per customer in that order, its items
// numbered from 1. An item dunned before is proposed at its level again
// even when its days no longer reach it at an earlier key date, as long
// as it is overdue then: a level never falls. The notices' ids, numbers
// and totals are total's.
func propose(due []overdueItem, levels []config.DunningLevel, previous map[ledger.ItemRef]dunned) []Notice {
	var notices []Notice
	for _, it := range due {
		before := previous[it.ItemRef].Level
		level := reach(levels, it.days, int(before))
		if level == 0 {
			continue
		}
		if n := len(notices); n == 0 || notices[n-1].CustomerID != it.PartnerID {
			notices = append(notices, Notice{CustomerID: it.PartnerID})
		}
		n := &notices[len(notices)-1]
		n.Items = append(n.Items, Item{ItemNo: int32(len(n.Items) + 1), ItemRef: it.ItemRef, DocumentNumber: it.Number, Reference: it.Reference,
			DocumentDate: it.DocumentDate, NetDueDate: it.Terms.NetDueDate(), DaysOverdue: int32(it.days), Currency: it.Currency, Amount: it.Amount,
			AmountLC: it.AmountLC, PreviousLevel: before, Level: int32(level)})
	}
	return notices
}

// total completes the proposal: each notice's id, its customer's number,
// its level (the highest among its items) and its items' count and
// amount in the company's currency, and the run's totals over them. More
// items than a run proposes are refused, as are amounts out of range.
func (e *Endpoints) total(ctx context.Context, run *Run, notices []Notice) error {
	customers := make([]uuid.UUID, len(notices))
	amounts := make([]money.Minor, len(notices))
	limit := e.MaxItems
	if limit == 0 {
		limit = MaxRunItems
	}
	for i := range notices {
		n := &notices[i]
		n.ID, n.RunID, n.ItemCount, customers[i] = uuid.Must(uuid.NewV7()), run.ID, int32(len(n.Items)), n.CustomerID
		local := make([]money.Minor, len(n.Items))
		for j, it := range n.Items {
			n.Level, local[j] = max(n.Level, it.Level), it.AmountLC
		}
		var err error
		if n.OverdueAmount, err = money.Sum(local...); err != nil { // money.Sum's only error is the range
			return errTooLarge()
		}
		amounts[i] = n.OverdueAmount
		if run.ItemCount += n.ItemCount; int(run.ItemCount) > limit {
			return problem.Unprocessable("collections.run_too_large", fmt.Sprintf("The run would propose more than %d overdue items, "+
				"the most one run proposes; larger runs are not supported yet.", limit))
		}
	}
	run.NoticeCount = int32(len(notices))
	var err error
	if run.OverdueAmount, err = money.Sum(amounts...); err != nil {
		return errTooLarge()
	}
	names, err := e.Partners.Names(ctx, customers)
	if err != nil {
		return fmt.Errorf("collections: customers of %d dunning notices: %w", len(notices), err)
	}
	for i := range notices {
		named, ok := names[notices[i].CustomerID]
		if !ok { // the ledger posts customer items to business partners only, and none is deleted
			return fmt.Errorf("collections: customer %s of an open item is no business partner", notices[i].CustomerID)
		}
		notices[i].CustomerNumber = named.Number
	}
	return nil
}

// insertRun writes the run with its notices and their items, in the
// company's currency.
func insertRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, run Run, notices []Notice) error {
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count,
			item_count, overdue_amount, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
		p.TenantID, run.ID, run.LegalEntityID, run.KeyDate, Proposed, le.LocalCurrency, run.NoticeCount, run.ItemCount, run.OverdueAmount, p.ID); err != nil {
		return fmt.Errorf("collections: insert dunning run %s: %w", run.ID, err)
	}
	n := len(notices)
	ids, customers, numbers := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]string, n)
	levels, counts, amounts := make([]int32, n), make([]int32, n), make([]money.Minor, n)
	var all []Item
	var of []uuid.UUID
	for i, nt := range notices {
		ids[i], customers[i], numbers[i], levels[i], counts[i], amounts[i] = nt.ID, nt.CustomerID, nt.CustomerNumber, nt.Level, nt.ItemCount, nt.OverdueAmount
		for _, it := range nt.Items {
			all, of = append(all, it), append(of, nt.ID)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount)
		SELECT $1, i, $2, c, n, l, k, a FROM unnest($3::uuid[], $4::uuid[], $5::text[], $6::int[], $7::int[], $8::bigint[]) AS x(i, c, n, l, k, a)`,
		p.TenantID, run.ID, ids, customers, numbers, levels, counts, amounts); err != nil {
		return fmt.Errorf("collections: write the notices of dunning run %s: %w", run.ID, err)
	}
	return insertItems(ctx, tx, p.TenantID, run.ID, of, all)
}

// insertItems writes a run's items, each with the notice it is of.
func insertItems(ctx context.Context, tx pgx.Tx, tenant, run uuid.UUID, of []uuid.UUID, items []Item) error {
	n := len(items)
	nos, years, entries, lines := make([]int32, n), make([]int16, n), make([]uuid.UUID, n), make([]int32, n)
	numbers, references, dates, dues := make([]string, n), make([]*string, n), make([]time.Time, n), make([]time.Time, n)
	days, currencies, amounts, local := make([]int32, n), make([]string, n), make([]money.Minor, n), make([]money.Minor, n)
	previous, levels := make([]int32, n), make([]int32, n)
	for i, it := range items {
		nos[i], years[i], entries[i], lines[i] = it.ItemNo, it.FiscalYear, it.EntryID, it.LineNo
		numbers[i], references[i], dates[i], dues[i] = it.DocumentNumber, it.Reference, it.DocumentDate, it.NetDueDate
		days[i], currencies[i], amounts[i], local[i] = it.DaysOverdue, it.Currency, it.Amount, it.AmountLC
		previous[i], levels[i] = it.PreviousLevel, it.Level
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year,
			journal_entry_id, line_no, document_number, reference, document_date, net_due_date, days_overdue, currency, amount, amount_lc,
			previous_level, level)
		SELECT $1, $2, nt, no, fy, je, ln, dn, rf, dd, du, dy, cu, am, lc, pl, lv
		FROM unnest($3::uuid[], $4::int[], $5::smallint[], $6::uuid[], $7::int[], $8::text[], $9::text[], $10::date[], $11::date[], $12::int[],
			$13::text[], $14::bigint[], $15::bigint[], $16::int[], $17::int[])
			AS x(nt, no, fy, je, ln, dn, rf, dd, du, dy, cu, am, lc, pl, lv)`,
		tenant, run, of, nos, years, entries, lines, numbers, references, dates, dues, days, currencies, amounts, local, previous, levels); err != nil {
		return fmt.Errorf("collections: write the items of dunning run %s: %w", run, err)
	}
	return nil
}

// record adds the audit record of a change of a run's status and
// publishes DunningRunChanged in the same transaction (ADR-0007,
// ADR-0011), followed by the events the change issues. before is the
// run's status before, nil for a run just started, which had none. The
// event carries the run's totals, not its notices, and so names no
// customer.
func record(ctx context.Context, action string, r Run, le tenancy.Entity, before any, issued ...outbox.Event) error {
	overdue := money.EventAmount(r.OverdueAmount, r.Currency, r.Exponent)
	payload := map[string]any{"dunning_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "legal_entity_code": le.Code,
		"key_date": r.KeyDate.Format(time.DateOnly), "notice_count": r.NoticeCount, "item_count": r.ItemCount, "overdue_amount": overdue,
		"status": r.Status, "status_before": before, "version": r.Version}
	detail := map[string]any{"legal_entity_id": r.LegalEntityID, "key_date": r.KeyDate.Format(time.DateOnly), "status": r.Status,
		"status_before": before, "notice_count": r.NoticeCount, "item_count": r.ItemCount, "overdue_amount": overdue}
	events := append([]outbox.Event{{Context: "collections", Name: "dunning_run_changed", AggregateType: "collections.dunning_run",
		AggregateID: r.ID, Payload: payload}}, issued...)
	if err := audit.Record(ctx, action, "collections.dunning_run", r.ID, detail, events...); err != nil {
		return fmt.Errorf("collections: record %s of dunning run %s: %w", action, r.ID, err)
	}
	return nil
}
