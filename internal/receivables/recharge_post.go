package receivables

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// charge is a charge again to post (recharge.go): the invoice, its
// reliefs, what is recovered of what they wrote off, the tax charged
// again (negative: charged back less) in the invoice's currency and the
// company's, the change it follows, and once posted the company, its id
// and entry.
type charge struct {
	invoice   Invoice
	state     reliefState
	recovered money.Minor
	tax, lc   money.Minor
	recharging
	le    tenancy.Entity
	id    uuid.UUID
	entry ledger.Entry
}

// reliefGroup is the reliefs' tax recovered under one adjustment code, on
// one account and in one emirate (nil: no subdivision on the line), as
// their entries' tax items say.
type reliefGroup struct {
	code         string
	account      uuid.UUID
	jurisdiction *string
	tax, lc      money.Minor
}

// reliefGroups are the reliefs' entries' tax items by code, account and
// emirate, in the order they first come: what a charge again reverses,
// in proportion to them.
func (e *Endpoints) reliefGroups(ctx context.Context, c *charge) ([]reliefGroup, error) {
	type key struct {
		code         string
		account      uuid.UUID
		jurisdiction string
	}
	var groups []reliefGroup
	at := map[key]int{}
	for _, entry := range c.state.entries {
		items, err := e.TaxItems.Of(ctx, entry)
		if err != nil {
			return nil, fmt.Errorf("receivables: tax items of bad debt relief entry %s: %w", entry, err)
		}
		for _, it := range items {
			g := reliefGroup{code: it.Code, account: *it.AccountID} // a relief's tax lines name their account (reliefLines)
			if it.Jurisdiction != nil && strings.Contains(*it.Jurisdiction, "-") {
				g.jurisdiction = it.Jurisdiction
			}
			k := key{code: g.code, account: g.account}
			if g.jurisdiction != nil {
				k.jurisdiction = *g.jurisdiction
			}
			i, ok := at[k]
			if !ok {
				i, at[k], groups = len(groups), len(groups), append(groups, g)
			}
			// Tax a relief posted: in range.
			groups[i].tax += it.Tax
			groups[i].lc += it.TaxLC
		}
	}
	return groups, nil
}

// chargeLines are the charge's lines: the tax shared out over the
// reliefs' groups by their tax (money.ShareOut), the local tax over those
// with a share by their local tax, each credited (a charge back less
// debited) under its code and emirate to its account, a share that rounds
// to nothing left out; then the total debited to the payment differences
// account.
func chargeLines(c charge, groups []reliefGroup, expensed uuid.UUID, text *string) []ledger.PostingLine {
	taxes, locals := make([]money.Minor, len(groups)), make([]money.Minor, len(groups))
	for i, g := range groups {
		taxes[i], locals[i] = g.tax, g.lc
	}
	tc := money.ShareOut(c.tax, taxes)
	for i := range groups {
		if tc[i] == 0 {
			locals[i] = 0 // no line takes it
		}
	}
	lc := money.ShareOut(c.lc, locals)
	var lines []ledger.PostingLine
	var posted money.Minor
	for i, g := range groups {
		if tc[i] == 0 {
			continue
		}
		var local money.Minor
		if lc != nil { // nil when no relief carried local tax
			local = -lc[i]
		}
		posted -= local
		code := g.code
		lines = append(lines, ledger.PostingLine{AccountID: g.account, Amount: -tc[i], LC: &local, TaxCode: &code, TaxJurisdiction: g.jurisdiction, Text: text})
	}
	return append(lines, ledger.PostingLine{AccountID: expensed, Amount: c.tax, LC: &posted, Text: text})
}

// postCharge posts the charge's entry (chargeLines): document type SA, the
// invoice's number its reference, dated and posted on the change's date,
// which is its tax date, the charge its source document. The company
// without its payment differences account is refused, 422
// receivables.no_write_off_account; a refusal by the ledger (a period not
// open, a code not in force) is the ledger's.
func (e *Endpoints) postCharge(ctx context.Context, c *charge) error {
	var err error
	if c.le, _, err = e.LegalEntities.Entity(ctx, c.invoice.LegalEntityID); err != nil { // a stored invoice's legal entity exists
		return fmt.Errorf("receivables: company of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	groups, err := e.reliefGroups(ctx, c)
	if err != nil {
		return err
	}
	expensed, ok, err := e.Accounts.Determination(ctx, c.le.ID, RechargeKey)
	if err != nil {
		return fmt.Errorf("receivables: the payment differences account of company %s: %w", c.le.Code, err)
	}
	if !ok {
		return problem.Unprocessable("receivables.no_write_off_account", "The bad debt relief of customer invoice "+c.invoice.DocumentNumber+
			" is charged again against the company's "+RechargeKey+" account, which the relief credited, and company "+c.le.Code+" has none.")
	}
	text := "Bad debt relief of customer invoice " + c.invoice.DocumentNumber + " charged again"
	lines := chargeLines(*c, groups, expensed, &text)
	c.lc = *lines[len(lines)-1].LC // what the tax lines posted, the payment differences line's local amount
	c.id = uuid.Must(uuid.NewV7())
	ref := c.invoice.DocumentNumber
	c.entry, err = e.Postings.Post(ctx, ledger.Posting{LegalEntityID: c.le.ID, DocumentType: ReliefDocumentType, DocumentDate: c.date, PostingDate: c.date,
		Currency: c.invoice.Currency, Reference: &ref, HeaderText: &text, SourceContext: "receivables", SourceDocumentID: c.id, Lines: lines})
	if err != nil {
		return fmt.Errorf("receivables: post the charge again of the bad debt relief of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	return nil
}

// record writes the charge posted and its audit record (ADR-0011): the
// invoice, the payment's application, what is recovered, the tax and the
// entry. The ledger records and publishes its entry; the payment's own
// event (IncomingPaymentApplied or IncomingPaymentReset) says what changed.
func (c charge) record(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.bad_debt_recharge (tenant_id, id, legal_entity_id, customer_invoice_id, incoming_payment_id,
			application_no, reset, currency, local_currency, recovered_amount, tax_amount, tax_amount_lc, posting_date, journal_entry_year, journal_entry_id,
			journal_entry_number, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		p.TenantID, c.id, c.le.ID, c.invoice.ID, c.payment, c.application, c.reset, c.invoice.Currency, c.le.LocalCurrency, c.recovered, c.tax, c.lc,
		c.date, c.entry.FiscalYear, c.entry.ID, c.entry.DocumentNumber, p.ID); err != nil {
		return fmt.Errorf("receivables: record the charge again of the bad debt relief of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	const action = "receivables.bad_debt_recharge.post"
	detail := map[string]any{"customer_invoice_id": c.invoice.ID, "customer_invoice_number": c.invoice.DocumentNumber, "incoming_payment_id": c.payment,
		"application_no": c.application, "reset": c.reset, "recovered_amount": money.EventAmount(c.recovered, c.invoice.Currency, c.invoice.Exponent),
		"tax_amount": money.EventAmount(c.tax, c.invoice.Currency, c.invoice.Exponent), "posting_date": c.date.Format(time.DateOnly),
		"journal_entry_id": c.entry.ID, "journal_entry_number": c.entry.DocumentNumber}
	return errs.Wrapf(audit.Record(ctx, action, "receivables.bad_debt_recharge", c.id, detail), "receivables: record %s of bad debt recharge %s", action, c.id)
}
