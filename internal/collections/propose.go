package collections

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// errTooLarge refuses a run or a worklist whose amounts are out of range.
func errTooLarge() *problem.Problem {
	return problem.Unprocessable("collections.amount_too_large", "The items add up to more than an amount can hold.")
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
// An item with an open dispute case is in no notice, whatever part of it
// is disputed: the run names it as disputed (T86b, plan default 4); a
// blocked or promised one as blocked or promised (T76e, T76f). The
// company's dunning procedure applies (T76d): its grace days, its
// minimum amount, its levels' charges and its interest rate.
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
	proc, err := e.procedure(ctx, tx, le)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: uuid.Must(uuid.NewV7()), LegalEntityID: le.ID, KeyDate: in.KeyDate, GraceDays: int32(proc.grace),
		MinimumAmount: proc.minimum, InterestRate: proc.rateText}
	notices, err := e.proposal(ctx, tx, le, &run, proc)
	if err != nil {
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

// proposal reads the company's open items, what is recorded of them and
// what holds them back, and gives the run's notices, completed and
// charged, with the run's totals.
func (e *Endpoints) proposal(ctx context.Context, tx pgx.Tx, le tenancy.Entity, run *Run, proc procedure) ([]Notice, error) {
	open, err := e.OpenItems.Receivable(ctx, le.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("collections: open customer items of company %s: %w", le.Code, err)
	}
	due := overdue(open, run.KeyDate)
	previous, err := recorded(ctx, tx, refsOf(due))
	if err != nil {
		return nil, err
	}
	h, err := holdsOf(ctx, tx, le.ID, run.KeyDate)
	if err != nil {
		return nil, err
	}
	notices, held := propose(due, proc, previous, h)
	run.Disputed, run.Blocked, run.Promised = held.disputed, held.blocked, held.promised
	if notices, err = charge(run, notices, proc); err != nil {
		return nil, err
	}
	return notices, e.total(ctx, run, notices)
}

// procedure is a company's dunning as a run applies it (ADR-0065 §2,
// T76d): its levels, the days an item is overdue before a run takes it
// (grace), the overdue amount below which a notice is not proposed
// (minimum, minor units of the company's currency, whose digits are
// exp), and the annual interest rate in thousandths of a percent (rate;
// 0: none), which rateText gives as configured for the run to keep.
type procedure struct {
	levels   []config.DunningLevel
	grace    int
	minimum  money.Minor
	rate     int64
	rateText *string
	exp      int
}

// rule is the configured level whose charge and interest apply at a
// level. A level above the configured ones, which an item keeps from a
// confirmed run after the company dropped levels (a level never falls),
// takes the highest configured level's (T76d, ADR-0065 As built).
func (p procedure) rule(level int32) config.DunningLevel {
	return p.levels[min(int(level), len(p.levels))-1]
}

// procedure reads the company's dunning in the active configuration; a
// company without levels has no dunning and its run is refused.
func (e *Endpoints) procedure(ctx context.Context, tx pgx.Tx, le tenancy.Entity) (procedure, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return procedure{}, fmt.Errorf("collections: dunning of company %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	if co == nil || len(co.Dunning.Levels) == 0 {
		return procedure{}, problem.Unprocessable("collections.no_dunning_levels", "Company "+le.Code+
			" has no dunning levels in the configuration (companies[].dunning.levels), so it has no dunning.")
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil) // a legal entity's currency exists
	if err != nil {
		return procedure{}, fmt.Errorf("collections: currency of company %s: %w", le.Code, err)
	}
	d := co.Dunning
	proc := procedure{levels: d.Levels, grace: d.GraceDays, minimum: d.Minimum(exp), rate: d.InterestRate(), exp: exp}
	if proc.rate != 0 {
		proc.rateText = &d.InterestRatePercent
	}
	return proc, nil
}

// overdueItem is an open item its customer owes that is past its net due
// date on a key date, by days.
type overdueItem struct {
	ledger.ReceivableItem
	days int
}

// overdue are, of the open items, in their order, those overdue on the
// key date: the ones the customer owes (a debit: credit items are not
// dunned and reduce nothing) that are past their net due date by a day at
// least. The due date is the ledger's (ReceivableItem.NetDueDate): the
// terms', or the document date of an item posted without any.
func overdue(open []ledger.ReceivableItem, keyDate time.Time) []overdueItem {
	var out []overdueItem
	for _, it := range open {
		if days := it.DaysOverdue(keyDate); days > 0 && it.Amount > 0 {
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

// holds are what keeps an item a run would propose out of its notices:
// the company's open dispute cases, by item (T86b), its dunning blocks in
// force on the run's key date, by item and by customer (T76e), and its
// promises to pay whose date is on or after the key date, by item (T76f).
type holds struct {
	disputes  map[ledger.ItemRef]openCase
	items     map[ledger.ItemRef]block
	customers map[uuid.UUID]block
	promises  map[ledger.ItemRef]promised
}

// holdsOf reads the company's open dispute cases, its dunning blocks in
// force on the key date and its promises to pay holding for it.
func holdsOf(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, keyDate time.Time) (holds, error) {
	disputes, err := openCases(ctx, tx, legalEntity)
	if err != nil {
		return holds{}, err
	}
	items, customers, err := blocksInForce(ctx, tx, legalEntity, keyDate)
	if err != nil {
		return holds{}, err
	}
	promises, err := promisesInForce(ctx, tx, legalEntity, keyDate)
	return holds{disputes: disputes, items: items, customers: customers, promises: promises}, err
}

// blockOf is the block that holds the item: its own, else its customer's.
func (h holds) blockOf(it ledger.ReceivableItem) (block, bool) {
	if b, ok := h.items[it.ItemRef]; ok {
		return b, true
	}
	b, ok := h.customers[it.PartnerID]
	return b, ok
}

// heldItem is what a run keeps of an item it holds back, its customer
// named by number.
func heldItem(it overdueItem, customerNumber string) HeldItem {
	return HeldItem{CustomerID: it.PartnerID, CustomerNumber: customerNumber, ItemRef: it.ItemRef, DocumentNumber: it.Number, Reference: it.Reference,
		NetDueDate: it.NetDueDate(), DaysOverdue: int32(it.days), Currency: it.Currency, Exponent: it.Exponent, Amount: it.Amount, AmountLC: it.AmountLC}
}

// propose is the proposal of a run: of the overdue items, in the ledger's
// order of customer and item, those that reach a dunning level, each from
// the level the confirmed runs recorded for it (previous; none: never
// dunned, level 0), one notice per customer in that order, its items
// numbered from 1. An item dunned before is proposed at its level again
// even when its days no longer reach it at an earlier key date, as long
// as it is overdue then: a level never falls. An item overdue by no more
// than the company's grace days is not proposed (T76d). An item that
// would be proposed but is blocked from dunning, itself or its customer,
// is left out and listed as blocked (T76e); one with an open dispute case
// is left out whole and listed as disputed (T86b); one a customer
// promised to pay by a date on or after the key date is left out and
// listed as promised (T76f), a block coming first and a dispute after;
// each list in the same order. The notices' ids, numbers and totals are
// charge's and total's.
func propose(due []overdueItem, proc procedure, previous map[ledger.ItemRef]dunned, h holds) ([]Notice, heldBack) {
	var notices []Notice
	var held heldBack
	for _, it := range due {
		before := previous[it.ItemRef]
		level := reach(proc.levels, it.days, int(before.Level))
		if level == 0 || it.days <= proc.grace {
			continue
		}
		if held.add(it, h) {
			continue
		}
		if n := len(notices); n == 0 || notices[n-1].CustomerID != it.PartnerID {
			notices = append(notices, Notice{CustomerID: it.PartnerID})
		}
		n := &notices[len(notices)-1]
		n.Items = append(n.Items, Item{ItemNo: int32(len(n.Items) + 1), ItemRef: it.ItemRef, DocumentNumber: it.Number, Reference: it.Reference,
			DocumentDate: it.DocumentDate, NetDueDate: it.NetDueDate(), DaysOverdue: int32(it.days), Currency: it.Currency, Amount: it.Amount,
			AmountLC: it.AmountLC, PreviousLevel: before.Level, Level: int32(level), PreviousDunnings: before.Times})
	}
	return notices, held
}

// heldBack are the items a run holds back, by why.
type heldBack struct {
	disputed []DisputedItem
	blocked  []BlockedItem
	promised []PromisedItem
}

// add lists the item when something holds it back, a block first, then a
// promise to pay, then a dispute case, and reports whether it did.
func (b *heldBack) add(it overdueItem, h holds) bool {
	if bl, ok := h.blockOf(it.ReceivableItem); ok {
		b.blocked = append(b.blocked, BlockedItem{BlockID: bl.ID, HeldItem: heldItem(it, bl.CustomerNumber)})
		return true
	}
	if p, ok := h.promises[it.ItemRef]; ok {
		b.promised = append(b.promised, PromisedItem{PromiseID: p.ID, PromisedDate: p.Date, HeldItem: heldItem(it, p.CustomerNumber)})
		return true
	}
	if c, ok := h.disputes[it.ItemRef]; ok {
		b.disputed = append(b.disputed, DisputedItem{CaseID: c.ID, HeldItem: heldItem(it, c.CustomerNumber)})
		return true
	}
	return false
}

// adder adds amounts and remembers that a sum was out of range, so a
// proposal's many sums are checked once.
type adder struct{ err error }

// sum is the amounts' sum; out of range, 0, and the proposal is refused.
func (a *adder) sum(amounts ...money.Minor) money.Minor {
	s, err := money.Sum(amounts...)
	if err != nil { // money.Sum's only error is the range
		a.err = errTooLarge()
	}
	return s
}

// interest is interestOn's, out of range remembered as sum's.
func (a *adder) interest(amount money.Minor, rate int64, days int32) money.Minor {
	m, err := interestOn(amount, rate, days)
	if err != nil { // money.Round's only error here is the range
		a.err = errTooLarge()
	}
	return m
}

// charge completes each notice of the proposal (T76d): its level (the
// highest among its items) and overdue amount, the items' sum in the
// company's currency; the interest of each item at a level that charges
// it, at the company's rate for its days overdue; and the charge of the
// notice's level. A notice whose overdue amount is below the company's
// minimum amount is left out, counted on the run with its amount. Amounts
// out of range, a notice's total among them, are refused.
func charge(run *Run, notices []Notice, proc procedure) ([]Notice, error) {
	var a adder
	kept := notices[:0]
	var below []money.Minor
	for _, n := range notices {
		local := make([]money.Minor, len(n.Items))
		interest := make([]money.Minor, len(n.Items))
		for j := range n.Items {
			it := &n.Items[j]
			n.Level, local[j] = max(n.Level, it.Level), it.AmountLC
			if proc.rule(it.Level).Interest {
				it.InterestAmount = a.interest(it.AmountLC, proc.rate, it.DaysOverdue)
			}
			interest[j] = it.InterestAmount
		}
		if n.OverdueAmount = a.sum(local...); n.OverdueAmount < proc.minimum {
			below = append(below, n.OverdueAmount)
			continue
		}
		n.ChargeAmount, n.InterestAmount = proc.rule(n.Level).ChargeOf(proc.exp), a.sum(interest...)
		a.sum(n.OverdueAmount, n.ChargeAmount, n.InterestAmount) // the notice's total, which the API gives
		kept = append(kept, n)
	}
	run.BelowMinimumCount, run.BelowMinimumAmount = int32(len(below)), a.sum(below...)
	return kept, a.err
}

// interestOn is the interest on an amount for days at an annual rate in
// thousandths of a percent: amount × rate × days ÷ 365, rounded half away
// from zero in the amount's minor units (T76d: actual days over a year of
// 365, decided by default). Out of range is an error.
func interestOn(amount money.Minor, rate int64, days int32) (money.Minor, error) {
	num := new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(rate*int64(days)))
	return money.Round(new(big.Rat).SetFrac(num, big.NewInt(100*1000*365)), 0)
}

// total completes the proposal: each notice's id and its customer's
// number, and the run's totals over the notices: their count, their
// items' and the amounts overdue, charged and charged as interest. More
// items than a run proposes are refused, the disputed ones it names
// counted with them, as are amounts out of range.
func (e *Endpoints) total(ctx context.Context, run *Run, notices []Notice) error {
	customers := make([]uuid.UUID, len(notices))
	overdue, charges, interest := make([]money.Minor, len(notices)), make([]money.Minor, len(notices)), make([]money.Minor, len(notices))
	limit := e.MaxItems
	if limit == 0 {
		limit = MaxRunItems
	}
	for i := range notices {
		n := &notices[i]
		n.ID, n.RunID, n.ItemCount, customers[i] = uuid.Must(uuid.NewV7()), run.ID, int32(len(n.Items)), n.CustomerID
		overdue[i], charges[i], interest[i] = n.OverdueAmount, n.ChargeAmount, n.InterestAmount
		run.ItemCount += n.ItemCount
	}
	// Checked once all are counted, so a run whose items are all held back is held to it too.
	if int(run.ItemCount)+len(run.Disputed)+len(run.Blocked)+len(run.Promised) > limit {
		return problem.Unprocessable("collections.run_too_large", fmt.Sprintf("The run would take more than %d overdue items, "+
			"the most one run proposes or names as disputed, blocked or promised; larger runs are not supported yet.", limit))
	}
	run.NoticeCount = int32(len(notices))
	if err := sums(run, overdue, charges, interest); err != nil {
		return err
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

// sums sets the run's overdue, charge and interest amounts, the sums of
// its notices', and checks the amounts the API gives besides: the run's
// total and its disputed and blocked amounts. Out of range is refused.
func sums(run *Run, overdue, charges, interest []money.Minor) error {
	var a adder
	run.OverdueAmount, run.ChargeAmount, run.InterestAmount = a.sum(overdue...), a.sum(charges...), a.sum(interest...)
	a.sum(run.OverdueAmount, run.ChargeAmount, run.InterestAmount)
	a.sum(heldAmounts(run.Disputed, DisputedItem.held)...)
	a.sum(heldAmounts(run.Blocked, BlockedItem.held)...)
	a.sum(heldAmounts(run.Promised, PromisedItem.held)...)
	return a.err
}

// insertRun writes the run with its notices and their items, in the
// company's currency.
func insertRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, run Run, notices []Notice) error {
	// Stored as lists, empty and not null without any, each item without
	// its currency's digits, which the run is read with.
	disputed := stored(run.Disputed, func(d *DisputedItem) { d.Exponent = 0 })
	blocked := stored(run.Blocked, func(b *BlockedItem) { b.Exponent = 0 })
	promised := stored(run.Promised, func(p *PromisedItem) { p.Exponent = 0 })
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count,
			item_count, overdue_amount, disputed_items, grace_days, minimum_amount, interest_rate_percent, charge_amount, interest_amount,
			below_minimum_count, below_minimum_amount, blocked_items, promised_items, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13::numeric, $14, $15, $16, $17, $18::jsonb, $19::jsonb, $20, $20)`,
		p.TenantID, run.ID, run.LegalEntityID, run.KeyDate, Proposed, le.LocalCurrency, run.NoticeCount, run.ItemCount, run.OverdueAmount,
		disputed, run.GraceDays, run.MinimumAmount, run.InterestRate, run.ChargeAmount, run.InterestAmount, run.BelowMinimumCount,
		run.BelowMinimumAmount, blocked, promised, p.ID); err != nil {
		return fmt.Errorf("collections: insert dunning run %s: %w", run.ID, err)
	}
	n := len(notices)
	ids, customers, numbers := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]string, n)
	levels, counts, amounts := make([]int32, n), make([]int32, n), make([]money.Minor, n)
	charges, interest := make([]money.Minor, n), make([]money.Minor, n)
	var all []Item
	var of []uuid.UUID
	for i, nt := range notices {
		ids[i], customers[i], numbers[i], levels[i], counts[i], amounts[i] = nt.ID, nt.CustomerID, nt.CustomerNumber, nt.Level, nt.ItemCount, nt.OverdueAmount
		charges[i], interest[i] = nt.ChargeAmount, nt.InterestAmount
		for _, it := range nt.Items {
			all, of = append(all, it), append(of, nt.ID)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count,
			overdue_amount, charge_amount, interest_amount)
		SELECT $1, i, $2, c, n, l, k, a, ch, it
		FROM unnest($3::uuid[], $4::uuid[], $5::text[], $6::int[], $7::int[], $8::bigint[], $9::bigint[], $10::bigint[]) AS x(i, c, n, l, k, a, ch, it)`,
		p.TenantID, run.ID, ids, customers, numbers, levels, counts, amounts, charges, interest); err != nil {
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
	previous, levels, dunnings, interest := make([]int32, n), make([]int32, n), make([]int32, n), make([]money.Minor, n)
	for i, it := range items {
		nos[i], years[i], entries[i], lines[i] = it.ItemNo, it.FiscalYear, it.EntryID, it.LineNo
		numbers[i], references[i], dates[i], dues[i] = it.DocumentNumber, it.Reference, it.DocumentDate, it.NetDueDate
		days[i], currencies[i], amounts[i], local[i] = it.DaysOverdue, it.Currency, it.Amount, it.AmountLC
		previous[i], levels[i], dunnings[i], interest[i] = it.PreviousLevel, it.Level, it.PreviousDunnings, it.InterestAmount
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year,
			journal_entry_id, line_no, document_number, reference, document_date, net_due_date, days_overdue, currency, amount, amount_lc,
			previous_level, level, previous_dunnings, interest_amount)
		SELECT $1, $2, nt, no, fy, je, ln, dn, rf, dd, du, dy, cu, am, lc, pl, lv, pd, ia
		FROM unnest($3::uuid[], $4::int[], $5::smallint[], $6::uuid[], $7::int[], $8::text[], $9::text[], $10::date[], $11::date[], $12::int[],
			$13::text[], $14::bigint[], $15::bigint[], $16::int[], $17::int[], $18::int[], $19::bigint[])
			AS x(nt, no, fy, je, ln, dn, rf, dd, du, dy, cu, am, lc, pl, lv, pd, ia)`,
		tenant, run, of, nos, years, entries, lines, numbers, references, dates, dues, days, currencies, amounts, local, previous, levels, dunnings,
		interest); err != nil {
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
		"charge_amount":   money.EventAmount(r.ChargeAmount, r.Currency, r.Exponent),
		"interest_amount": money.EventAmount(r.InterestAmount, r.Currency, r.Exponent), "status": r.Status, "status_before": before,
		"version": r.Version}
	detail := map[string]any{"legal_entity_id": r.LegalEntityID, "key_date": r.KeyDate.Format(time.DateOnly), "status": r.Status,
		"status_before": before, "notice_count": r.NoticeCount, "item_count": r.ItemCount, "overdue_amount": overdue}
	events := append([]outbox.Event{{Context: "collections", Name: "dunning_run_changed", AggregateType: "collections.dunning_run",
		AggregateID: r.ID, Payload: payload}}, issued...)
	if err := audit.Record(ctx, action, "collections.dunning_run", r.ID, detail, events...); err != nil {
		return fmt.Errorf("collections: record %s of dunning run %s: %w", action, r.ID, err)
	}
	return nil
}
