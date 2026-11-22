package receivables

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Relief is a row of receivables.bad_debt_relief with its invoice's
// number and customer: the write-off it relieves by entry and dispute
// case, what that wrote off and the tax recovered, in the invoice's
// currency and in the company's, the adjustment code, the customer's
// notice where given and the entry that posted it.
type Relief struct {
	ID                 uuid.UUID
	LegalEntityID      uuid.UUID
	InvoiceID          uuid.UUID
	InvoiceNumber      string
	CustomerID         uuid.UUID
	WriteOffYear       int16
	WriteOffID         uuid.UUID
	WriteOffNumber     string
	DisputeCaseID      uuid.UUID
	Currency           string
	Exponent           int
	LocalCurrency      string
	LocalExponent      int
	WrittenOff         money.Minor
	WrittenOffLC       money.Minor
	Tax, TaxLC         money.Minor
	TaxCode            string
	CustomerNotifiedOn *time.Time
	PostingDate        time.Time
	EntryYear          int16
	EntryID            uuid.UUID
	EntryNumber        string
	CreatedAt          time.Time
	CreatedBy          uuid.UUID
}

// reliefSelect reads reliefs with their invoices' numbers and customers
// and their currencies' minor-unit digits. The invoice's columns come
// from a lateral subquery that carries no id, so id names the relief's
// alone, as the keyset's page condition takes it (pagination.Keyset.SQL).
const reliefSelect = `SELECT r.id, r.legal_entity_id, r.customer_invoice_id, i.document_number, i.customer_id, r.write_off_entry_year,
		r.write_off_entry_id, r.write_off_entry_number, r.dispute_case_id, r.currency, c.exponent, r.local_currency, l.exponent,
		r.written_off_amount, r.written_off_amount_lc, r.tax_amount, r.tax_amount_lc, r.tax_code, r.customer_notified_on, r.posting_date,
		r.journal_entry_year, r.journal_entry_id, r.journal_entry_number, r.created_at, r.created_by
	FROM receivables.bad_debt_relief r
	CROSS JOIN LATERAL (SELECT v.document_number, v.customer_id FROM receivables.customer_invoice v
		WHERE v.tenant_id = r.tenant_id AND v.id = r.customer_invoice_id) i
	JOIN ref.currency c ON c.code = r.currency JOIN ref.currency l ON l.code = r.local_currency`

func scanRelief(row pgx.Row) (Relief, error) {
	var r Relief
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.InvoiceID, &r.InvoiceNumber, &r.CustomerID, &r.WriteOffYear, &r.WriteOffID, &r.WriteOffNumber,
		&r.DisputeCaseID, &r.Currency, &r.Exponent, &r.LocalCurrency, &r.LocalExponent, &r.WrittenOff, &r.WrittenOffLC, &r.Tax, &r.TaxLC, &r.TaxCode,
		&r.CustomerNotifiedOn, &r.PostingDate, &r.EntryYear, &r.EntryID, &r.EntryNumber, &r.CreatedAt, &r.CreatedBy)
	return r, errs.Wrapf(err, "scan bad debt relief")
}

// reliefLines are the relief's lines: per adjustment component account
// and emirate the shares on it, a debit that takes the output tax back,
// with the adjustment code and the emirate the invoice's item named
// (reliefComponent), each with its local amount, the shares' at the
// invoice's booked rate; then their total credited to the payment
// differences account. A share that rounds to nothing in the invoice's
// currency is left out, its local part too. It returns the lines and the
// local tax they post.
func reliefLines(c claim, text *string) ([]ledger.PostingLine, money.Minor) {
	type key struct {
		account      uuid.UUID
		jurisdiction string
	}
	var lines []ledger.PostingLine
	at := map[key]int{}
	var lc money.Minor
	for _, sh := range c.shares {
		if sh.tc == 0 {
			continue
		}
		k := key{account: sh.account}
		if sh.item.Jurisdiction != nil && strings.Contains(*sh.item.Jurisdiction, "-") { // levied on its line (reliefComponent)
			k.jurisdiction = *sh.item.Jurisdiction
		}
		i, ok := at[k]
		if !ok {
			l := ledger.PostingLine{AccountID: sh.account, TaxCode: &c.in.TaxCode, Text: text, LC: new(money.Minor)}
			if k.jurisdiction != "" {
				l.TaxJurisdiction = &k.jurisdiction
			}
			i, at[k], lines = len(lines), len(lines), append(lines, l)
		}
		// Shares of the invoice's tax, which was posted: in range.
		lines[i].Amount += sh.tc
		*lines[i].LC += sh.lc
		lc += sh.lc
	}
	back := -lc
	return append(lines, ledger.PostingLine{AccountID: c.expensed, Amount: -c.tax, LC: &back, Text: text}), lc
}

// postRelief posts the relief's entry (reliefLines) through the ledger:
// document type SA, dated and posted on the relief's date, its tax date,
// the invoice's number its reference and the relief its source document.
// A refusal by the ledger (a period not open, a code without a rate on
// the date) is the ledger's problem.
func (e *Endpoints) postRelief(ctx context.Context, c *claim, id uuid.UUID) (ledger.Entry, error) {
	text := "Bad debt relief of customer invoice " + c.invoice.DocumentNumber
	header := fmt.Sprintf("Bad debt relief of customer invoice %s, write-off %s (%s)", c.invoice.DocumentNumber, c.written.EntryNumber, c.rule.law)
	lines, lc := reliefLines(*c, &text)
	c.lc = lc
	ref := c.invoice.DocumentNumber
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: c.le.ID, DocumentType: ReliefDocumentType, DocumentDate: c.date, PostingDate: c.date,
		Currency: c.invoice.Currency, Reference: &ref, HeaderText: &header, SourceContext: "receivables", SourceDocumentID: id, Lines: lines})
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("receivables: post the bad debt relief of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	return entry, nil
}

// insertRelief records the relief posted.
func insertRelief(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, c claim, entry ledger.Entry) error {
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.bad_debt_relief (tenant_id, id, legal_entity_id, customer_invoice_id, write_off_entry_year,
			write_off_entry_id, write_off_entry_number, dispute_case_id, currency, local_currency, written_off_amount, written_off_amount_lc,
			tax_amount, tax_amount_lc, tax_code, customer_notified_on, posting_date, journal_entry_year, journal_entry_id, journal_entry_number,
			created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
		p.TenantID, id, c.le.ID, c.invoice.ID, c.written.EntryYear, c.in.WriteOffEntryID, c.written.EntryNumber, c.written.DisputeCaseID,
		c.invoice.Currency, c.le.LocalCurrency, c.written.Amount, c.written.AmountLC, c.tax, c.lc, c.in.TaxCode, c.in.CustomerNotifiedOn, c.date,
		entry.FiscalYear, entry.ID, entry.DocumentNumber, p.ID); err != nil {
		return fmt.Errorf("receivables: record the bad debt relief of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	return nil
}

// loadRelief reads a relief whatever its company.
func loadRelief(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Relief, error) {
	r, err := scanRelief(tx.QueryRow(ctx, reliefSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Relief{}, problem.NotFoundOf("bad debt relief")
	}
	if err != nil {
		return Relief{}, fmt.Errorf("receivables: read bad debt relief %s: %w", id, err)
	}
	return r, nil
}

// GetRelief returns a relief of a legal entity the caller may act in;
// another's is not found, so ids cannot be probed (04 §4.9).
func GetRelief(ctx context.Context, id uuid.UUID) (Relief, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Relief{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Relief{}, fmt.Errorf("receivables: company access for bad debt relief %s: %w", id, err)
	}
	r, err := loadRelief(ctx, tx, id)
	if err == nil && !access.Allows(r.LegalEntityID) {
		return Relief{}, problem.NotFoundOf("bad debt relief")
	}
	return r, err
}

// recordRelief adds the relief's audit record (ADR-0011): the invoice, the
// write-off, the tax recovered under its code and the entry. The ledger
// records and publishes its entry as it does every entry; the relief
// changes no document another context reads, so it publishes no event of
// its own.
func recordRelief(ctx context.Context, r Relief) error {
	const action = "receivables.bad_debt_relief.claim"
	var notified any
	if r.CustomerNotifiedOn != nil {
		notified = r.CustomerNotifiedOn.Format(time.DateOnly)
	}
	detail := map[string]any{"customer_invoice_id": r.InvoiceID, "customer_invoice_number": r.InvoiceNumber, "customer_id": r.CustomerID,
		"write_off_journal_entry_id": r.WriteOffID, "write_off_journal_entry_number": r.WriteOffNumber, "dispute_case_id": r.DisputeCaseID,
		"written_off_amount": money.EventAmount(r.WrittenOff, r.Currency, r.Exponent), "tax_amount": money.EventAmount(r.Tax, r.Currency, r.Exponent),
		"tax_code": r.TaxCode, "customer_notified_on": notified, "posting_date": r.PostingDate.Format(time.DateOnly),
		"journal_entry_id": r.EntryID, "journal_entry_number": r.EntryNumber}
	return errs.Wrapf(audit.Record(ctx, action, "receivables.bad_debt_relief", r.ID, detail), "receivables: record %s of bad debt relief %s", action, r.ID)
}
