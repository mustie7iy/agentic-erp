package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
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
	problem.RegisterConstraint("account_determination_gr_ir_fixed", func() *problem.Problem {
		return problem.Conflict("ledger.gr_ir_account_fixed", "The legal entity's GR/IR account cannot change once determined: its positions are the lines on it.")
	})
	problem.RegisterConstraint("journal_line_gr_ir_order_item", func() *problem.Problem {
		return problem.Unprocessable("ledger.automatic_account", "The GR/IR account is posted only by goods receipts and invoices, which name the order item of each line.",
			problem.FieldError{Field: "lines.account_id", Code: "automatic", Message: "an account other than the legal entity's GR/IR account"})
	})
	problem.RegisterConstraint("account_determination_down_payment_fixed", func() *problem.Problem {
		return problem.Conflict("ledger.down_payment_account_fixed", "The legal entity's down payment account cannot change once determined: its down payments are the lines on it.")
	})
	problem.RegisterConstraint("journal_line_special_gl_account", func() *problem.Problem {
		return problem.Unprocessable("ledger.special_gl_account", "A line is on the down payment account exactly when it is a down payment (special G/L A).",
			problem.FieldError{Field: "lines.special_gl", Code: "special_gl_account", Message: "A, on the down payment account"})
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
	// PostingGroupID is the posting group's first entry (the entry itself
	// on the first); LedgerGroupID the ledger group posted to (ADR-0045).
	// Both nil on entries posted before T52b.
	PostingGroupID *uuid.UUID
	LedgerGroupID  *uuid.UUID
	PostedAt       time.Time
	PostedBy       uuid.UUID
	ExpTC          int
	ExpLC          int
	ExpGC          int
	Lines          []Line
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
	// The purchase order item a GR/IR line belongs to (T33), with the
	// quantity it moves.
	PurchaseOrderID *uuid.UUID `json:"purchase_order_id"`
	POItemNo        *int32     `json:"po_item_no"`
	// SpecialGL is the special G/L indicator of a supplier line, A for a
	// down payment (ADR-0037), or nil for an ordinary line.
	SpecialGL *string `json:"special_gl"`
}

const entrySelect = `SELECT e.id, e.legal_entity_id, e.ledger_id, e.fiscal_year, e.fiscal_period, e.document_type, e.document_number,
		e.document_date, e.posting_date, e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc::text, e.rate_lc_gc::text,
		e.reference, e.header_text, e.source_context, e.source_document_id, e.reversal_of_id, e.reversal_of_year, r.reversal_entry_id,
		e.posting_group_id, e.ledger_group_id, e.posted_at, e.posted_by, ctc.exponent, clc.exponent, cgc.exponent,
		(SELECT json_agg(json_build_object('line_no', l.line_no, 'account_id', l.account_id, 'tc', l.amount_tc, 'lc', l.amount_lc,
			'gc', l.amount_gc, 'partner_id', l.partner_id, 'tax_code', l.tax_code, 'quantity', trim_scale(l.quantity)::text,
			'unit', l.uom_code, 'text', l.line_text, 'purchase_order_id', l.purchase_order_id, 'po_item_no', l.po_item_no,
			'special_gl', l.special_gl) ORDER BY l.line_no)
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
		&e.PostingGroupID, &e.LedgerGroupID, &e.PostedAt, &e.PostedBy, &e.ExpTC, &e.ExpLC, &e.ExpGC, &e.Lines), "scan journal entry")
	return e, err
}

// getEntry loads an entry of a legal entity the caller may act in.
func getEntry(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Entry, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Entry{}, err
	}
	e, err := scanEntry(tx.QueryRow(ctx, entrySelect+` WHERE e.id = $1`, id))
	if err = authz.Visible(err, access, e.LegalEntityID, problem.NotFoundOf("journal entry"), "ledger: read journal entry %s", id); err != nil {
		return Entry{}, err
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
	entity tenancy.Entity
	// ledgers are the ledgers posted to, the representative first; nil:
	// every ledger the company keeps. ledgerGroup is the group named.
	ledgers          []uuid.UUID
	ledgerGroup      *uuid.UUID
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
	// reversedCopies are the copies of reversalOf by ledger, each reversed
	// by the reversal's copy in that ledger.
	reversedCopies map[uuid.UUID]uuid.UUID
	lines          []Line
}

// target is a ledger a posting goes to, with the status of its period.
type target struct {
	ledger  uuid.UUID
	code    string
	leading bool
	status  *string
}

// postingPeriod finds the open posting period of the posting date in each
// ledger posted to: ledgers, the representative first, or every ledger the
// company keeps, the leading one first, when nil (ADR-0045). It holds each
// period's status row FOR SHARE until the transaction ends. A close locks
// the row FOR UPDATE (changePeriod), so a posting waits for a close under
// way and then reads the committed status, and a close waits for the
// postings under way: no entry lands in a period whose close committed
// first. The lock comes after the entry buckets and before the number range
// (03-data-model, entry locks); a close takes nothing else, so no cycle.
func postingPeriod(ctx context.Context, tx pgx.Tx, e tenancy.Entity, ledgers []uuid.UUID, date time.Time) (int16, int16, []target, error) {
	var year, number int16
	day := date.Format(time.DateOnly)
	err := tx.QueryRow(ctx, `SELECT fiscal_year, period FROM ledger.fiscal_period
		WHERE calendar_id = $1 AND NOT is_special AND $2 BETWEEN start_date AND end_date`,
		e.FiscalCalendarID, date).Scan(&year, &number)
	var targets []target
	if err == nil {
		// FOR SHARE in a lateral subquery: it cannot lock the nullable side of an outer join.
		rows, _ := tx.Query(ctx, `SELECT l.id, l.code, l.is_leading, s.status FROM ledger.ledger l
			LEFT JOIN LATERAL (SELECT ps.status FROM ledger.period_status ps
				WHERE ps.legal_entity_id = $1 AND ps.ledger_id = l.id AND ps.fiscal_year = $2 AND ps.period = $3 FOR SHARE) s ON true
			WHERE CASE WHEN $4::uuid[] IS NULL THEN ledger.company_keeps($1, l.id) ELSE l.id = ANY($4) END
			ORDER BY array_position($4, l.id), NOT l.is_leading, l.code`, e.ID, year, number, ledgers)
		var t target
		_, err = pgx.ForEachRow(rows, []any{&t.ledger, &t.code, &t.leading, &t.status}, func() error { targets = append(targets, t); return nil })
	}
	missing := errors.Is(err, pgx.ErrNoRows) || err == nil && len(targets) == 0
	for _, t := range targets {
		missing = missing || t.status == nil
	}
	switch {
	case missing:
		return 0, 0, nil, noFiscalPeriod("posting_date", "No fiscal period of the legal entity covers "+day+
			"; a fiscal year is set up by a number range for it in the configuration.")
	case err != nil:
		return 0, 0, nil, fmt.Errorf("ledger: posting period of %s on %s: %w", e.ID, day, err)
	}
	for _, t := range targets {
		what := "postings dated " + day
		if !t.leading {
			what += " in ledger " + t.code
		}
		if err := mustBeOpen(year, number, *t.status, what); err != nil {
			return 0, 0, nil, err
		}
	}
	return year, number, targets, nil
}

// noFiscalPeriod refuses a document dated outside every fiscal period;
// field names the request field that holds the date.
func noFiscalPeriod(field, detail string) error {
	return problem.Unprocessable("ledger.no_fiscal_period", detail,
		problem.FieldError{Field: field, Code: "no_fiscal_period", Message: "a date in a fiscal year that is set up"})
}

// mustBeOpen refuses a document dated in a period that is not open, closed
// or locked alike; what names the documents ("postings dated ...").
func mustBeOpen(year, number int16, status, what string) error {
	if status == Open {
		return nil
	}
	return problem.Conflict("ledger.period_not_open", fmt.Sprintf("Period %d/%d is %s; %s need it open.", year, number, status, what))
}

// post inserts a balanced entry with its lines in each ledger the draft
// goes to, as linked entries of one posting group under one document
// number (ADR-0045), publishes JournalEntryPosted for each and queues each
// for the balance projector. It returns the first entry, the
// representative ledger's: the leading ledger's when the draft names no
// ledgers, the one open items and clearings refer to.
func (e *Endpoints) post(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d draft) (Entry, error) {
	year, number, targets, err := postingPeriod(ctx, tx, d.entity, d.ledgers, d.postingDate)
	if err != nil {
		return Entry{}, err
	}
	docNumber, err := e.Numbers.Next(ctx, d.entity.ID, "journal_"+strings.ToLower(d.documentType), year)
	if err != nil {
		return Entry{}, err
	}
	group := uuid.Must(uuid.NewV7())
	cols := lineColumnsOf(d.lines)
	var first Entry
	for i, t := range targets {
		id := group
		if i > 0 {
			id = uuid.Must(uuid.NewV7())
		}
		posted, err := insertEntry(ctx, tx, p, d, cols, header{id: id, group: group, ledger: t.ledger, year: year, period: number, number: docNumber})
		if err != nil {
			return Entry{}, err
		}
		if i == 0 {
			first = posted
		}
	}
	return first, nil
}

// header is what one entry of a posting group is posted with.
type header struct {
	id, group, ledger uuid.UUID
	year, period      int16
	number            string
}

// lineColumns are a draft's lines as the columns insertEntry inserts.
type lineColumns struct {
	nos                                       []int32
	items                                     []*int32
	accounts                                  []uuid.UUID
	partners, orders                          []*uuid.UUID
	tcs, lcs, gcs                             []money.Minor
	taxes, quantities, units, texts, specials []*string
}

func lineColumnsOf(lines []Line) lineColumns {
	n := len(lines)
	c := lineColumns{nos: make([]int32, n), items: make([]*int32, n), accounts: make([]uuid.UUID, n), partners: make([]*uuid.UUID, n),
		orders: make([]*uuid.UUID, n), tcs: make([]money.Minor, n), lcs: make([]money.Minor, n), gcs: make([]money.Minor, n),
		taxes: make([]*string, n), quantities: make([]*string, n), units: make([]*string, n), texts: make([]*string, n), specials: make([]*string, n)}
	for i, l := range lines {
		c.nos[i], c.accounts[i], c.partners[i] = int32(i+1), l.AccountID, l.PartnerID
		c.tcs[i], c.lcs[i], c.gcs[i] = l.TC, l.LC, l.GC
		c.taxes[i], c.quantities[i], c.units[i], c.texts[i] = l.TaxCode, l.Quantity, l.Unit, l.Text
		c.orders[i], c.items[i], c.specials[i] = l.PurchaseOrderID, l.POItemNo, l.SpecialGL
	}
	return c
}

// insertEntry inserts one entry of a posting group with its lines, reads
// it back, publishes JournalEntryPosted and queues it for the projector.
func insertEntry(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d draft, c lineColumns, h header) (Entry, error) {
	var reversalOf *uuid.UUID
	var reversalYear *int16
	if d.reversalOf != nil {
		of := d.reversalOf.ID
		if copied, ok := d.reversedCopies[h.ledger]; ok {
			of = copied
		}
		reversalOf, reversalYear = &of, &d.reversalOf.FiscalYear
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period,
			document_type, document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc,
			reference, header_text, source_context, source_document_id, reversal_of_id, reversal_of_year, posted_by, posting_group_id, ledger_group_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::numeric, $15::numeric, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
		p.TenantID, h.year, h.id, d.entity.ID, h.ledger, h.period, d.documentType, h.number, d.documentDate, d.postingDate,
		d.currencyTC, d.currencyLC, d.currencyGC, d.rateTCLC, d.rateLCGC, d.reference, d.headerText, d.sourceContext, d.sourceDocumentID,
		reversalOf, reversalYear, p.ID, h.group, d.ledgerGroup); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: %w", h.id, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id,
			fiscal_period, posting_date, account_id, amount_tc, amount_lc, amount_gc, partner_id, tax_code, quantity, uom_code, line_text,
			purchase_order_id, po_item_no, special_gl)
		SELECT $1, $2, $3, n, $4, $5, $6, $7, a, tc, lc, gc, pa, tx, q::numeric, u, t, o, i, s
		FROM unnest($8::int[], $9::uuid[], $10::bigint[], $11::bigint[], $12::bigint[], $13::uuid[], $14::text[], $15::text[], $16::text[], $17::text[],
			$18::uuid[], $19::int[], $20::text[]) AS x(n, a, tc, lc, gc, pa, tx, q, u, t, o, i, s)`,
		p.TenantID, h.year, h.id, d.entity.ID, h.ledger, h.period, d.postingDate, c.nos, c.accounts, c.tcs, c.lcs, c.gcs, c.partners,
		c.taxes, c.quantities, c.units, c.texts, c.orders, c.items, c.specials); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: lines: %w", h.id, err)
	}
	// Read back without the caller's company filter: the caller was let post
	// in the legal entity, or an approval it decides authorises the posting
	// (T35e).
	posted, err := scanEntry(tx.QueryRow(ctx, entrySelect+` WHERE e.id = $1`, h.id))
	if err != nil {
		return Entry{}, fmt.Errorf("ledger: read posted entry %s: %w", h.id, err)
	}
	event, err := outbox.Add(ctx, postedEvent(posted))
	if err != nil {
		return Entry{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.balance_projection (tenant_id, event_id, fiscal_year, journal_entry_id) VALUES ($1, $2, $3, $4)`,
		p.TenantID, event, h.year, h.id); err != nil {
		return Entry{}, fmt.Errorf("ledger: post entry %s: queue projection: %w", h.id, err)
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
		if l.PurchaseOrderID != nil {
			line["purchase_order_id"], line["po_item_no"] = *l.PurchaseOrderID, *l.POItemNo
		}
		if l.SpecialGL != nil {
			line["special_gl"] = *l.SpecialGL
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
			"reversal_of_year": e.ReversalOfYear, "posting_group_id": e.PostingGroupID, "ledger_group_id": e.LedgerGroupID, "lines": lines,
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
	SpecialGL      *string // set by the posting contexts only (ADR-0037)
}

// NewEntry is a manual journal entry to post.
type NewEntry struct {
	LegalEntityID uuid.UUID
	// LedgerID names the one ledger the entry posts to, LedgerGroup (a
	// code) a ledger group; neither: every ledger the company keeps.
	LedgerID     *uuid.UUID
	LedgerGroup  *string
	DocumentType string
	DocumentDate time.Time
	PostingDate  time.Time
	Currency     string
	Reference    *string
	HeaderText   *string
	Lines        []NewLine
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
	return e.activeEntity(ctx, access, id, field)
}

// activeEntity loads an active legal entity within access.
func (e *Endpoints) activeEntity(ctx context.Context, access authz.Access, id uuid.UUID, field string) (tenancy.Entity, error) {
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
	downPayment  bool // the legal entity's down payment account (ADR-0037)
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
		if err := requireSpecialGL(i, field, l, a); err != nil {
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
	rows, _ := tx.Query(ctx, `SELECT a.id, a.reconciliation_kind, a.status, ca.account_id IS NOT NULL, coalesce(ca.is_posting_blocked, false),
			EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $2 AND d.key = 'down_payment'
				AND d.account_id = a.id)
		FROM ledger.gl_account a LEFT JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $2
		WHERE a.id = ANY($1)`, ids, le)
	accounts := map[uuid.UUID]account{}
	var id uuid.UUID
	var a account
	if _, err := pgx.ForEachRow(rows, []any{&id, &a.kind, &a.status, &a.set, &a.blocked, &a.downPayment}, func() error { accounts[id] = a; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: check accounts in %s: %w", le, err)
	}
	return accounts, nil
}

// requireSpecialGL holds line i to ADR-0037: a line is on the legal
// entity's down payment account exactly when it is a down payment
// (special G/L A).
func requireSpecialGL(i int, field string, l Line, a account) error {
	special := l.SpecialGL != nil
	switch {
	case a.downPayment && !special:
		return problem.Unprocessable("ledger.special_gl_required", fmt.Sprintf("Line %d posts to the down payment account, which takes only down payments (special G/L A).", i+1),
			problem.FieldError{Field: field + ".account_id", Code: "special_gl_required", Message: "the down payment account takes down payments only"})
	case special && (!a.downPayment || *l.SpecialGL != "A"):
		return problem.Unprocessable("ledger.special_gl_account", fmt.Sprintf("Line %d is a down payment (special G/L A), posted only to the down payment account.", i+1),
			problem.FieldError{Field: field + ".special_gl", Code: "special_gl_account", Message: "A, on the down payment account"})
	}
	return nil
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

// exchange is a rate from one currency to another. exact is what amounts
// are converted at: the recorded rate, or the exact reciprocal of the
// reverse pair's. shown is the same rate rounded to the 12 decimals of
// numeric(28,12), what a journal entry records and people read. Converting
// at shown instead, as before F19, moved amounts by a minor unit or more
// (3,000,000,000,000.02 at 1/3) and, for a recorded rate above 2e12 whose
// rounded reciprocal is 0, converted every amount to 0.
type exchange struct {
	exact *big.Rat
	shown string
}

// rate is the average rate from one currency to another valid on the date:
// the latest rate of the pair in either direction, its reciprocal when
// only the reverse pair is maintained.
func rate(ctx context.Context, tx pgx.Tx, from, to string, date time.Time) (exchange, error) {
	if from == to {
		return exchange{exact: big.NewRat(1, 1), shown: "1"}, nil
	}
	var recorded, shown string
	var forward bool
	err := tx.QueryRow(ctx, `SELECT rate::text, from_currency = $1, CASE WHEN from_currency = $1 THEN rate ELSE round(1 / rate, 12) END::text
		FROM ledger.exchange_rate WHERE rate_type = 'average' AND valid_from <= $3
			AND ((from_currency = $1 AND to_currency = $2) OR (from_currency = $2 AND to_currency = $1))
		ORDER BY valid_from DESC, from_currency = $1 DESC LIMIT 1`, from, to, date).Scan(&recorded, &forward, &shown)
	if errors.Is(err, pgx.ErrNoRows) {
		return exchange{}, problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("No average exchange rate from %s to %s is valid on %s.", from, to, date.Format(time.DateOnly)),
			problem.FieldError{Field: "posting_date", Code: "no_exchange_rate", Message: "record the rate first"})
	}
	if err != nil {
		return exchange{}, fmt.Errorf("ledger: rate %s/%s on %s: %w", from, to, date.Format(time.DateOnly), err)
	}
	// Read in money's plain grammar: the column's rate > 0 check lets 'NaN'
	// through, which CreateRate never stores but a direct write could.
	exact, err := money.ParsePlain(recorded)
	if err != nil || exact.Sign() <= 0 {
		return exchange{}, fmt.Errorf("ledger: rate %s/%s on %s: stored rate %q is not a positive decimal", from, to, date.Format(time.DateOnly), recorded)
	}
	x := exchange{exact: exact, shown: shown}
	if !forward {
		x.exact.Inv(x.exact) // never zero: checked above
	}
	return x, nil
}

// postingRates are the rates a posting converts at, from its transaction
// currency to the local one and from that to the group's. A rate whose
// 12-decimal form is zero, the reciprocal of a recorded rate above 2e12,
// cannot be recorded on the entry, so the posting is refused (F19).
func postingRates(ctx context.Context, tx pgx.Tx, tc, lc, gc string, date time.Time) (toLC, toGC exchange, err error) {
	if toLC, err = rate(ctx, tx, tc, lc, date); err != nil {
		return exchange{}, exchange{}, err
	}
	if toGC, err = rate(ctx, tx, lc, gc, date); err != nil {
		return exchange{}, exchange{}, err
	}
	for _, r := range []struct {
		x        exchange
		from, to string
	}{{toLC, tc, lc}, {toGC, lc, gc}} {
		if money.ParseRat(r.x.shown).Sign() == 0 {
			return exchange{}, exchange{}, problem.Unprocessable("ledger.rate_not_recordable",
				fmt.Sprintf("The average rate from %s to %s, the reciprocal of the recorded %s/%s rate, is zero to the 12 decimals a journal entry records.",
					r.from, r.to, r.to, r.from),
				problem.FieldError{Field: "currency", Code: "rate_not_recordable", Message: "record a " + r.from + "/" + r.to + " rate"})
		}
	}
	return toLC, toGC, nil
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

// convert translates minor units at the exact rate (money.ConvertAt); an
// amount out of range is too large to post.
func convert(minor money.Minor, r exchange, fromExp, toExp int) (money.Minor, error) {
	v, err := money.ConvertAt(minor, r.exact, fromExp, toExp)
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

// prepared is a manual entry checked as far as it can be before it is
// posted: its legal entity, ledger, currencies and lines, with accounts,
// partners and tax codes valid; the open period and the rates depend on
// when it is posted.
type prepared struct {
	le     tenancy.Entity
	choice ledgerChoice
	group  string
	exps   currency.Exponents
	expTC  int
	lines  []Line // amounts in transaction currency only
}

// prepare checks a manual entry as parking and posting both need it, in a
// legal entity the caller may act in.
func (e *Endpoints) prepare(ctx context.Context, tx pgx.Tx, in NewEntry) (prepared, error) {
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return prepared{}, err
	}
	return e.prepareFor(ctx, tx, le, in)
}

// prepareFor checks a manual entry of the legal entity le.
func (e *Endpoints) prepareFor(ctx context.Context, tx pgx.Tx, le tenancy.Entity, in NewEntry) (prepared, error) {
	choice, err := chooseLedgers(ctx, tx, le.ID, in)
	if err != nil {
		return prepared{}, err
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return prepared{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency, le.LocalCurrency, group)
	if err != nil {
		return prepared{}, err
	}
	expTC, ok := exps[in.Currency]
	if !ok {
		return prepared{}, problem.Unprocessable("ledger.unknown_currency", "The currency is not known.",
			problem.FieldError{Field: "currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"})
	}
	lines, err := entryLines(in, expTC)
	if err != nil {
		return prepared{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, true); err != nil {
		return prepared{}, err
	}
	if err := checkTaxCodes(ctx, tx, le.ID, in.PostingDate, lines); err != nil {
		return prepared{}, err
	}
	return prepared{le: le, choice: choice, group: group, exps: exps, expTC: expTC, lines: lines}, nil
}

// postJournalEntry validates and posts an entry another context posts
// through Postings: no approval limit applies to its documents' postings.
func (e *Endpoints) postJournalEntry(ctx context.Context, in NewEntry) (Entry, error) {
	return e.postEntry(ctx, in, nil)
}

// postDirect validates and posts a manual entry a person posts at once
// through the API, within its company's approval limit (T35f).
func (e *Endpoints) postDirect(ctx context.Context, in NewEntry) (Entry, error) {
	return e.postEntry(ctx, in, e.withinLimit)
}

// postEntry validates and posts an entry; check, when set, may refuse it
// once it is converted, before anything is written.
func (e *Endpoints) postEntry(ctx context.Context, in NewEntry, check func(context.Context, pgx.Tx, prepared, NewEntry) error) (Entry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return Entry{}, err
	}
	return e.postPrepared(ctx, tx, p, v, in, check)
}

// postPrepared posts a prepared manual entry: local and group amounts are
// converted at the average rates valid on the posting date, and rounding
// differences go to the largest line.
func (e *Endpoints) postPrepared(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v prepared, in NewEntry,
	check func(context.Context, pgx.Tx, prepared, NewEntry) error) (Entry, error) {
	rateLC, rateGC, err := postingRates(ctx, tx, in.Currency, v.le.LocalCurrency, v.group, in.PostingDate)
	if err != nil {
		return Entry{}, err
	}
	if err := convertLines(v.lines, rateLC, rateGC, v.expTC, v.exps[v.le.LocalCurrency], v.exps[v.group]); err != nil {
		return Entry{}, err
	}
	if check != nil {
		if err := check(ctx, tx, v, in); err != nil {
			return Entry{}, err
		}
	}
	posted, err := e.post(ctx, tx, p, draft{entity: v.le, ledgers: v.choice.ledgers, ledgerGroup: v.choice.group, documentType: in.DocumentType, documentDate: in.DocumentDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: v.le.LocalCurrency, currencyGC: v.group, rateTCLC: rateLC.shown,
		rateLCGC: rateGC.shown, reference: in.Reference, headerText: in.HeaderText, sourceContext: cmp.Or(in.SourceContext, "ledger"),
		sourceDocumentID: in.SourceDocumentID, lines: v.lines})
	if err != nil {
		return Entry{}, err
	}
	return posted, audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: posted.ID,
		Outcome: "succeeded", Detail: map[string]any{"document_number": posted.DocumentNumber, "fiscal_year": posted.FiscalYear, "lines": len(v.lines),
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
		lines[i] = Line{AccountID: l.AccountID, TC: tc, PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text,
			SpecialGL: l.SpecialGL}
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
func convertLines(lines []Line, rateLC, rateGC exchange, expTC, expLC, expGC int) error {
	tcs := make([]money.Minor, len(lines))
	for i, l := range lines {
		tcs[i] = l.TC
	}
	lcs, err := money.ConvertAllAt(tcs, rateLC.exact, expTC, expLC)
	if err != nil {
		return convertErr(err)
	}
	gcs, err := money.ConvertAllAt(lcs, rateGC.exact, expLC, expGC)
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
// number, in every ledger of orig's posting group: each copy is reversed
// by the reversal's copy in its ledger.
func (e *Endpoints) postReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, orig Entry, postingDate *time.Time, headerText *string) (Entry, error) {
	ledgers, copies, err := groupCopies(ctx, tx, orig)
	if err != nil {
		return Entry{}, err
	}
	le, err := e.legalEntity(ctx, orig.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Entry{}, err
	}
	date := orig.PostingDate
	if postingDate != nil {
		date = *postingDate
	}
	lines, err := grirAdjustments(ctx, tx, orig, reversalLines(orig))
	if err != nil {
		return Entry{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	if headerText == nil {
		text := "Reversal of " + orig.DocumentNumber
		headerText = &text
	}
	rev, err := e.post(ctx, tx, p, draft{entity: le, ledgers: ledgers, ledgerGroup: orig.LedgerGroupID, documentType: orig.DocumentType, documentDate: orig.DocumentDate,
		postingDate: date, currencyTC: orig.CurrencyTC, currencyLC: orig.CurrencyLC, currencyGC: orig.CurrencyGC, rateTCLC: orig.RateTCLC,
		rateLCGC: orig.RateLCGC, reference: orig.Reference, headerText: headerText, sourceContext: orig.SourceContext,
		sourceDocumentID: orig.SourceDocumentID, reversalOf: &orig, reversedCopies: copies, lines: lines})
	if err != nil {
		return Entry{}, err
	}
	return rev, project(ctx, tx, rev)
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
// (supplier-invoices/{id}:reverse), and procurement, whose goods receipts
// are reversed by a 102 movement posting its own WE entry
// (goods-receipts/{id}:reverse, T33). Entries of contexts without such a
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
	case orig.SourceContext == "procurement" && orig.SourceDocumentID != nil:
		return problem.Conflict("ledger.entry_owned_elsewhere", "The journal entry was posted for goods movement "+orig.SourceDocumentID.String()+
			"; goods receipts are reversed with POST /v1/goods-receipts/{receipt id}:reverse, which posts its own entry.")
	}
	return problem.Conflict("ledger.entry_owned_elsewhere", "The journal entry was posted by the "+orig.SourceContext+
		" context, and no reversal path exists yet for that context's entries, so it cannot be reversed.")
}

// reversalLines are the entry's lines with every amount and quantity
// negated, each on the order item of the line it reverses.
func reversalLines(orig Entry) []Line {
	lines := make([]Line, len(orig.Lines))
	for i, l := range orig.Lines {
		lines[i] = Line{AccountID: l.AccountID, TC: -l.TC, LC: -l.LC, GC: -l.GC, PartnerID: l.PartnerID, TaxCode: l.TaxCode,
			Quantity: negate(l.Quantity), Unit: l.Unit, Text: l.Text, PurchaseOrderID: l.PurchaseOrderID, POItemNo: l.POItemNo, SpecialGL: l.SpecialGL}
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
