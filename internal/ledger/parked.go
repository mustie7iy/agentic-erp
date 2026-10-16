package ledger

import (
	"context"
	"errors"
	"fmt"
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
	ID                uuid.UUID
	LegalEntityID     uuid.UUID
	LedgerID          uuid.UUID
	DocumentType      string
	DocumentDate      time.Time
	PostingDate       time.Time
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
	Lines             []Line
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const parkedSelect = `SELECT p.id, p.legal_entity_id, p.ledger_id, p.document_type, p.document_date, p.posting_date, p.currency, c.exponent,
		p.reference, p.header_text, p.status, p.created_by_kind, p.owner_id, p.approval_request_id, p.journal_fiscal_year, p.journal_entry_id,
		p.version, p.created_at, p.created_by, p.updated_at, p.updated_by,
		coalesce((SELECT json_agg(json_build_object('line_no', l.line_no, 'account_id', l.account_id, 'tc', l.amount, 'lc', 0, 'gc', 0,
			'partner_id', l.partner_id, 'tax_code', l.tax_code, 'quantity', trim_scale(l.quantity)::text, 'unit', l.uom_code, 'text', l.line_text)
			ORDER BY l.line_no)
		 FROM ledger.parked_entry_line l WHERE l.tenant_id = p.tenant_id AND l.parked_entry_id = p.id AND NOT l.is_removed), '[]')
	FROM ledger.parked_entry p JOIN ref.currency c ON c.code = p.currency`

func scanParked(row pgx.Row) (ParkedEntry, error) {
	var pe ParkedEntry
	err := row.Scan(&pe.ID, &pe.LegalEntityID, &pe.LedgerID, &pe.DocumentType, &pe.DocumentDate, &pe.PostingDate, &pe.Currency, &pe.Exp,
		&pe.Reference, &pe.HeaderText, &pe.Status, &pe.CreatedByKind, &pe.OwnerID, &pe.ApprovalRequestID, &pe.JournalFiscalYear, &pe.JournalEntryID,
		&pe.Version, &pe.CreatedAt, &pe.CreatedBy, &pe.UpdatedAt, &pe.UpdatedBy, &pe.Lines)
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
	if err := refuseAutomatic(ctx, tx, v.le.ID, v.lines); err != nil {
		return ParkedEntry{}, err
	}
	total, err := debitTotal(v.lines)
	if err != nil {
		return ParkedEntry{}, err
	}
	id := uuid.Must(uuid.NewV7())
	var owner *uuid.UUID
	if p.Kind == reqctx.KindHuman {
		owner = &p.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
			currency, reference, header_text, created_by_kind, owner_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)`,
		p.TenantID, id, v.le.ID, v.ledger, in.DocumentType, in.DocumentDate, in.PostingDate, in.Currency, in.Reference, in.HeaderText,
		p.Kind, owner, p.ID); err != nil {
		return ParkedEntry{}, fmt.Errorf("ledger: park entry %s: %w", id, err)
	}
	if err := insertParkedLines(ctx, tx, p.TenantID, id, v.lines); err != nil {
		return ParkedEntry{}, err
	}
	pe, err := getParked(ctx, tx, id)
	if err != nil {
		return ParkedEntry{}, err
	}
	return pe, parkedChanged(ctx, pe, total, nil, "park")
}

// refuseAutomatic refuses a line on the legal entity's GR/IR account, which
// only goods receipts and invoices post (T33): parked, it could never be
// posted. Posting is refused by the database (journal_line_gr_ir_order_item)
// with the same problem.
func refuseAutomatic(ctx context.Context, tx pgx.Tx, le uuid.UUID, lines []Line) error {
	var grir uuid.UUID
	err := tx.QueryRow(ctx, `SELECT account_id FROM ledger.account_determination WHERE legal_entity_id = $1 AND key = 'gr_ir'`, le).Scan(&grir)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ledger: GR/IR account of %s: %w", le, err)
	}
	for i, l := range lines {
		if l.AccountID == grir {
			return problem.Unprocessable("ledger.automatic_account", "The GR/IR account is posted only by goods receipts and invoices, which name the order item of each line.",
				problem.FieldError{Field: fmt.Sprintf("lines[%d].account_id", i), Code: "automatic", Message: "an account other than the legal entity's GR/IR account"})
		}
	}
	return nil
}

// insertParkedLines writes the lines of a parked entry, numbered 1..n.
func insertParkedLines(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, lines []Line) error {
	n := len(lines)
	nos, accounts, partners, amounts := make([]int32, n), make([]uuid.UUID, n), make([]*uuid.UUID, n), make([]money.Minor, n)
	taxes, quantities, units, texts := make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
	for i, l := range lines {
		nos[i], accounts[i], partners[i], amounts[i] = int32(i+1), l.AccountID, l.PartnerID, l.TC
		taxes[i], quantities[i], units[i], texts[i] = l.TaxCode, l.Quantity, l.Unit, l.Text
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount, partner_id,
			tax_code, quantity, uom_code, line_text)
		SELECT $1, $2, n, a, m, pa, tx, q::numeric, u, t
		FROM unnest($3::int[], $4::uuid[], $5::bigint[], $6::uuid[], $7::text[], $8::text[], $9::text[], $10::text[]) AS x(n, a, m, pa, tx, q, u, t)`,
		tenant, id, nos, accounts, amounts, partners, taxes, quantities, units, texts); err != nil {
		return fmt.Errorf("ledger: park entry %s: lines: %w", id, err)
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

// parkedChanged records a change of a parked entry: the audit record of
// the action and ParkedJournalEntryChanged with its debit total and its
// status before (previous; nil when it was just parked).
func parkedChanged(ctx context.Context, pe ParkedEntry, total money.Minor, previous *string, action string) error {
	return audit.Record(ctx, "ledger.parked_entry."+action, "ledger.parked_entry", pe.ID,
		map[string]any{"status": pe.Status, "previous_status": previous, "lines": len(pe.Lines)},
		outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed", AggregateType: "ledger.parked_entry", AggregateID: pe.ID,
			Payload: map[string]any{"parked_entry_id": pe.ID, "legal_entity_id": pe.LegalEntityID, "posting_date": pe.PostingDate.Format(time.DateOnly),
				"amount": money.EventAmount(total, pe.Currency, pe.Exp), "status": pe.Status, "previous_status": previous,
				"created_by_kind": pe.CreatedByKind, "owner_id": pe.OwnerID, "approval_request_id": pe.ApprovalRequestID,
				"journal_entry_id": pe.JournalEntryID}})
}
