package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Parked entry statuses (T35, migration 000052).
const (
	EntryParked          = "parked"
	EntryPendingApproval = "pending_approval"
	EntryPosted          = "posted"
	EntryRejected        = "rejected"
	EntryCancelled       = "cancelled"
)

// ParkedEntry is a manual journal entry parked before it is posted, with
// its lines in transaction currency (Line.TC; LC and GC are zero).
type ParkedEntry struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	LedgerID      uuid.UUID
	// LedgerSpecific: it posts to LedgerID alone; LedgerGroup: to that
	// ledger group (LedgerID its representative); neither: to every ledger
	// of its company (LedgerID the leading one).
	LedgerSpecific    bool
	LedgerGroup       *string
	DocumentType      string
	DocumentDate      time.Time
	PostingDate       time.Time
	TaxDate           *time.Time // nil: the posting date
	Currency          string
	Exp               int // the currency's minor-unit digits
	Reference         *string
	HeaderText        *string
	Status            string
	CreatedByKind     string
	OwnerID           *uuid.UUID
	ApprovalRequestID *uuid.UUID
	JournalFiscalYear *int16
	JournalEntryID    *uuid.UUID
	FiscalPeriod      *int16 // the special period it is to be posted into (ADR-0059 §3); nil: its posting date's
	Lines             []Line
	Debits            money.Minor // the debit total of Lines
	Limit             *ParkLimit  // why the ERP parked it instead of posting it; nil: a principal parked it
	TemplateID        *uuid.UUID  // the entry template it was parked from (ADR-0056)
	TemplateName      *string     // its name, as read
	OccurrenceDate    *time.Time  // the template's due date the ERP parked it for; nil: a principal parked it
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

// lineTable is a table of lines in transaction currency kept as a parked
// entry's are: ledger.parked_entry_line and ledger.entry_template_line.
type lineTable struct {
	table  string // in schema ledger
	parent string // the column naming the entry or template
	what   string // what the lines are of, for errors
}

var (
	parkedLines   = lineTable{table: "parked_entry_line", parent: "parked_entry_id", what: "parked entry"}
	templateLines = lineTable{table: "entry_template_line", parent: "template_id", what: "entry template"}
)

// name is the table's sanitized name.
func (t lineTable) name() string { return pgx.Identifier{"ledger", t.table}.Sanitize() }

// column is the parent column's sanitized name.
func (t lineTable) column() string { return pgx.Identifier{t.parent}.Sanitize() }

// read is a select list's expressions reading the live lines of the row
// alias names (as JSON, scanned into []Line) and their debit total.
func (t lineTable) read(alias string) string {
	where := ` WHERE l.tenant_id = ` + alias + `.tenant_id AND l.` + t.column() + ` = ` + alias + `.id AND NOT l.is_removed`
	return `coalesce((SELECT json_agg(json_build_object('line_no', l.line_no, 'account_id', l.account_id, 'tc', l.amount, 'lc', 0, 'gc', 0,
			'partner_id', l.partner_id, 'tax_code', l.tax_code, 'quantity', trim_scale(l.quantity)::text, 'unit', l.uom_code, 'text', l.line_text,
			'tax_base_tc', l.tax_base, 'tax_jurisdiction', l.tax_jurisdiction, 'cost_center_id', l.cost_center_id, 'profit_center_id', l.profit_center_id,
			'segment_id', l.segment_id, 'cost_center_code', cc.code, 'profit_center_code', pc.code, 'segment_code', sg.code,
			'trading_partner_id', l.trading_partner_id)
			ORDER BY l.line_no)
		 FROM ` + t.name() + ` l` + centerJoins + where + `), '[]'),
		(SELECT coalesce(sum(l.amount) FILTER (WHERE l.amount > 0), 0)::bigint FROM ` + t.name() + ` l` + where + `)`
}

var parkedSelect = `SELECT p.id, p.legal_entity_id, p.ledger_id, p.document_type, p.document_date, p.posting_date, p.tax_date, p.currency, c.exponent,
		p.reference, p.header_text, p.status, p.created_by_kind, p.owner_id, p.approval_request_id, p.journal_fiscal_year, p.journal_entry_id,
		p.version, p.created_at, p.created_by, p.updated_at, p.updated_by, p.ledger_specific,
		(SELECT g.code FROM ledger.ledger_group g WHERE g.tenant_id = p.tenant_id AND g.id = p.ledger_group_id),
		` + parkedLines.read("p") + `,
		p.limit_kind, p.limit_debits, p.limit_debits_currency, (SELECT x.exponent FROM ref.currency x WHERE x.code = p.limit_debits_currency),
		p.limit_amount, p.limit_currency, (SELECT x.exponent FROM ref.currency x WHERE x.code = p.limit_currency), p.template_id, p.occurrence_date,
		(SELECT t.name FROM ledger.entry_template t WHERE t.tenant_id = p.tenant_id AND t.id = p.template_id), p.fiscal_period
	FROM ledger.parked_entry p JOIN ref.currency c ON c.code = p.currency`

func scanParked(row pgx.Row) (ParkedEntry, error) {
	var pe ParkedEntry
	var kind, debitsCurrency, limitCurrency *string
	var debits, limit *money.Minor
	var debitsExp, limitExp *int
	err := row.Scan(&pe.ID, &pe.LegalEntityID, &pe.LedgerID, &pe.DocumentType, &pe.DocumentDate, &pe.PostingDate, &pe.TaxDate, &pe.Currency, &pe.Exp,
		&pe.Reference, &pe.HeaderText, &pe.Status, &pe.CreatedByKind, &pe.OwnerID, &pe.ApprovalRequestID, &pe.JournalFiscalYear, &pe.JournalEntryID,
		&pe.Version, &pe.CreatedAt, &pe.CreatedBy, &pe.UpdatedAt, &pe.UpdatedBy, &pe.LedgerSpecific, &pe.LedgerGroup, &pe.Lines, &pe.Debits,
		&kind, &debits, &debitsCurrency, &debitsExp, &limit, &limitCurrency, &limitExp, &pe.TemplateID, &pe.OccurrenceDate, &pe.TemplateName, &pe.FiscalPeriod)
	// The database holds the five together (parked_entry_limit_figures),
	// and a currency it keeps has its exponent.
	if err == nil && kind != nil {
		pe.Limit = &ParkLimit{Kind: *kind, Debits: *debits, DebitsCurrency: *debitsCurrency, DebitsExp: *debitsExp,
			Limit: *limit, LimitCurrency: *limitCurrency, LimitExp: *limitExp}
	}
	return pe, errs.Wrapf(err, "scan parked entry")
}

// getParked loads a parked entry of a legal entity the caller may act in.
func getParked(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ParkedEntry, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	pe, err := scanParked(tx.QueryRow(ctx, parkedSelect+` WHERE p.id = $1`, id))
	if err = authz.Visible(err, access, pe.LegalEntityID, problem.NotFoundOf("parked journal entry"), "ledger: read parked entry %s", id); err != nil {
		return ParkedEntry{}, err
	}
	return pe, nil
}

func init() {
	problem.RegisterConstraint("parked_entry_line_uom_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.unknown_unit", "A quantity's unit of measure is not known.",
			problem.FieldError{Field: "lines.quantity.unit", Code: "unknown", Message: "a unit of measure code from the reference data"})
	})
}

// parkEntry parks a manual entry, checked as a posting is except for the
// open period and the rates, which depend on when it is posted. Nothing is
// posted and no number is drawn: the number is the journal's, given when
// the entry is posted, so cancelled and rejected entries leave no gaps. A
// person owns what they park; an agent's entry has no owner until a person
// takes it on (T35d), so only a person can submit or post it.
func (e *Endpoints) parkEntry(ctx context.Context, in NewEntry) (ParkedEntry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return ParkedEntry{}, err
	}
	if err := checkSpecial(ctx, tx, v, in); err != nil {
		return ParkedEntry{}, err
	}
	return parkPrepared(ctx, tx, p, v, in, parking{})
}

// parking is how an entry is parked, besides what it holds.
type parking struct {
	// limit, when set, is the limit the ERP parks it by instead of posting
	// it (a service principal's entry, T61n), recorded with it and in its
	// audit record.
	limit *ParkLimit
	// template is the entry template it is parked from (ADR-0056), and
	// occurrence the template's due date when the ERP parks it, for owner,
	// the template's owner.
	template   *uuid.UUID
	occurrence *time.Time
	owner      *uuid.UUID
}

// ownerOf is who owns the entry the principal parks: a person what they
// park; the template's owner what the ERP parks for a due date; no one an
// agent's or a service's entry, until a person takes it on (T35d).
func (how parking) ownerOf(p reqctx.Principal) *uuid.UUID {
	if p.Kind == reqctx.KindHuman {
		return &p.ID
	}
	return how.owner
}

// parkPrepared parks a prepared entry as how says.
func parkPrepared(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v prepared, in NewEntry, how parking) (ParkedEntry, error) {
	if err := parkable(ctx, tx, v, in.PostingDate); err != nil {
		return ParkedEntry{}, err
	}
	id := uuid.Must(uuid.NewV7())
	var (
		kind, debitsCurrency, limitCurrency *string
		debits, amount                      *money.Minor
		extra                               = map[string]any{}
	)
	if limit := how.limit; limit != nil {
		kind, debits, debitsCurrency, amount, limitCurrency = &limit.Kind, &limit.Debits, &limit.DebitsCurrency, &limit.Limit, &limit.LimitCurrency
		extra = map[string]any{"limit_exceeded": limit.Kind, "debits": money.EventAmount(limit.Debits, limit.DebitsCurrency, limit.DebitsExp),
			"limit": money.EventAmount(limit.Limit, limit.LimitCurrency, limit.LimitExp)}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
			currency, reference, header_text, created_by_kind, owner_id, created_by, updated_by, ledger_specific, ledger_group_id, tax_date,
			limit_kind, limit_debits, limit_debits_currency, limit_amount, limit_currency, template_id, occurrence_date, fiscal_period)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
		p.TenantID, id, v.le.ID, v.choice.ledger, in.DocumentType, in.DocumentDate, in.PostingDate, in.Currency, in.Reference, in.HeaderText,
		p.Kind, how.ownerOf(p), p.ID, v.choice.specific, v.choice.group, in.TaxDate, kind, debits, debitsCurrency, amount, limitCurrency,
		how.template, how.occurrence, in.special); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: park entry %s: %w", id, err)
	}
	if err := putParkedLines(ctx, tx, p.TenantID, id, v.lines); err != nil {
		return ParkedEntry{}, err
	}
	// Read as written: the caller may act in its company (prepare), or is
	// the ERP parking a template's due date.
	pe, err := scanParked(tx.QueryRow(ctx, parkedSelect+` WHERE p.id = $1`, id))
	if err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: read parked entry %s: %w", id, err)
	}
	return pe, parkedChanged(ctx, pe, nil, "park", extra)
}

// parkable refuses, before anything is written, a prepared entry that
// could never be posted (refuseAutomatic), whose debits are out of range,
// or whose lines' assignment posting it on the date would refuse now
// (assign, without locking the centers: it is checked again when the
// entry is posted). A date in no fiscal year is refused when it is posted.
func parkable(ctx context.Context, tx pgx.Tx, v prepared, date time.Time) error {
	if err := refuseAutomatic(ctx, v.le.ID, v.lines); err != nil {
		return err
	}
	if _, err := debitTotal(v.lines); err != nil {
		return err
	}
	year, _, err := periodOn(ctx, tx, v.le.FiscalCalendarID, date)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return assign(ctx, tx, v.le.ID, year, date, slices.Clone(v.lines), false)
}

// refuseAutomatic refuses a line on the legal entity's GR/IR account, which
// only goods receipts and invoices post (T33): parked, it could never be
// posted. Posting is refused by the database (journal_line_gr_ir_order_item)
// with the same problem.
func refuseAutomatic(ctx context.Context, le uuid.UUID, lines []Line) error {
	grir, ok, err := Accounts{}.Determination(ctx, le, "gr_ir")
	if err != nil || !ok {
		return err
	}
	for i, l := range lines {
		if l.AccountID == grir {
			return problem.Unprocessable("ledger.automatic_account", "The GR/IR account is posted only by goods receipts and invoices, which name the order item of each line.",
				problem.FieldError{Field: fmt.Sprintf("lines[%d].account_id", i), Code: "automatic", Message: "an account other than the legal entity's GR/IR account"})
		}
	}
	return nil
}

// The statements writing a parked entry's or a template's lines, as
// constants: the problem codes of their tables' constraints are traced to
// the functions that run them (api TestProblemCodes).
const (
	lineValues = `line_no, account_id, amount, partner_id,
			tax_code, quantity, uom_code, line_text, tax_base, tax_jurisdiction, cost_center_id, profit_center_id, segment_id, trading_partner_id)
		SELECT $1, $2, n, a, m, pa, tx, q::numeric, u, t, b, j, cc, pc, sg, tp
		FROM unnest($3::int[], $4::uuid[], $5::bigint[], $6::uuid[], $7::text[], $8::text[], $9::text[], $10::text[], $11::bigint[], $12::text[],
			$13::uuid[], $14::uuid[], $15::uuid[], $16::uuid[]) AS x(n, a, m, pa, tx, q, u, t, b, j, cc, pc, sg, tp)`
	lineUpdate = ` DO UPDATE SET account_id = EXCLUDED.account_id, amount = EXCLUDED.amount,
			partner_id = EXCLUDED.partner_id, tax_code = EXCLUDED.tax_code, quantity = EXCLUDED.quantity, uom_code = EXCLUDED.uom_code,
			line_text = EXCLUDED.line_text, tax_base = EXCLUDED.tax_base, tax_jurisdiction = EXCLUDED.tax_jurisdiction,
			cost_center_id = EXCLUDED.cost_center_id, profit_center_id = EXCLUDED.profit_center_id, segment_id = EXCLUDED.segment_id,
			trading_partner_id = EXCLUDED.trading_partner_id, is_removed = false`
	writeParkedLines = `INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, ` + lineValues +
		` ON CONFLICT (tenant_id, parked_entry_id, line_no)` + lineUpdate
	removeParkedLines  = `UPDATE ledger.parked_entry_line SET is_removed = true WHERE parked_entry_id = $1 AND line_no > $2 AND NOT is_removed`
	writeTemplateLines = `INSERT INTO ledger.entry_template_line (tenant_id, template_id, ` + lineValues +
		` ON CONFLICT (tenant_id, template_id, line_no)` + lineUpdate
	removeTemplateLines = `UPDATE ledger.entry_template_line SET is_removed = true WHERE template_id = $1 AND line_no > $2 AND NOT is_removed`
)

// putParkedLines makes the parked entry's live lines the given ones.
func putParkedLines(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, lines []Line) error {
	return putLines(ctx, tx, [2]string{writeParkedLines, removeParkedLines}, parkedLines.what, tenant, id, lines)
}

// putTemplateLines makes the template's live lines the given ones.
func putTemplateLines(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, lines []Line) error {
	return putLines(ctx, tx, [2]string{writeTemplateLines, removeTemplateLines}, templateLines.what, tenant, id, lines)
}

// putLines makes the live lines of the entry or template id the given
// ones, numbered 1..n, with the statements sql (write, remove): existing
// numbers are updated, new ones inserted and the rest marked removed, as
// the application deletes nothing (000015).
func putLines(ctx context.Context, tx pgx.Tx, sql [2]string, what string, tenant, id uuid.UUID, lines []Line) error {
	n := len(lines)
	nos, accounts, partners, amounts := make([]int32, n), make([]uuid.UUID, n), make([]*uuid.UUID, n), make([]money.Minor, n)
	taxes, quantities, units, texts := make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
	bases, jurisdictions := make([]*money.Minor, n), make([]*string, n)
	ccs, pcs, segs, tps := make([]*uuid.UUID, n), make([]*uuid.UUID, n), make([]*uuid.UUID, n), make([]*uuid.UUID, n)
	for i, l := range lines {
		nos[i], accounts[i], partners[i], amounts[i] = int32(i+1), l.AccountID, l.PartnerID, l.TC
		taxes[i], quantities[i], units[i], texts[i], bases[i], jurisdictions[i] = l.TaxCode, l.Quantity, l.Unit, l.Text, l.TaxBase, l.TaxJurisdiction
		ccs[i], pcs[i], segs[i], tps[i] = l.CostCenterID, l.ProfitCenterID, l.SegmentID, l.TradingPartnerID
	}
	if _, err := tx.Exec(ctx, sql[0], tenant, id, nos, accounts, amounts, partners, taxes, quantities, units, texts, bases, jurisdictions,
		ccs, pcs, segs, tps); err != nil {
		return fmt.Errorf("ledger: write lines of %s %s: %w", what, id, err)
	}
	if _, err := tx.Exec(ctx, sql[1], id, n); err != nil {
		return fmt.Errorf("ledger: remove lines of %s %s: %w", what, id, err)
	}
	return nil
}

// debitTotal is what a manual entry is measured by: the sum of its debits
// in transaction currency.
func debitTotal(lines []Line) (money.Minor, error) {
	var debits []money.Minor
	for _, l := range lines {
		if l.TC > 0 {
			debits = append(debits, l.TC)
		}
	}
	total, err := money.Sum(debits...)
	if err != nil {
		return 0, errTotalTooLarge("lines")
	}
	return total, nil
}

// parkedChanged records a change of status of a parked entry: the audit
// record of the action, with extra detail, and ParkedJournalEntryChanged
// with its status before (previous; nil when it was just parked).
func parkedChanged(ctx context.Context, pe ParkedEntry, previous *string, action string, extra map[string]any) error {
	detail := map[string]any{"status": pe.Status, "previous_status": previous, "lines": len(pe.Lines)}
	maps.Copy(detail, extra)
	return audit.Record(ctx, "ledger.parked_entry."+action, "ledger.parked_entry", pe.ID, detail,
		outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed", AggregateType: "ledger.parked_entry", AggregateID: pe.ID,
			Payload: map[string]any{"parked_entry_id": pe.ID, "legal_entity_id": pe.LegalEntityID, "posting_date": pe.PostingDate.Format(time.DateOnly),
				"amount": money.EventAmount(pe.Debits, pe.Currency, pe.Exp), "status": pe.Status, "previous_status": previous,
				"created_by_kind": pe.CreatedByKind, "owner_id": pe.OwnerID, "approval_request_id": pe.ApprovalRequestID,
				"journal_entry_id": pe.JournalEntryID, "template_id": pe.TemplateID, "template_name": pe.TemplateName, "occurrence_date": outbox.Date(pe.OccurrenceDate)}})
}

// EntryChange is a merge patch of a parked entry: nil fields are
// unchanged, a nil *string in Reference or HeaderText clears it, and Lines
// replaces the lines as a whole.
type EntryChange struct {
	DocumentType              *string
	DocumentDate, PostingDate *time.Time
	TaxDate                   **time.Time // a nil *time.Time clears it: the posting date
	FiscalPeriod              **int16     // a nil *int16 removes the special period: the posting date's
	Currency                  *string
	Reference, HeaderText     **string
	Lines                     []NewLine
}

// editable locks a parked entry and returns it with the owner it has after
// the caller changes or cancels it. Read FOR UPDATE (ADR-0008): the
// database refuses line changes of an entry no longer parked, but reads its
// status unlocked. Only the owner acts on an entry (ownerAfter).
func editable(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error) (ParkedEntry, *uuid.UUID, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return ParkedEntry{}, nil, err
	}
	// Only an entry of a company the caller may see: one it may not is
	// refused below as not found and must not stay locked until rollback.
	if _, err := tx.Exec(ctx, `SELECT FROM ledger.parked_entry WHERE id = $1 AND ($2 OR legal_entity_id = ANY($3)) FOR UPDATE`,
		id, access.All, access.IDs); err != nil {
		return ParkedEntry{}, nil, fmt.Errorf("ledger: lock parked entry %s: %w", id, err)
	}
	pe, err := getParked(ctx, tx, id)
	if err != nil {
		return ParkedEntry{}, nil, err
	}
	if err := ifMatch(pe.Version); err != nil {
		return ParkedEntry{}, nil, err
	}
	if pe.Status != EntryParked {
		return ParkedEntry{}, nil, problem.Conflict("ledger.invalid_parked_status", "The parked entry is "+pe.Status+"; this requires status "+EntryParked+".")
	}
	owner, err := ownerAfter(pe, p)
	return pe, owner, err
}

// ownerAfter is the owner of a parked entry once the caller acts on it:
// its owner, alone; a person taking on an entry no person owns (an
// agent's proposal), who becomes its owner; or the agent or service that
// parked an entry no person owns, which stays unowned. So the requester an
// approval excludes is the one person who wrote the entry (T35).
func ownerAfter(pe ParkedEntry, p reqctx.Principal) (*uuid.UUID, error) {
	if owner, ok := takenOn(pe.OwnerID, pe.CreatedBy, p); ok {
		return owner, nil
	}
	return nil, problem.Forbidden("ledger.not_entry_owner", "Only the owner of a parked entry changes, posts or cancels it: the person who parked it or took it on.")
}

// takenOn is the owner of what owner owns and createdBy made once p acts
// on it, as ownerAfter tells it; ok is false when p may not act on it.
func takenOn(owner *uuid.UUID, createdBy uuid.UUID, p reqctx.Principal) (after *uuid.UUID, ok bool) {
	switch {
	case owner != nil && *owner == p.ID:
		return owner, true
	case owner == nil && p.Kind == reqctx.KindHuman:
		return &p.ID, true
	case owner == nil && createdBy == p.ID:
		return nil, true
	}
	return nil, false
}

// changed is the parked entry as a manual entry with the change applied.
func changed(pe ParkedEntry, ch EntryChange) NewEntry {
	in := NewEntry{LegalEntityID: pe.LegalEntityID, LedgerGroup: pe.LedgerGroup, DocumentType: pe.DocumentType, DocumentDate: pe.DocumentDate,
		PostingDate: pe.PostingDate, TaxDate: pe.TaxDate, Currency: pe.Currency, Reference: pe.Reference, HeaderText: pe.HeaderText, Lines: ch.Lines,
		special: deref(ch.FiscalPeriod, pe.FiscalPeriod)}
	if pe.LedgerSpecific {
		in.LedgerID = &pe.LedgerID
	}
	if in.Lines == nil {
		in.Lines = asNewLines(pe.Lines, pe.Currency, pe.Exp)
	}
	in.DocumentType = deref(ch.DocumentType, in.DocumentType)
	in.DocumentDate, in.PostingDate = deref(ch.DocumentDate, in.DocumentDate), deref(ch.PostingDate, in.PostingDate)
	in.TaxDate = deref(ch.TaxDate, in.TaxDate)
	in.Currency = deref(ch.Currency, in.Currency)
	in.Reference, in.HeaderText = deref(ch.Reference, in.Reference), deref(ch.HeaderText, in.HeaderText)
	return in
}

// asNewLines are kept lines, in minor units of currency with exp digits,
// as the lines of a manual entry to check again.
func asNewLines(lines []Line, currency string, exp int) []NewLine {
	out := make([]NewLine, len(lines))
	for i, l := range lines {
		out[i] = NewLine{AccountID: l.AccountID, Amount: l.TC.Plain(exp), Currency: currency, PartnerID: l.PartnerID, TaxCode: l.TaxCode,
			Quantity: l.Quantity, Unit: l.Unit, Text: l.Text, TaxBaseCurrency: currency, TaxJurisdiction: l.TaxJurisdiction,
			CostCenterID: l.CostCenterID, ProfitCenterID: l.ProfitCenterID, SegmentID: l.SegmentID, TradingPartnerID: l.TradingPartnerID}
		if l.TaxBase != nil {
			b := l.TaxBase.Plain(exp)
			out[i].TaxBase = &b
		}
	}
	return out
}

// deref is *v, or otherwise when v is nil.
func deref[T any](v *T, otherwise T) T {
	if v == nil {
		return otherwise
	}
	return *v
}

// changeEntry changes a parked entry and checks it again as parking does.
// A change is no change of status, so it is audited but publishes nothing.
func (e *Endpoints) changeEntry(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch EntryChange) (ParkedEntry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	pe, owner, err := editable(ctx, tx, p, id, ifMatch)
	if err != nil {
		return ParkedEntry{}, err
	}
	in := changed(pe, ch)
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return ParkedEntry{}, err
	}
	if err := parkable(ctx, tx, v, in.PostingDate); err != nil {
		return ParkedEntry{}, err
	}
	if err := checkSpecial(ctx, tx, v, in); err != nil {
		return ParkedEntry{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.parked_entry SET ledger_id = $2, document_type = $3, document_date = $4, posting_date = $5, currency = $6,
			reference = $7, header_text = $8, owner_id = $9, version = version + 1, updated_by = $10, tax_date = $11, fiscal_period = $12 WHERE id = $1`,
		id, v.choice.ledger, in.DocumentType, in.DocumentDate, in.PostingDate, in.Currency, in.Reference, in.HeaderText, owner, p.ID, in.TaxDate, in.special); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: change parked entry %s: %w", id, err)
	}
	if err := putParkedLines(ctx, tx, p.TenantID, id, v.lines); err != nil {
		return ParkedEntry{}, err
	}
	updated, err := getParked(ctx, tx, id)
	if err != nil {
		return ParkedEntry{}, err
	}
	return updated, audit.Add(ctx, audit.Entry{Action: "ledger.parked_entry.update", ObjectType: "ledger.parked_entry", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"fields": ch.fields(), "lines": len(v.lines), "owner_id": owner, "taken_on": pe.OwnerID == nil && owner != nil}})
}

// fields names what a change sets, for the audit trail: once a person
// takes on an agent's proposal, it shows what the person changed.
func (ch EntryChange) fields() []string {
	out := []string{}
	for _, f := range []struct {
		name string
		set  bool
	}{{"document_type", ch.DocumentType != nil}, {"document_date", ch.DocumentDate != nil}, {"posting_date", ch.PostingDate != nil},
		{"tax_date", ch.TaxDate != nil}, {"fiscal_period", ch.FiscalPeriod != nil},
		{"currency", ch.Currency != nil}, {"reference", ch.Reference != nil}, {"header_text", ch.HeaderText != nil}, {"lines", ch.Lines != nil}} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// cancelEntry cancels a parked entry for good; nothing was posted.
func cancelEntry(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (ParkedEntry, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ParkedEntry{}, err
	}
	pe, owner, err := editable(ctx, tx, p, id, ifMatch)
	if err != nil {
		return ParkedEntry{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.parked_entry SET status = 'cancelled', owner_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, owner, p.ID); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: cancel parked entry %s: %w", id, err)
	}
	cancelled, err := getParked(ctx, tx, id)
	if err != nil {
		return ParkedEntry{}, err
	}
	return cancelled, parkedChanged(ctx, cancelled, &pe.Status, "cancel", map[string]any{"reason": reason})
}
