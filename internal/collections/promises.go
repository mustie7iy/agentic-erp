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
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a promise to pay on a date (T76f): PromiseKept, its
// items no longer open add up to the promised amount; PromiseBroken, not
// kept and the date is past the promised date; PromiseOpen otherwise.
const (
	PromiseOpen   = "open"
	PromiseKept   = "kept"
	PromiseBroken = "broken"
)

// MaxPromiseItems is the most items one promise is for, as the contract
// bounds them.
const MaxPromiseItems = 100

// Promise is a row of collections.promise_to_pay with its items by
// journal line (ADR-0065 As built, T76f): the customer promised the
// company to pay Amount, minor units of Currency (the company's, whose
// digits are Exponent), by Date. CustomerNumber is the customer's when
// the promise was recorded. A promise is not changed: Version stays 1.
type Promise struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	Currency       string
	Exponent       int
	Amount         money.Minor
	Date           time.Time
	Items          []PromiseItem
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

// PromiseItem is an item a promise is for, as the promise read it:
// Amount is minor units of Currency, the item's, whose digits are
// Exponent; AmountLC of the promise's currency. Open is whether it is
// still open, which the reader of the promise sets (stillOpen).
type PromiseItem struct {
	ledger.ItemRef
	DocumentNumber string
	Reference      *string
	NetDueDate     time.Time
	Currency       string
	Exponent       int
	Amount         money.Minor
	AmountLC       money.Minor
	Open           bool
}

// Paid is what the promised items no longer open amount to in the
// promise's currency, as booked: an item counts in full once it is
// cleared, whether paid, credited or reversed.
func (p Promise) Paid() money.Minor {
	var paid []money.Minor
	for _, it := range p.Items {
		if !it.Open {
			paid = append(paid, it.AmountLC)
		}
	}
	sum, _ := money.Sum(paid...) // in range: part of the items' local amounts, whose sum RecordPromise checked
	return sum
}

// StatusOn is the promise's status on a day, from its items' Open as set:
// kept once what is paid reaches the promised amount, broken once the
// day is past the promised date and it is not kept, open otherwise.
func (p Promise) StatusOn(day time.Time) string {
	switch {
	case p.Paid() >= p.Amount:
		return PromiseKept
	case day.After(p.Date):
		return PromiseBroken
	}
	return PromiseOpen
}

// stillOpen sets each item's Open: whether the open set holds it.
func (p *Promise) stillOpen(open func(ledger.ItemRef) bool) {
	for i := range p.Items {
		p.Items[i].Open = open(p.Items[i].ItemRef)
	}
}

const promiseSelect = `SELECT p.id, p.legal_entity_id, p.customer_id, p.customer_number, p.currency, c.exponent, p.promised_amount, p.promised_date,
		p.version, p.created_at, p.created_by, p.updated_at, p.updated_by
	FROM collections.promise_to_pay p JOIN ref.currency c ON c.code = p.currency`

func scanPromise(row pgx.Row) (Promise, error) {
	var p Promise
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.CustomerID, &p.CustomerNumber, &p.Currency, &p.Exponent, &p.Amount, &p.Date, &p.Version,
		&p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &p.UpdatedBy)
	return p, errs.Wrapf(err, "scan promise to pay")
}

// promiseItems reads the items of the promises, each promise's by journal
// line, and gives them to their promises.
func promiseItems(ctx context.Context, tx pgx.Tx, promises []Promise) error {
	at := make(map[uuid.UUID]int, len(promises))
	ids := make([]uuid.UUID, len(promises))
	for i, p := range promises {
		at[p.ID], ids[i] = i, p.ID
	}
	rows, _ := tx.Query(ctx, `SELECT i.promise_to_pay_id, i.fiscal_year, i.journal_entry_id, i.line_no, i.document_number, i.reference, i.net_due_date,
			i.currency, c.exponent, i.amount, i.amount_lc
		FROM collections.promise_to_pay_item i JOIN ref.currency c ON c.code = i.currency
		WHERE i.promise_to_pay_id = ANY($1) ORDER BY i.promise_to_pay_id, i.fiscal_year, i.journal_entry_id, i.line_no`, ids)
	var promise uuid.UUID
	var it PromiseItem
	_, err := pgx.ForEachRow(rows, []any{&promise, &it.FiscalYear, &it.EntryID, &it.LineNo, &it.DocumentNumber, &it.Reference, &it.NetDueDate,
		&it.Currency, &it.Exponent, &it.Amount, &it.AmountLC}, func() error {
		p := &promises[at[promise]]
		p.Items = append(p.Items, it)
		return nil
	})
	if err != nil {
		return fmt.Errorf("collections: read the items of %d promises to pay: %w", len(promises), err)
	}
	return nil
}

// readPromises reads the promises a query selects (promiseSelect with its
// conditions and order), each with its items.
func readPromises(ctx context.Context, tx pgx.Tx, what, q string, args ...any) ([]Promise, error) {
	rows, _ := tx.Query(ctx, q, args...)
	promises, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Promise, error) { return scanPromise(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: read %s: %w", what, err)
	}
	return promises, promiseItems(ctx, tx, promises)
}

// GetPromise returns a promise to pay of a legal entity the caller may
// act in, with its items; another company's is not found, so ids cannot
// be probed (04 §4.9). Its items' Open is not set.
func GetPromise(ctx context.Context, id uuid.UUID) (Promise, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Promise{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Promise{}, fmt.Errorf("collections: company access for promise to pay %s: %w", id, err)
	}
	p, err := scanPromise(tx.QueryRow(ctx, promiseSelect+` WHERE p.id = $1`, id))
	if err := authz.Visible(err, access, p.LegalEntityID, problem.NotFoundOf("promise to pay"), "collections: read promise to pay %s", id); err != nil {
		return Promise{}, err
	}
	ps := []Promise{p}
	if err := promiseItems(ctx, tx, ps); err != nil {
		return Promise{}, err
	}
	return ps[0], nil
}

// promised is a promise as a run reads it: its id, its customer's number
// and its date.
type promised struct {
	ID             uuid.UUID
	CustomerNumber string
	Date           time.Time
}

// promisesInForce reads the company's promises that hold for a run at the
// key date (their date on or after it), by item, over the index
// promise_to_pay_by_date. Of several promises of one item, which a past
// key date can find, the latest recorded is named.
func promisesInForce(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, keyDate time.Time) (map[ledger.ItemRef]promised, error) {
	rows, _ := tx.Query(ctx, `SELECT i.fiscal_year, i.journal_entry_id, i.line_no, p.id, p.customer_number, p.promised_date
		FROM collections.promise_to_pay p JOIN collections.promise_to_pay_item i ON i.tenant_id = p.tenant_id AND i.promise_to_pay_id = p.id
		WHERE p.legal_entity_id = $1 AND p.promised_date >= $2 ORDER BY p.created_at, p.id`, legalEntity, keyDate)
	out := map[ledger.ItemRef]promised{}
	var ref ledger.ItemRef
	var p promised
	_, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo, &p.ID, &p.CustomerNumber, &p.Date}, func() error {
		out[ref] = p
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collections: read the promises to pay of company %s: %w", legalEntity, err)
	}
	return out, nil
}

// PromiseInput is a promise to pay as a client records it: the company,
// the customer, the promised amount as a decimal in a currency, the date
// and the items by journal line.
type PromiseInput struct {
	LegalEntityID    uuid.UUID
	CustomerID       uuid.UUID
	Amount, Currency string
	Date             time.Time
	Items            []ledger.ItemRef
}

// RecordPromise records a customer's promise to pay in a company the
// caller may act in (ADR-0065 As built, T76f): the customer is a business
// partner of the tenant; each item one of the company's open items it
// owes, named once and in no other promise whose date has not passed;
// the amount in the company's currency, above zero and at most what the
// items amount to in it; the date from the company's today to
// LastKeyDate. The promises of a customer are recorded one at a time (a
// lock on the customer, held to the end of the transaction), so two of
// one item cannot both pass the check. The audit record is written in the
// request's transaction; nothing is posted and no item changes.
func (e *Endpoints) RecordPromise(ctx context.Context, in PromiseInput) (Promise, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Promise{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Promise{}, fmt.Errorf("collections: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Promise{}, fmt.Errorf("collections: legal entity of a promise to pay: %w", err)
	}
	today, err := e.today(ctx, le)
	if err != nil {
		return Promise{}, err
	}
	if in.Date.Before(today.Date) || in.Date.After(LastKeyDate) {
		return Promise{}, problem.Invalid("collections.invalid_promised_date", "promised_date", fmt.Sprintf("A customer promises to pay by a date "+
			"from company %s's today, %s (%s), to %s.", le.Code, today.Date.Format(time.DateOnly), today.Zone, LastKeyDate.Format(time.DateOnly)))
	}
	number, err := e.namedCustomer(ctx, in.CustomerID)
	if err != nil {
		return Promise{}, err
	}
	items, err := e.promisedItems(ctx, tx, le, in.CustomerID, in.Items, today.Date)
	if err != nil {
		return Promise{}, err
	}
	amount, err := promisedAmount(in, le, items)
	if err != nil {
		return Promise{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := insertPromise(ctx, tx, p, id, le, in, number, amount, items); err != nil {
		return Promise{}, err
	}
	stored, err := GetPromise(ctx, id)
	if err != nil {
		return Promise{}, err
	}
	return stored, recordPromise(ctx, stored, today.Date)
}

// today is the company's date now in its time zone, by the endpoints'
// clock.
func (e *Endpoints) today(ctx context.Context, le tenancy.Entity) (config.CompanyDay, error) {
	today, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return config.CompanyDay{}, fmt.Errorf("collections: company %s's day: %w", le.Code, err)
	}
	return today, nil
}

// promisedItems reads the items a promise names: each the company's open
// item the customer owes (refused naming its journal entry otherwise),
// named once, and in no other promise whose date is today or later. The
// items are read first; then the customer's lock is taken, held to the
// end of the transaction, and only then are the other promises checked,
// so the check holds until the promise is written. That relies on the
// request running at READ COMMITTED (createPromiseToPay has no
// x-snapshot): the check's statement, taken after the lock, sees a
// promise a transaction that held the lock before committed.
func (e *Endpoints) promisedItems(ctx context.Context, tx pgx.Tx, le tenancy.Entity, customer uuid.UUID, refs []ledger.ItemRef,
	today time.Time) ([]ledger.ReceivableItem, error) {
	if len(refs) == 0 || len(refs) > MaxPromiseItems { // the contract bounds them; a Go caller is held to it too
		return nil, problem.Unprocessable("request.validation_failed", fmt.Sprintf("The request is not valid: a promise is for 1 to %d items.",
			MaxPromiseItems), problem.FieldError{Field: "items", Code: "invalid", Message: "1 to 100 items"})
	}
	seen := make(map[ledger.ItemRef]bool, len(refs))
	items := make([]ledger.ReceivableItem, len(refs))
	for i, ref := range refs {
		if seen[ref] {
			return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: a promise names each item once.",
				problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "duplicate", Message: "named before"})
		}
		seen[ref] = true
		it, err := e.customersItem(ctx, le, customer, ref, fmt.Sprintf("items[%d].journal_entry_id", i))
		if err != nil {
			return nil, err
		}
		items[i] = it
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('collections.promise:' || $1::text, 0))`, customer); err != nil {
		return nil, fmt.Errorf("collections: lock the promises of customer %s: %w", customer, err)
	}
	years, entries, lines := ledger.ItemColumns(refs)
	var other uuid.UUID
	err := tx.QueryRow(ctx, `SELECT p.id FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(fy, je, ln)
		JOIN collections.promise_to_pay_item i ON i.fiscal_year = x.fy AND i.journal_entry_id = x.je AND i.line_no = x.ln
		JOIN collections.promise_to_pay p ON p.tenant_id = i.tenant_id AND p.id = i.promise_to_pay_id
		WHERE p.promised_date >= $4 ORDER BY p.created_at, p.id LIMIT 1`, years, entries, lines, today).Scan(&other)
	switch {
	case err == nil:
		return nil, problem.Conflict("collections.promise_exists", fmt.Sprintf("An item is in promise to pay %s, whose date has not passed; "+
			"an item is promised once at a time.", other))
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("collections: the promises of %d items: %w", len(refs), err)
	}
	return items, nil
}

// promisedAmount reads the promised amount: in the company's currency,
// above zero and at most what the items amount to in it, as booked.
func promisedAmount(in PromiseInput, le tenancy.Entity, items []ledger.ReceivableItem) (money.Minor, error) {
	if in.Currency != le.LocalCurrency {
		return 0, problem.Invalid("collections.currency_mismatch", "promised_amount.currency", "A promise to pay is in the company's currency, "+
			le.LocalCurrency+".")
	}
	local := make([]money.Minor, len(items))
	for i, it := range items {
		local[i] = it.AmountLC
	}
	total, err := money.Sum(local...)
	if err != nil { // money.Sum's only error is the range
		return 0, errTooLarge()
	}
	exp := items[0].LocalExponent // the contract asks for one item at least; each is in the company's currency
	amount, err := currency.ParseAmount(in.Amount, in.Currency, exp, "promised_amount.amount")
	if err != nil {
		return 0, err
	}
	if amount <= 0 || amount > total {
		return 0, problem.Invalid("collections.invalid_promised_amount", "promised_amount.amount", fmt.Sprintf("A customer promises to pay more "+
			"than zero and at most what the items amount to, %s %s.", total.Plain(exp), le.LocalCurrency))
	}
	return amount, nil
}

// insertPromise writes the promise and its items.
func insertPromise(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, le tenancy.Entity, in PromiseInput, number string,
	amount money.Minor, items []ledger.ReceivableItem) error {
	if _, err := tx.Exec(ctx, `INSERT INTO collections.promise_to_pay (tenant_id, id, legal_entity_id, customer_id, customer_number, currency,
			promised_amount, promised_date, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`, p.TenantID, id, le.ID, in.CustomerID, number, le.LocalCurrency, amount, in.Date, p.ID); err != nil {
		return fmt.Errorf("collections: record a promise to pay of customer %s: %w", in.CustomerID, err)
	}
	n := len(items)
	years, entries, lines := ledger.ItemColumns(in.Items)
	numbers, references, dues := make([]string, n), make([]*string, n), make([]time.Time, n)
	currencies, amounts, local := make([]string, n), make([]money.Minor, n), make([]money.Minor, n)
	for i, it := range items {
		numbers[i], references[i], dues[i], currencies[i], amounts[i], local[i] = it.Number, it.Reference, it.NetDueDate(), it.Currency, it.Amount, it.AmountLC
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.promise_to_pay_item (tenant_id, promise_to_pay_id, fiscal_year, journal_entry_id, line_no,
			document_number, reference, net_due_date, currency, amount, amount_lc)
		SELECT $1, $2, fy, je, ln, dn, rf, du, cu, am, lc
		FROM unnest($3::smallint[], $4::uuid[], $5::int[], $6::text[], $7::text[], $8::date[], $9::text[], $10::bigint[], $11::bigint[])
			AS x(fy, je, ln, dn, rf, du, cu, am, lc)`,
		p.TenantID, id, years, entries, lines, numbers, references, dues, currencies, amounts, local); err != nil {
		return fmt.Errorf("collections: write the items of promise to pay %s: %w", id, err)
	}
	return nil
}

// recordPromise adds the audit record of a promise recorded (ADR-0011),
// with its company, customer, amount, date and items. A promise publishes
// no event: it changes nothing outside the context, and a run started
// afterwards names the items it holds back. Without events audit.Record
// fails only outside a unit of work, which reqctx.Request refused before.
func recordPromise(ctx context.Context, p Promise, today time.Time) error {
	items := make([]map[string]any, len(p.Items))
	for i, it := range p.Items {
		items[i] = map[string]any{"fiscal_year": it.FiscalYear, "journal_entry_id": it.EntryID, "line_no": it.LineNo}
	}
	return audit.Record(ctx, "collections.promise_to_pay.record", "collections.promise_to_pay", p.ID, map[string]any{
		"legal_entity_id": p.LegalEntityID, "customer_id": p.CustomerID, "promised_amount": money.EventAmount(p.Amount, p.Currency, p.Exponent),
		"promised_date": p.Date.Format(time.DateOnly), "recorded_on": today.Format(time.DateOnly), "items": items})
}
