package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	problem.RegisterConstraint("reversal_pkey", func() *problem.Problem {
		return problem.Conflict("ledger.already_reversed", "The journal entry has already been reversed.")
	})
	problem.RegisterConstraint("journal_line_uom_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.unknown_unit", "A quantity's unit of measure is not known.",
			problem.FieldError{Field: "lines.quantity.unit", Code: "unknown", Message: "a unit of measure code from the reference data"})
	})
}

// Entry is a posted journal entry with its lines. Amounts are signed minor
// units, debit positive; Exp* are the currencies' minor-unit digits.
type Entry struct {
	ID               uuid.UUID
	LegalEntityID    uuid.UUID
	LedgerID         uuid.UUID
	FiscalYear       int16
	FiscalPeriod     int16
	DocumentType     string
	DocumentNumber   string
	DocumentDate     time.Time
	PostingDate      time.Time
	CurrencyTC       string
	CurrencyLC       string
	CurrencyGC       string
	RateTCLC         string
	RateLCGC         string
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID *uuid.UUID
	ReversalOfID     *uuid.UUID
	ReversalOfYear   *int16
	ReversedByID     *uuid.UUID
	PostedAt         time.Time
	PostedBy         uuid.UUID
	ExpTC            int
	ExpLC            int
	ExpGC            int
	Lines            []Line
}

// Line is one journal line.
type Line struct {
	LineNo    int32       `json:"line_no"`
	AccountID uuid.UUID   `json:"account_id"`
	TC        money.Minor `json:"tc"`
	LC        money.Minor `json:"lc"`
	GC        money.Minor `json:"gc"`
	PartnerID *uuid.UUID  `json:"partner_id"`
	TaxCode   *string     `json:"tax_code"`
	Quantity  *string     `json:"quantity"`
	Unit      *string     `json:"unit"`
	Text      *string     `json:"text"`
}

const entrySelect = `SELECT e.id, e.legal_entity_id, e.ledger_id, e.fiscal_year, e.fiscal_period, e.document_type, e.document_number,
		e.document_date, e.posting_date, e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc::text, e.rate_lc_gc::text,
		e.reference, e.header_text, e.source_context, e.source_document_id, e.reversal_of_id, e.reversal_of_year, r.reversal_entry_id,
		e.posted_at, e.posted_by, ctc.exponent, clc.exponent, cgc.exponent,
		(SELECT json_agg(json_build_object('line_no', l.line_no, 'account_id', l.account_id, 'tc', l.amount_tc, 'lc', l.amount_lc,
			'gc', l.amount_gc, 'partner_id', l.partner_id, 'tax_code', l.tax_code, 'quantity', trim_scale(l.quantity)::text,
			'unit', l.uom_code, 'text', l.line_text) ORDER BY l.line_no)
		 FROM ledger.journal_line l WHERE l.tenant_id = e.tenant_id AND l.fiscal_year = e.fiscal_year AND l.journal_entry_id = e.id)
	FROM ledger.journal_entry e
	JOIN ref.currency ctc ON ctc.code = e.currency_tc
	JOIN ref.currency clc ON clc.code = e.currency_lc
	JOIN ref.currency cgc ON cgc.code = e.currency_gc
	LEFT JOIN ledger.reversal r ON r.tenant_id = e.tenant_id AND r.original_entry_id = e.id`

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	err := errs.Wrapf(row.Scan(&e.ID, &e.LegalEntityID, &e.LedgerID, &e.FiscalYear, &e.FiscalPeriod, &e.DocumentType, &e.DocumentNumber,
		&e.DocumentDate, &e.PostingDate, &e.CurrencyTC, &e.CurrencyLC, &e.CurrencyGC, &e.RateTCLC, &e.RateLCGC,
		&e.Reference, &e.HeaderText, &e.SourceContext, &e.SourceDocumentID, &e.ReversalOfID, &e.ReversalOfYear, &e.ReversedByID,
		&e.PostedAt, &e.PostedBy, &e.ExpTC, &e.ExpLC, &e.ExpGC, &e.Lines), "scan journal entry")
	return e, err
}

// getEntry loads an entry of a legal entity the caller may act in.
func getEntry(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Entry, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Entry{}, err
	}
	e, err := scanEntry(tx.QueryRow(ctx, entrySelect+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(e.LegalEntityID)) {
		return Entry{}, problem.NotFound("No journal entry with this id.")
	}
	if err != nil {
		return Entry{}, fmt.Errorf("ledger: read journal entry %s: %w", id, err)
	}
	return e, nil
}

// GetEntry returns a journal entry.
func GetEntry(ctx context.Context, id uuid.UUID) (Entry, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	return getEntry(ctx, tx, id)
}

// draft is an entry to post, with amounts in all three currencies.
type draft struct {
	entity           tenancy.Entity
	ledgerID         uuid.UUID
	documentType     string
	documentDate     time.Time
	postingDate      time.Time
	currencyTC       string
	currencyLC       string
	currencyGC       string
	rateTCLC         string
	rateLCGC         string
	reference        *string
	headerText       *string
	sourceContext    string
	sourceDocumentID *uuid.UUID
	reversalOf       *Entry
	lines            []Line
}

// postingPeriod finds the open posting period of the posting date and
// holds its status row FOR SHARE until the transaction ends. A close locks
// the row FOR UPDATE (changePeriod), so a posting waits for a close under
// way and then reads the committed status, and a close waits for the
// postings under way: no entry lands in a period whose close committed
// first. The lock comes after the entry buckets and before the number range
// (03-data-model, entry locks); a close takes nothing else, so no cycle.
func postingPeriod(ctx context.Context, tx pgx.Tx, e tenancy.Entity, ledgerID uuid.UUID, date time.Time) (int16, int16, error) {
	var year, number int16
	var status string
	day := date.Format(time.DateOnly)
	err := tx.QueryRow(ctx, `SELECT fiscal_year, period FROM ledger.fiscal_period
		WHERE calendar_id = $1 AND NOT is_special AND $2 BETWEEN start_date AND end_date`,
		e.FiscalCalendarID, date).Scan(&year, &number)
	if err == nil {
		// A query of its own: FOR SHARE cannot lock the nullable side of an outer join.
		err = tx.QueryRow(ctx, `SELECT status FROM ledger.period_status
			WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3 AND period = $4 FOR SHARE`,
			e.ID, ledgerID, year, number).Scan(&status)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, 0, problem.Unprocessable("ledger.no_fiscal_period", "No fiscal period of the legal entity covers "+day+
			"; a fiscal year is set up by a number range for it in the configuration.",
			problem.FieldError{Field: "posting_date", Code: "no_fiscal_period", Message: "a date in a fiscal year that is set up"})
	case err != nil:
		return 0, 0, fmt.Errorf("ledger: posting period of %s on %s: %w", e.ID, day, err)
	case status != Open:
		return 0, 0, problem.Conflict("ledger.period_not_open", fmt.Sprintf("Period %d/%d is %s; postings dated %s need it open.", year, number, status, day))
	}
	return year, number, nil
}

// post inserts a balanced entry with its lines, publishes JournalEntryPosted
// and queues the entry for the balance projector.
func (e *Endpoints) post(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d draft) (Entry, error) {
	year, number, err := postingPeriod(ctx, tx, d.entity, d.ledgerID, d.postingDate)
	if err != nil {
		return Entry{}, err
	}
	docNumber, err := e.Numbers.Next(ctx, d.entity.ID, "journal_"+strings.ToLower(d.documentType), year)
	if err != nil {
		return Entry{}, err
	}
	id := uuid.Must(uuid.NewV7())
	var reversalOf *uuid.UUID
	var reversalYear *int16
	if d.reversalOf != nil {
		reversalOf, reversalYear = &d.reversalOf.ID, &d.reversalOf.FiscalYear
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period,
			document_type, document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc,
			reference, header_text, source_context, source_document_id, reversal_of_id, reversal_of_year, posted_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::numeric, $15::numeric, $16, $17, $18, $19, $20, $21, $22)`,
		p.TenantID, year, id, d.entity.ID, d.ledgerID, number, d.documentType, docNumber, d.documentDate, d.postingDate,
		d.currencyTC, d.currencyLC, d.currencyGC, d.rateTCLC, d.rateLCGC, d.reference, d.headerText, d.sourceContext, d.sourceDocumentID,
		reversalOf, reversalYear, p.ID); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: %w", id, err)
	}
	n := len(d.lines)
	nos, accounts, partners := make([]int32, n), make([]uuid.UUID, n), make([]*uuid.UUID, n)
	tcs, lcs, gcs := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n)
	taxes, quantities, units, texts := make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
	for i, l := range d.lines {
		nos[i], accounts[i], partners[i] = int32(i+1), l.AccountID, l.PartnerID
		tcs[i], lcs[i], gcs[i] = l.TC, l.LC, l.GC
		taxes[i], quantities[i], units[i], texts[i] = l.TaxCode, l.Quantity, l.Unit, l.Text
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id,
			fiscal_period, posting_date, account_id, amount_tc, amount_lc, amount_gc, partner_id, tax_code, quantity, uom_code, line_text)
		SELECT $1, $2, $3, n, $4, $5, $6, $7, a, tc, lc, gc, pa, tx, q::numeric, u, t
		FROM unnest($8::int[], $9::uuid[], $10::bigint[], $11::bigint[], $12::bigint[], $13::uuid[], $14::text[], $15::text[], $16::text[], $17::text[])
			AS x(n, a, tc, lc, gc, pa, tx, q, u, t)`,
		p.TenantID, year, id, d.entity.ID, d.ledgerID, number, d.postingDate, nos, accounts, tcs, lcs, gcs, partners,
		taxes, quantities, units, texts); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: lines: %w", id, err)
	}
	posted, err := getEntry(ctx, tx, id)
	if err != nil {
		return Entry{}, err
	}
	event, err := outbox.Add(ctx, postedEvent(posted))
	if err != nil {
		return Entry{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.balance_projection (tenant_id, event_id, fiscal_year, journal_entry_id) VALUES ($1, $2, $3, $4)`,
		p.TenantID, event, year, id); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: queue projection: %w", id, err)
	}
	return posted, nil
}

func postedEvent(e Entry) outbox.Event {
	lines := make([]map[string]any, len(e.Lines))
	for i, l := range e.Lines {
		line := map[string]any{"line_no": l.LineNo, "account_id": l.AccountID, "partner_id": l.PartnerID, "tax_code": l.TaxCode,
			"amount_tc": money.EventAmount(l.TC, e.CurrencyTC, e.ExpTC), "amount_lc": money.EventAmount(l.LC, e.CurrencyLC, e.ExpLC),
			"amount_gc": money.EventAmount(l.GC, e.CurrencyGC, e.ExpGC)}
		if l.Quantity != nil {
			line["quantity"] = map[string]any{"value": *l.Quantity, "unit": *l.Unit}
		}
		lines[i] = line
	}
	return outbox.Event{Context: "ledger", Name: "journal_entry_posted", AggregateType: "ledger.journal_entry", AggregateID: e.ID,
		Payload: map[string]any{
			"journal_entry_id": e.ID, "fiscal_year": e.FiscalYear, "fiscal_period": e.FiscalPeriod, "legal_entity_id": e.LegalEntityID,
			"ledger_id": e.LedgerID, "document_type": e.DocumentType, "document_number": e.DocumentNumber,
			"document_date": e.DocumentDate.Format(time.DateOnly), "posting_date": e.PostingDate.Format(time.DateOnly),
			"currency_tc": e.CurrencyTC, "currency_lc": e.CurrencyLC, "currency_gc": e.CurrencyGC, "reference": e.Reference,
			"source_context": e.SourceContext, "source_document_id": e.SourceDocumentID, "reversal_of_id": e.ReversalOfID,
			"reversal_of_year": e.ReversalOfYear, "lines": lines,
		}}
}

// NewLine is a line of a manual entry, in transaction currency.
type NewLine struct {
	AccountID      uuid.UUID
	Amount         string
	Currency       string
	PartnerID      *uuid.UUID
	TaxCode        *string
	Quantity, Unit *string
	Text           *string
}

// NewEntry is a manual journal entry to post.
type NewEntry struct {
	LegalEntityID uuid.UUID
	LedgerID      *uuid.UUID
	DocumentType  string
	DocumentDate  time.Time
	PostingDate   time.Time
	Currency      string
	Reference     *string
	HeaderText    *string
	Lines         []NewLine
	// The context and document a posting is made for; manual entries are
	// the ledger's own (source "ledger", no document).
	SourceContext    string
	SourceDocumentID *uuid.UUID
}

// legalEntity loads a legal entity the caller may post in; field names the
// request field that refers to it.
func (e *Endpoints) legalEntity(ctx context.Context, id uuid.UUID, field string) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, id, field)
	if err != nil {
		return tenancy.Entity{}, err
	}
	if le.Status != "active" {
		return tenancy.Entity{}, problem.Unprocessable("ledger.legal_entity_not_active", "The legal entity is "+le.Status+"; postings need it active.",
			problem.FieldError{Field: field, Code: "not_active", Message: "an active legal entity"})
	}
	return le, nil
}

// ledgerID resolves the requested ledger, the leading one by default.
func ledgerID(ctx context.Context, tx pgx.Tx, requested *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	var err error
	if requested != nil {
		err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE id = $1`, *requested).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE is_leading`).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return id, problem.Unprocessable("ledger.unknown_ledger", "No such ledger; the leading ledger is created with the tenant's first configuration.",
			problem.FieldError{Field: "ledger_id", Code: "not_found", Message: "a ledger of the tenant"})
	}
	if err != nil {
		return id, fmt.Errorf("ledger: resolve ledger: %w", err)
	}
	return id, nil
}

// exponents returns the minor-unit digits of the currencies.
func exponents(ctx context.Context, tx pgx.Tx, codes ...string) (currency.Exponents, error) {
	out, err := currency.LoadCodes(ctx, tx, codes...)
	if err != nil {
		return nil, fmt.Errorf("ledger: currency exponents %v: %w", codes, err)
	}
	return out, nil
}

// account is what a posting needs to know about an account in a legal entity.
type account struct {
	kind, status string
	set          bool // has settings in the legal entity
	blocked      bool
}

// checkAccounts requires every line's account to be postable in the legal
// entity, and partners exactly on reconciliation accounts. withPartners
// asks the partner context whether each partner may be posted to.
func (e *Endpoints) checkAccounts(ctx context.Context, tx pgx.Tx, le uuid.UUID, lines []Line, withPartners bool) error {
	accounts, err := lineAccounts(ctx, tx, le, lines)
	if err != nil {
		return err
	}
	type partnerKey struct {
		partner uuid.UUID
		kind    string
	}
	checked := map[partnerKey]bool{}
	for i, l := range lines {
		field := fmt.Sprintf("lines[%d]", i)
		a, ok := accounts[l.AccountID]
		if err := requirePostable(i, field, a, ok); err != nil {
			return err
		}
		recon := a.kind == "supplier" || a.kind == "customer"
		switch {
		case recon && l.PartnerID == nil:
			return problem.Unprocessable("ledger.partner_required", "Line "+fmt.Sprint(i+1)+" posts to a "+a.kind+" reconciliation account and needs a partner.",
				problem.FieldError{Field: field + ".partner_id", Code: "required", Message: "the " + a.kind + " of the line"})
		case !recon && l.PartnerID != nil:
			return problem.Unprocessable("ledger.partner_not_allowed", "Line "+fmt.Sprint(i+1)+": partners are posted only on reconciliation accounts.",
				problem.FieldError{Field: field + ".partner_id", Code: "not_allowed", Message: "only on supplier or customer reconciliation accounts"})
		case !recon || !withPartners || checked[partnerKey{*l.PartnerID, a.kind}]:
			continue
		}
		checked[partnerKey{*l.PartnerID, a.kind}] = true
		why, err := e.Partners.PostingBlock(ctx, *l.PartnerID, le, a.kind)
		if err != nil {
			return err
		}
		if why != "" {
			return problem.Unprocessable("ledger.partner_not_postable", "Line "+fmt.Sprint(i+1)+": "+why+".",
				problem.FieldError{Field: field + ".partner_id", Code: "not_postable", Message: why})
		}
	}
	return nil
}

// lineAccounts reads the accounts the lines post to, with their settings
// in the legal entity.
func lineAccounts(ctx context.Context, tx pgx.Tx, le uuid.UUID, lines []Line) (map[uuid.UUID]account, error) {
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.AccountID
	}
	rows, _ := tx.Query(ctx, `SELECT a.id, a.reconciliation_kind, a.status, ca.account_id IS NOT NULL, coalesce(ca.is_posting_blocked, false)
		FROM ledger.gl_account a LEFT JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $2
		WHERE a.id = ANY($1)`, ids, le)
	accounts := map[uuid.UUID]account{}
	var id uuid.UUID
	var a account
	if _, err := pgx.ForEachRow(rows, []any{&id, &a.kind, &a.status, &a.set, &a.blocked}, func() error { accounts[id] = a; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: check accounts in %s: %w", le, err)
	}
	return accounts, nil
}

// requirePostable rejects line i when its account does not exist, is not
// set up in the legal entity, is inactive or is blocked for posting.
func requirePostable(i int, field string, a account, ok bool) error {
	reason := ""
	switch {
	case !ok:
		reason = "there is no account with this id"
	case !a.set:
		reason = "the account is not set up in this legal entity"
	case a.status != "active":
		reason = "the account is " + a.status
	case a.blocked:
		reason = "the account is blocked for posting in this legal entity"
	}
	if reason != "" {
		return problem.Unprocessable("ledger.account_not_postable", "Line "+fmt.Sprint(i+1)+": "+reason+".",
			problem.FieldError{Field: field + ".account_id", Code: "not_postable", Message: reason})
	}
	return nil
}

// checkTaxCodes requires every tax code to be valid in the legal entity on
// the posting date.
func checkTaxCodes(ctx context.Context, tx pgx.Tx, le uuid.UUID, date time.Time, lines []Line) error {
	var codes []string
	for _, l := range lines {
		if l.TaxCode != nil {
			codes = append(codes, *l.TaxCode)
		}
	}
	if len(codes) == 0 {
		return nil
	}
	var missing *string
	if err := tx.QueryRow(ctx, `SELECT min(c) FROM unnest($1::text[]) AS c WHERE NOT EXISTS (SELECT FROM ledger.tax_code t
		WHERE t.legal_entity_id = $2 AND t.code = c AND t.valid_from <= $3 AND (t.valid_to IS NULL OR t.valid_to > $3))`, codes, le, date).Scan(&missing); err != nil {
		return fmt.Errorf("ledger: check tax codes in %s: %w", le, err)
	}
	if missing != nil {
		return problem.Unprocessable("ledger.unknown_tax_code", "Tax code "+*missing+" is not valid in this legal entity on the posting date.",
			problem.FieldError{Field: "lines.tax_code", Code: "unknown", Message: "a tax code of the legal entity"})
	}
	return nil
}

// rate is the average rate from one currency to another valid on the date:
// the latest rate of the pair in either direction, inverted when only the
// reverse pair is maintained.
func rate(ctx context.Context, tx pgx.Tx, from, to string, date time.Time) (string, error) {
	if from == to {
		return "1", nil
	}
	var r string
	err := tx.QueryRow(ctx, `SELECT CASE WHEN from_currency = $1 THEN rate ELSE round(1 / rate, 12) END::text
		FROM ledger.exchange_rate WHERE rate_type = 'average' AND valid_from <= $3
			AND ((from_currency = $1 AND to_currency = $2) OR (from_currency = $2 AND to_currency = $1))
		ORDER BY valid_from DESC, from_currency = $1 DESC LIMIT 1`, from, to, date).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("No average exchange rate from %s to %s is valid on %s.", from, to, date.Format(time.DateOnly)),
			problem.FieldError{Field: "posting_date", Code: "no_exchange_rate", Message: "record the rate first"})
	}
	if err != nil {
		return r, fmt.Errorf("ledger: rate %s/%s on %s: %w", from, to, date.Format(time.DateOnly), err)
	}
	return r, nil
}

var errTooLarge = problem.Unprocessable("ledger.amount_too_large", "A converted amount is too large to post.")

// errTotalTooLarge refuses amounts whose total is out of the range of a
// posted amount (money.ErrOverflow): checked here, it is a refusal of the
// request rather than a failure of the balance trigger at commit.
func errTotalTooLarge(field string) error {
	return problem.Unprocessable("ledger.amount_too_large", "The amounts add up to more than can be posted.",
		problem.FieldError{Field: field, Code: "too_large", Message: "amounts whose total is in range"})
}

// netsToZero reports whether amounts add up to exactly zero; a total out
// of range does not.
func netsToZero(amounts ...money.Minor) bool {
	sum, err := money.Sum(amounts...)
	return err == nil && sum == 0
}

// convert translates minor units at the rate (money.Convert); an amount
// out of range is too large to post.
func convert(minor money.Minor, r string, fromExp, toExp int) (money.Minor, error) {
	v, err := money.Convert(minor, r, fromExp, toExp)
	return v, convertErr(err)
}

// convertErr maps a failed conversion: out of range is the caller's
// amount, anything else (a rate or exponent the database never stores) is
// an internal error.
func convertErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, money.ErrOverflow):
		return errTooLarge
	}
	return fmt.Errorf("ledger: convert: %w", err)
}

// postJournalEntry validates and posts a manual journal entry. Local and
// group amounts are converted at the average rates valid on the posting
// date, and rounding differences go to the largest line.
func (e *Endpoints) postJournalEntry(ctx context.Context, in NewEntry) (Entry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Entry{}, err
	}
	ledger, err := ledgerID(ctx, tx, in.LedgerID)
	if err != nil {
		return Entry{}, err
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return Entry{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency, le.LocalCurrency, group)
	if err != nil {
		return Entry{}, err
	}
	expTC, ok := exps[in.Currency]
	if !ok {
		return Entry{}, problem.Unprocessable("ledger.unknown_currency", "The currency is not known.",
			problem.FieldError{Field: "currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"})
	}
	lines, err := entryLines(in, expTC)
	if err != nil {
		return Entry{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, true); err != nil {
		return Entry{}, err
	}
	if err := checkTaxCodes(ctx, tx, le.ID, in.PostingDate, lines); err != nil {
		return Entry{}, err
	}
	rateLC, err := rate(ctx, tx, in.Currency, le.LocalCurrency, in.PostingDate)
	if err != nil {
		return Entry{}, err
	}
	rateGC, err := rate(ctx, tx, le.LocalCurrency, group, in.PostingDate)
	if err != nil {
		return Entry{}, err
	}
	if err := convertLines(lines, rateLC, rateGC, expTC, exps[le.LocalCurrency], exps[group]); err != nil {
		return Entry{}, err
	}
	posted, err := e.post(ctx, tx, p, draft{entity: le, ledgerID: ledger, documentType: in.DocumentType, documentDate: in.DocumentDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: group, rateTCLC: rateLC,
		rateLCGC: rateGC, reference: in.Reference, headerText: in.HeaderText, sourceContext: cmp.Or(in.SourceContext, "ledger"),
		sourceDocumentID: in.SourceDocumentID, lines: lines})
	if err != nil {
		return Entry{}, err
	}
	return posted, audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: posted.ID,
		Outcome: "succeeded", Detail: map[string]any{"document_number": posted.DocumentNumber, "fiscal_year": posted.FiscalYear, "lines": len(lines),
			"source_context": posted.SourceContext}})
}

// entryLines parses the lines of a manual entry: amounts in the entry's
// currency with its minor-unit digits, never zero, adding up to zero.
func entryLines(in NewEntry, expTC int) ([]Line, error) {
	lines := make([]Line, len(in.Lines))
	amounts := make([]money.Minor, len(in.Lines))
	for i, l := range in.Lines {
		field := fmt.Sprintf("lines[%d].amount", i)
		if l.Currency != in.Currency {
			return nil, problem.Unprocessable("ledger.currency_mismatch", "Every line amount must be in the entry's currency "+in.Currency+".",
				problem.FieldError{Field: field + ".currency", Code: "mismatch", Message: in.Currency})
		}
		tc, err := money.ParseAmount(l.Amount, expTC)
		if err != nil {
			return nil, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", in.Currency, expTC),
				problem.FieldError{Field: field + ".amount", Code: "invalid", Message: err.Error()})
		}
		if tc == 0 {
			return nil, problem.Unprocessable("ledger.zero_amount", "Line amounts are never zero.",
				problem.FieldError{Field: field + ".amount", Code: "zero", Message: "debit positive, credit negative"})
		}
		amounts[i] = tc
		lines[i] = Line{AccountID: l.AccountID, TC: tc, PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text}
	}
	sum, err := money.Sum(amounts...)
	if err != nil {
		return nil, errTotalTooLarge("lines")
	}
	if sum != 0 {
		return nil, problem.Unprocessable("ledger.unbalanced", "Debits and credits differ by "+sum.Plain(expTC)+" "+in.Currency+".",
			problem.FieldError{Field: "lines", Code: "unbalanced", Message: "the line amounts must add up to zero"})
	}
	return lines, nil
}

// convertLines sets the local amounts, converted from the transaction
// amounts, and the group amounts, converted from the local ones; each
// currency's rounding difference goes to its largest line.
func convertLines(lines []Line, rateLC, rateGC string, expTC, expLC, expGC int) error {
	tcs := make([]money.Minor, len(lines))
	for i, l := range lines {
		tcs[i] = l.TC
	}
	lcs, err := money.ConvertAll(tcs, rateLC, expTC, expLC)
	if err != nil {
		return convertErr(err)
	}
	gcs, err := money.ConvertAll(lcs, rateGC, expLC, expGC)
	if err != nil {
		return convertErr(err)
	}
	for i := range lines {
		lines[i].LC, lines[i].GC = lcs[i], gcs[i]
	}
	return nil
}

// reverseJournalEntry posts the reversal of an entry: every amount negated
// in all three currencies at the original rates, on the original posting
// date unless another is given. Reversals and entries with cleared lines
// cannot be reversed. The reversal clears the entry's open items against
// its own (clearReversal).
func (e *Endpoints) reverseJournalEntry(ctx context.Context, id uuid.UUID, ifMatch func(Entry) error, postingDate *time.Time, headerText *string) (Entry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	orig, err := reversibleEntry(ctx, tx, id, ifMatch)
	if err != nil {
		return Entry{}, err
	}
	rev, err := e.postReversal(ctx, tx, p, orig, postingDate, headerText)
	if err != nil {
		return Entry{}, err
	}
	if err := recordReversal(ctx, tx, p, orig, rev); err != nil {
		return Entry{}, err
	}
	return rev, audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.reverse", ObjectType: "ledger.journal_entry", ObjectID: orig.ID,
		Outcome: "succeeded", Detail: map[string]any{"reversal_entry_id": rev.ID, "reversal_document_number": rev.DocumentNumber}})
}

// reversibleEntry locks the entry's bucket and reads the entry, refusing
// it when ifMatch does, when it cannot be reversed, or when a run holds
// one of its items. Locked before reading, so requireReversible sees any
// clearing of the entry that committed while this waited.
func reversibleEntry(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(Entry) error) (Entry, error) {
	if err := lockEntries(ctx, tx, false, []uuid.UUID{id}); err != nil {
		return Entry{}, err
	}
	orig, err := getEntry(ctx, tx, id)
	if err != nil {
		return Entry{}, err
	}
	if err := ifMatch(orig); err != nil {
		return Entry{}, err
	}
	if err := requireReversible(ctx, tx, orig); err != nil {
		return Entry{}, err
	}
	if err := requireUnreserved(ctx, tx, orig); err != nil {
		return Entry{}, err
	}
	return orig, nil
}

// postReversal posts the reversing entry of orig, on postingDate or the
// original posting date, with headerText or "Reversal of" the original's
// number.
func (e *Endpoints) postReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orig Entry, postingDate *time.Time, headerText *string) (Entry, error) {
	le, err := e.legalEntity(ctx, orig.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Entry{}, err
	}
	date := orig.PostingDate
	if postingDate != nil {
		date = *postingDate
	}
	lines := reversalLines(orig)
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	if headerText == nil {
		text := "Reversal of " + orig.DocumentNumber
		headerText = &text
	}
	return e.post(ctx, tx, p, draft{entity: le, ledgerID: orig.LedgerID, documentType: orig.DocumentType, documentDate: orig.DocumentDate,
		postingDate: date, currencyTC: orig.CurrencyTC, currencyLC: orig.CurrencyLC, currencyGC: orig.CurrencyGC, rateTCLC: orig.RateTCLC,
		rateLCGC: orig.RateLCGC, reference: orig.Reference, headerText: headerText, sourceContext: orig.SourceContext,
		sourceDocumentID: orig.SourceDocumentID, reversalOf: &orig, lines: lines})
}

// recordReversal links the reversal to its original, publishes
// JournalEntryReversed and clears the original's open items against the
// reversal's.
func recordReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orig, rev Entry) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.reversal (tenant_id, original_year, original_entry_id, reversal_year, reversal_entry_id)
		VALUES ($1, $2, $3, $4, $5)`, p.TenantID, orig.FiscalYear, orig.ID, rev.FiscalYear, rev.ID); err != nil {
		return fmt.Errorf("ledger: reverse entry %s: record reversal %s: %w", orig.ID, rev.ID, err)
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "journal_entry_reversed", AggregateType: "ledger.journal_entry", AggregateID: orig.ID,
		Payload: map[string]any{"journal_entry_id": orig.ID, "fiscal_year": orig.FiscalYear, "legal_entity_id": orig.LegalEntityID,
			"document_number": orig.DocumentNumber, "reversal_entry_id": rev.ID, "reversal_fiscal_year": rev.FiscalYear,
			"reversal_document_number": rev.DocumentNumber, "reversal_posting_date": rev.PostingDate.Format(time.DateOnly)}}); err != nil {
		return err
	}
	// After the reversal's event, so consumers learn of the cause first.
	return clearReversal(ctx, tx, p, orig, rev)
}

// requireReversible refuses to reverse a reversed entry, a reversal, or an
// entry with cleared lines.
func requireReversible(ctx context.Context, tx pgx.Tx, orig Entry) error {
	switch {
	case orig.ReversedByID != nil:
		return problem.Conflict("ledger.already_reversed", "The journal entry has already been reversed.")
	case orig.ReversalOfID != nil:
		return problem.Conflict("ledger.cannot_reverse_reversal", "The journal entry is itself a reversal; post a new entry instead.")
	}
	var cleared bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.clearing_item WHERE fiscal_year = $1 AND journal_entry_id = $2)`,
		orig.FiscalYear, orig.ID).Scan(&cleared); err != nil {
		return fmt.Errorf("ledger: reverse entry %s: %w", orig.ID, err)
	}
	if cleared {
		return problem.Conflict("ledger.entry_cleared", "Lines of the journal entry are cleared; an entry with cleared lines cannot be reversed.")
	}
	return nil
}

// requireOwnEntry refuses to reverse, through the ledger's :reverse, an
// entry a context posted for a document that has a reversal of its own:
// only the owner knows what else the reversal must change (a supplier
// invoice's status, its permission, the payment runs holding its item), so
// it reverses the entry through Postings.Reverse. Today that is payables
// (supplier-invoices/{id}:reverse). Entries of contexts without such a
// reversal stay the ledger's to reverse: manual ones, those a migration run
// loaded (their source document is the load run), and payments (whose
// entries clear their items, so requireReversible refuses them anyway).
//
// The contexts reversible here are listed, so a context that starts
// posting is refused until it is decided whether it owns its reversals.
func requireOwnEntry(orig Entry) error {
	switch {
	case orig.SourceContext == "ledger" || orig.SourceContext == "integration" || orig.SourceContext == "payments":
		return nil
	case orig.SourceContext == "payables" && orig.SourceDocumentID != nil:
		return problem.Conflict("ledger.entry_owned_elsewhere", "The journal entry was posted for supplier invoice "+orig.SourceDocumentID.String()+
			"; reverse it with POST /v1/supplier-invoices/"+orig.SourceDocumentID.String()+":reverse.")
	}
	return problem.Conflict("ledger.entry_owned_elsewhere", "The journal entry was posted by the "+orig.SourceContext+
		" context, and no reversal path exists yet for that context's entries, so it cannot be reversed.")
}

// reversalLines are the entry's lines with every amount and quantity
// negated.
func reversalLines(orig Entry) []Line {
	lines := make([]Line, len(orig.Lines))
	for i, l := range orig.Lines {
		lines[i] = Line{AccountID: l.AccountID, TC: -l.TC, LC: -l.LC, GC: -l.GC, PartnerID: l.PartnerID, TaxCode: l.TaxCode,
			Quantity: negate(l.Quantity), Unit: l.Unit, Text: l.Text}
	}
	return lines
}

// negate flips the sign of a decimal quantity.
func negate(q *string) *string {
	if q == nil {
		return nil
	}
	v := "-" + *q
	if strings.HasPrefix(*q, "-") {
		v = (*q)[1:]
	}
	return &v
}
