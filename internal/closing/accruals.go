package closing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of an accrual proposal as stored (ADR-0058 §5, §8).
const AccrualProposed = "proposed"

// Kinds of an accrual proposal's evidence (ADR-0058 §8).
const (
	EvidenceInvoice = "supplier_invoice"
	EvidenceOrder   = "purchase_order"
	EvidenceEntry   = "journal_entry"
)

// evidence is a document a proposal rests on, as it was cited.
type evidence struct {
	Kind       string     `json:"kind"`
	DocumentID uuid.UUID  `json:"document_id"`
	Number     string     `json:"document_number"`
	FiscalYear *int16     `json:"fiscal_year"`
	SupplierID *uuid.UUID `json:"supplier_id"`
}

// accrual is a stored accrual proposal with its close's legal entity and
// period, and its evidence in the order given.
type accrual struct {
	ID, CloseID, LegalEntityID                                     uuid.UUID
	FiscalYear, FiscalPeriod                                       int16
	SessionID, AgentID                                             uuid.UUID
	AccountID                                                      uuid.UUID
	AccountNumber, AccountName                                     string
	AccrualAccountID                                               uuid.UUID
	AccrualAccountNumber, AccrualAccountName                       string
	CostCenterID, ProfitCenterID                                   *uuid.UUID
	CostCenterCode, CostCenterName, ProfitCenterCode, ProfitCenter *string
	SupplierID                                                     *uuid.UUID
	Currency                                                       string
	Amount                                                         money.Minor
	Description, Reason                                            string
	ParkedEntryID                                                  uuid.UUID
	PostingDate                                                    time.Time
	Status                                                         string
	Version                                                        int32
	CreatedAt, UpdatedAt                                           time.Time
	Evidence                                                       []evidence
}

const accrualSelect = `SELECT a.id, a.close_id, c.legal_entity_id, c.fiscal_year, c.fiscal_period, a.session_id, a.agent_id, a.account_id, a.account_number,
		a.account_name, a.accrual_account_id, a.accrual_account_number, a.accrual_account_name, a.cost_center_id, a.profit_center_id, a.cost_center_code,
		a.cost_center_name, a.profit_center_code, a.profit_center_name, a.supplier_id, a.currency, a.amount, a.description, a.reason, a.parked_entry_id,
		a.posting_date, a.status, a.version, a.created_at, a.updated_at,
		coalesce((SELECT jsonb_agg(jsonb_build_object('kind', e.kind, 'document_id', e.document_id, 'document_number', e.document_number,
			'fiscal_year', e.fiscal_year, 'supplier_id', e.supplier_id) ORDER BY e.seq)
			FROM closing.accrual_evidence e WHERE e.tenant_id = a.tenant_id AND e.proposal_id = a.id), '[]')
	FROM closing.accrual_proposal a JOIN closing.period_close c ON c.tenant_id = a.tenant_id AND c.id = a.close_id`

func scanAccrual(r pgx.Row) (accrual, error) {
	var a accrual
	err := r.Scan(&a.ID, &a.CloseID, &a.LegalEntityID, &a.FiscalYear, &a.FiscalPeriod, &a.SessionID, &a.AgentID, &a.AccountID, &a.AccountNumber,
		&a.AccountName, &a.AccrualAccountID, &a.AccrualAccountNumber, &a.AccrualAccountName, &a.CostCenterID, &a.ProfitCenterID, &a.CostCenterCode,
		&a.CostCenterName, &a.ProfitCenterCode, &a.ProfitCenter, &a.SupplierID, &a.Currency, &a.Amount, &a.Description, &a.Reason, &a.ParkedEntryID,
		&a.PostingDate, &a.Status, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.Evidence)
	return a, errs.Wrapf(err, "scan accrual proposal")
}

// readAccrual reads a proposal of a close of a legal entity the caller
// may act in; any other is not found.
func readAccrual(ctx context.Context, tx pgx.Tx, id uuid.UUID) (accrual, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return accrual{}, err
	}
	a, err := scanAccrual(tx.QueryRow(ctx, accrualSelect+` WHERE a.id = $1`, id))
	if err = authz.Visible(err, access, a.LegalEntityID, problem.NotFoundOf("accrual proposal"), "closing: read accrual proposal %s", id); err != nil {
		return accrual{}, err
	}
	return a, nil
}

func accrualPath(id uuid.UUID) string { return "/v1/accrual-proposals/" + id.String() }

// accrualInput is an agent's proposal, its amount parsed.
type accrualInput struct {
	*gen.AccrualProposalInput
	amount money.Minor
}

// costCenter and profitCenter are the proposal's centers, nil for none.
func (in accrualInput) costCenter() *uuid.UUID   { return apitypes.Ptr[uuid.UUID](in.CostCenterID) }
func (in accrualInput) profitCenter() *uuid.UUID { return apitypes.Ptr[uuid.UUID](in.ProfitCenterID) }
func (in accrualInput) supplier() *uuid.UUID     { return apitypes.Ptr[uuid.UUID](in.SupplierID) }

// ProposeAccrual implements POST /v1/accrual-proposals (ADR-0058 §8): the
// calling agent's accrual of one expense of the close's period, in its
// running session, parked as a journal entry for a person to post; the
// same proposal again answers it as it is.
func (e *Endpoints) ProposeAccrual(ctx context.Context, req *gen.AccrualProposalInput, _ gen.ProposeAccrualParams) (gen.ProposeAccrualRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if p.Kind != reqctx.KindAgent {
		return nil, problem.Forbidden("agents.not_an_agent", "Only agents propose accruals; a person parks the entry (POST /v1/parked-journal-entries).")
	}
	if err := e.Sessions.Running(ctx, req.SessionID); err != nil {
		return nil, err
	}
	for _, f := range [][2]string{{"description", req.Description}, {"reason", req.Reason}} {
		if err := problem.RequireText(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	a, created, err := e.writeAccrual(ctx, tx, p, req)
	if err != nil {
		return nil, err
	}
	out, err := e.accrualOut(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	if !created {
		return &gen.AccrualProposalHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Location: gen.NewOptString(accrualPath(a.ID)), Response: out}, nil
	}
	return &gen.ProposeAccrualCreatedHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Location: gen.NewOptString(accrualPath(a.ID)), Response: out}, nil
}

// accrualTarget is what a proposal is written against: its close and
// company, the accounts it debits and credits, and the currency's digits.
type accrualTarget struct {
	close            periodClose
	company          Company
	exp              int
	expense, accrual Account
}

// writeAccrual writes the agent's proposal under the close's lock
// (ADR-0058 §5, §8): a duplicate is refused, or answered as it is when it
// is the session's retry of the same content (created false); else a
// company without an accrual account it can post to is refused, the
// expense account, the supplier and the evidence are checked, the entry
// parked and the proposal written.
func (e *Endpoints) writeAccrual(ctx context.Context, tx pgx.Tx, p reqctx.Principal, req *gen.AccrualProposalInput) (accrual, bool, error) {
	if _, err := lockClose(ctx, tx, req.CloseID); err != nil {
		return accrual{}, false, err
	}
	unlocked, err := e.writable(ctx, tx, req.CloseID)
	if err != nil {
		return accrual{}, false, err
	}
	target, in, err := e.accrualTarget(ctx, tx, req)
	if err != nil {
		return accrual{}, false, err
	}
	// A retried proposal answers as it is stored, whatever happened to the
	// accounts since.
	if a, found, err := proposedAlready(ctx, tx, in); found || err != nil {
		return a, false, err
	}
	if err := e.accounts(ctx, &target, req.AccountID); err != nil {
		return accrual{}, false, err
	}
	if err := e.checkAccrual(ctx, target, in); err != nil {
		return accrual{}, false, err
	}
	cited, err := e.citeEvidence(ctx, target, in)
	if err != nil {
		return accrual{}, false, err
	}
	if err := reopen(ctx, tx, p, req.CloseID, unlocked); err != nil {
		return accrual{}, false, err
	}
	a, err := e.parkAccrual(ctx, tx, p, target, in, cited)
	return a, err == nil, err
}

// accrualTarget reads the close, its company and currency, and parses
// the amount.
func (e *Endpoints) accrualTarget(ctx context.Context, tx pgx.Tx, req *gen.AccrualProposalInput) (accrualTarget, accrualInput, error) {
	c, err := readClose(ctx, tx, req.CloseID)
	if err != nil {
		return accrualTarget{}, accrualInput{}, err
	}
	co, ok, err := e.Companies.Company(ctx, c.LegalEntityID)
	if err == nil && !ok {
		err = fmt.Errorf("legal entity %s is not held by the tenancy context", c.LegalEntityID)
	}
	if err != nil {
		return accrualTarget{}, accrualInput{}, fmt.Errorf("closing: company of close %s: %w", c.ID, err)
	}
	t := accrualTarget{close: c, company: co}
	if t.exp, err = currency.Exponent(ctx, tx, co.LocalCurrency, nil); err != nil {
		return accrualTarget{}, accrualInput{}, fmt.Errorf("closing: currency of company %s: %w", co.Code, err)
	}
	in := accrualInput{AccrualProposalInput: req}
	if in.amount, err = accrualAmount(req.Amount, co.LocalCurrency, t.exp); err != nil {
		return accrualTarget{}, accrualInput{}, err
	}
	return t, in, nil
}

// accrualAmount parses the amount: in the company's local currency, with
// its digits, more than zero.
func accrualAmount(m gen.Money, local string, exp int) (money.Minor, error) {
	invalid := func(why string) error {
		return problem.Unprocessable("request.invalid_amount", "An accrual is an expense in the company's local currency "+local+", more than zero: "+why+".",
			problem.FieldError{Field: "amount", Code: "invalid", Message: "more than zero, in " + local})
	}
	if m.Currency != local {
		return 0, invalid("the amount is in " + m.Currency)
	}
	amount, err := money.ParseAmount(m.Amount, exp)
	switch {
	case err != nil:
		return 0, invalid(err.Error())
	case amount <= 0:
		return 0, invalid("the amount is " + m.Amount)
	}
	return amount, nil
}

// accounts reads the expense account and the company's accrual account
// (companies[].close.accrual_account): one that is not configured, or not
// set up and postable in the company, refuses every proposal of it.
func (e *Endpoints) accounts(ctx context.Context, t *accrualTarget, expense uuid.UUID) error {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return fmt.Errorf("closing: configuration: %w", err)
	}
	var number string
	if co := doc.Company(t.company.Code); co != nil {
		number = co.Close.AccrualAccount
	}
	missing := func(why string) error {
		return problem.Conflict("closing.accrual_account_missing", "Company "+t.company.Code+" "+why+"; accruals are credited to it "+
			"(companies[].close.accrual_account).")
	}
	if number == "" {
		return missing("configures no accrual account")
	}
	as, err := e.Ledger.Accounts(ctx, t.close.LegalEntityID, []uuid.UUID{expense}, []string{number})
	if err != nil {
		return fmt.Errorf("closing: accounts of an accrual of close %s: %w", t.close.ID, err)
	}
	var accrualFound bool
	for _, a := range as {
		if a.ID == expense {
			t.expense = a
		}
		if a.Number == number && a.Postable {
			t.accrual, accrualFound = a, true
		}
	}
	if !accrualFound {
		return missing("cannot post to its accrual account " + number + ": it is not set up, active and postable by hand in the company")
	}
	return nil
}

// proposedAlready finds a proposal of the close for the same expense
// account, cost center and supplier that is not rejected: the session's
// with the same content answers again (a retry); any other is a
// duplicate, refused naming it (ADR-0058 §8).
func proposedAlready(ctx context.Context, tx pgx.Tx, in accrualInput) (accrual, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM closing.accrual_proposal WHERE close_id = $1 AND account_id = $2 AND cost_center_id IS NOT DISTINCT FROM $3
		AND supplier_id IS NOT DISTINCT FROM $4 AND status <> 'rejected' ORDER BY created_at, id LIMIT 1`, in.CloseID, in.AccountID, in.costCenter(),
		in.supplier()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return accrual{}, false, nil
	}
	if err != nil {
		return accrual{}, false, fmt.Errorf("closing: accrual proposals of close %s: %w", in.CloseID, err)
	}
	a, err := readAccrual(ctx, tx, id)
	if err != nil {
		return accrual{}, false, err
	}
	if a.SessionID != in.SessionID || !sameAccrual(a, in) {
		p := problem.Conflict("closing.accrual_duplicate", "Accrual proposal "+a.ID.String()+" of this close is for the same expense account, cost center "+
			"and supplier and is not rejected; one proposal per expense.")
		p.Header = http.Header{"Location": {accrualPath(a.ID)}}
		return accrual{}, false, p
	}
	return a, true, nil
}

// sameAccrual reports whether a stored proposal has the content of the
// input: its profit center, amount, description, reason and evidence (its
// account, cost center and supplier found it). A retry that gives no
// profit center matches the one the proposal took from its expense line.
func sameAccrual(a accrual, in accrualInput) bool {
	cited := make([]gen.AccrualEvidenceInput, len(a.Evidence))
	for i, ev := range a.Evidence {
		cited[i] = gen.AccrualEvidenceInput{Kind: ev.Kind, DocumentID: ev.DocumentID}
	}
	pc := in.profitCenter()
	return (pc == nil || a.ProfitCenterID != nil && *a.ProfitCenterID == *pc) && a.Amount == in.amount && a.Description == in.Description && a.Reason == in.Reason &&
		slices.Equal(cited, in.Evidence)
}

// checkAccrual refuses an expense account that is no profit and loss
// account postable by hand in the company, and a supplier that is no
// business partner.
func (e *Endpoints) checkAccrual(ctx context.Context, t accrualTarget, in accrualInput) error {
	if x := t.expense; x.ID == uuid.Nil || x.BalanceSheet || !x.Postable {
		return problem.Unprocessable("closing.accrual_account_invalid", "An accrual debits an expense: a profit and loss account set up and postable by "+
			"hand in company "+t.company.Code+", no reconciliation account.",
			problem.FieldError{Field: "account_id", Code: "invalid", Message: "a profit and loss account postable in the company"})
	}
	s := in.supplier()
	if s == nil {
		return nil
	}
	found, err := e.Evidence.Suppliers(ctx, []uuid.UUID{*s})
	if err != nil {
		return fmt.Errorf("closing: supplier of an accrual of close %s: %w", t.close.ID, err)
	}
	if _, ok := found[*s]; !ok {
		return problem.ReferenceNotFound("supplier_id", "business partner", "a business partner (GET /v1/business-partners)")
	}
	return nil
}

// citeEvidence checks each document the proposal rests on: a supplier
// invoice of the close's company that is posted, a purchase order of it
// that is released, or a journal entry of it posted in the leading ledger
// (422 closing.evidence_unknown on evidence[i] otherwise); each is kept
// with its number, an invoice or order with its supplier, an entry with
// its fiscal year.
func (e *Endpoints) citeEvidence(ctx context.Context, t accrualTarget, in accrualInput) ([]evidence, error) {
	ids := map[string][]uuid.UUID{}
	for _, ev := range in.Evidence {
		ids[ev.Kind] = append(ids[ev.Kind], ev.DocumentID)
	}
	invoices, err := e.Evidence.Invoices(ctx, ids[EvidenceInvoice])
	if err == nil {
		var orders map[uuid.UUID]Document
		if orders, err = e.Evidence.Orders(ctx, ids[EvidenceOrder]); err == nil {
			return e.evidenceOf(ctx, t, in, map[string]map[uuid.UUID]Document{EvidenceInvoice: invoices, EvidenceOrder: orders}, ids[EvidenceEntry])
		}
	}
	return nil, fmt.Errorf("closing: evidence of an accrual of close %s: %w", t.close.ID, err)
}

// evidenceOf matches each document cited with what was found of it.
func (e *Endpoints) evidenceOf(ctx context.Context, t accrualTarget, in accrualInput, docs map[string]map[uuid.UUID]Document, entryIDs []uuid.UUID) ([]evidence, error) {
	entries, err := e.Ledger.Cited(ctx, t.close.LegalEntityID, in.AccountID, entryIDs)
	if err != nil {
		return nil, fmt.Errorf("closing: entries cited by an accrual of close %s: %w", t.close.ID, err)
	}
	out := make([]evidence, len(in.Evidence))
	for i, ev := range in.Evidence {
		out[i] = evidence{Kind: ev.Kind, DocumentID: ev.DocumentID}
		ok := false
		if ev.Kind == EvidenceEntry {
			var c CitedEntry
			if c, ok = entries[ev.DocumentID]; ok {
				out[i].Number, out[i].FiscalYear = c.DocumentNumber, &c.FiscalYear
			}
		} else if d, found := docs[ev.Kind][ev.DocumentID]; found && d.Counts && d.LegalEntityID == t.close.LegalEntityID {
			ok, out[i].Number, out[i].SupplierID = true, d.Number, &d.SupplierID
		}
		if !ok {
			return nil, problem.Unprocessable("closing.evidence_unknown", fmt.Sprintf("Evidence %d is no %s of company %s that counts as evidence: a posted "+
				"supplier invoice, a released purchase order or a journal entry posted in the leading ledger.", i+1, ev.Kind, t.company.Code),
				problem.FieldError{Field: fmt.Sprintf("evidence[%d]", i), Code: "unknown", Message: "a posted invoice, released order or posted entry of the company"})
		}
	}
	return out, nil
}

// parkAccrual parks the proposal's entry through the ledger as the agent,
// dated the period's last day in local currency (ADR-0058 §8): Dr the
// expense account with the centers, Cr the accrual account with the
// expense line's profit center (the one given, or the one a posting of
// the expense line derives), the description each line's text; then
// writes the proposal, with that profit center, and its evidence,
// audited and published.
func (e *Endpoints) parkAccrual(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t accrualTarget, in accrualInput, cited []evidence) (accrual, error) {
	id := uuid.Must(uuid.NewV7())
	amount := in.amount.Plain(t.exp)
	profit, err := e.Ledger.ProfitCenter(ctx, t.close.LegalEntityID, t.close.FiscalYear, t.close.PeriodEnd, t.expense.ID, in.costCenter(), in.profitCenter())
	if err != nil {
		return accrual{}, asProposal(err)
	}
	entry, err := e.Ledger.Park(ctx, ParkEntry{LegalEntityID: t.close.LegalEntityID, Date: t.close.PeriodEnd, Currency: t.company.LocalCurrency,
		HeaderText: "Accrual proposal " + id.String(), Lines: []ParkLine{
			{AccountID: t.expense.ID, Amount: amount, CostCenterID: in.costCenter(), ProfitCenterID: profit, Text: in.Description},
			{AccountID: t.accrual.ID, Amount: "-" + amount, ProfitCenterID: profit, Text: in.Description}}})
	if err != nil {
		return accrual{}, asProposal(err)
	}
	centers, err := e.centers(ctx, in.costCenter(), profit)
	if err != nil {
		return accrual{}, err
	}
	cc, pc := centers.of(in.costCenter()), centers.of(profit)
	if _, err := tx.Exec(ctx, `INSERT INTO closing.accrual_proposal (tenant_id, id, close_id, session_id, agent_id, account_id, account_number, account_name,
			accrual_account_id, accrual_account_number, accrual_account_name, cost_center_id, cost_center_code, cost_center_name, profit_center_id,
			profit_center_code, profit_center_name, supplier_id, currency, amount, description, reason, parked_entry_id, posting_date, status,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, 'proposed', $5, $5)`,
		p.TenantID, id, in.CloseID, in.SessionID, p.ID, t.expense.ID, t.expense.Number, t.expense.Name, t.accrual.ID, t.accrual.Number, t.accrual.Name,
		in.costCenter(), cc.code, cc.name, profit, pc.code, pc.name, in.supplier(), t.company.LocalCurrency, in.amount, in.Description,
		in.Reason, entry, t.close.PeriodEnd); err != nil {
		return accrual{}, fmt.Errorf("closing: insert accrual proposal of close %s: %w", in.CloseID, err)
	}
	for i, ev := range cited {
		if _, err := tx.Exec(ctx, `INSERT INTO closing.accrual_evidence (tenant_id, proposal_id, seq, kind, document_id, document_number, fiscal_year,
			supplier_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.TenantID, id, i+1, ev.Kind, ev.DocumentID, ev.Number, ev.FiscalYear, ev.SupplierID); err != nil {
			return accrual{}, fmt.Errorf("closing: cite %s %s in accrual proposal %s: %w", ev.Kind, ev.DocumentID, id, err)
		}
	}
	a, err := readAccrual(ctx, tx, id)
	if err != nil {
		return accrual{}, err
	}
	return a, recordAccrual(ctx, a, t.exp)
}

// lineNames name the parked entry's lines in a refusal of the proposal.
var lineNames = strings.NewReplacer("Line 1: ", "The expense line: ", "Line 2: ", "The accrual account's line: ")

// asProposal states the ledger's refusal of the parked entry in the
// proposal's terms: a field of the expense line (lines[0].cost_center_id)
// is the proposal's field (cost_center_id); one of the accrual account's
// line, which the configuration names, keeps its line's field
// (lines[1].account_id); and each line is named in the detail by what it
// is.
func asProposal(err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for k, f := range p.Errors {
		if field, ok := strings.CutPrefix(f.Field, "lines[0]."); ok {
			p.Errors[k].Field = field
		}
	}
	p.Detail = lineNames.Replace(p.Detail)
	return p
}

// centerNames are the proposal's centers' codes and names, by id.
type centerNames map[uuid.UUID]Center

// centerText is a center's code and name, nil for no center.
type centerText struct{ code, name *string }

func (c centerNames) of(id *uuid.UUID) centerText {
	if id == nil {
		return centerText{}
	}
	n := c[*id]
	return centerText{&n.Code, &n.Name}
}

// centers names the proposal's centers, which the parking checked exist
// (a center the ledger did not name would leave its code empty, which
// accrual_proposal_cost_center_code and accrual_proposal_profit_center_code refuse).
func (e *Endpoints) centers(ctx context.Context, cost, profit *uuid.UUID) (centerNames, error) {
	var ids []uuid.UUID
	for _, id := range []*uuid.UUID{cost, profit} {
		if id != nil {
			ids = append(ids, *id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := e.Ledger.Centers(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("closing: centers of an accrual: %w", err)
	}
	return found, nil
}

// recordAccrual audits a proposal and publishes it as AccrualProposed.
func recordAccrual(ctx context.Context, a accrual, exp int) error {
	cited := make([]map[string]any, len(a.Evidence))
	for i, ev := range a.Evidence {
		cited[i] = map[string]any{"kind": ev.Kind, "document_id": ev.DocumentID}
	}
	amount := money.EventAmount(a.Amount, a.Currency, exp)
	detail := map[string]any{"period_close_id": a.CloseID, "legal_entity_id": a.LegalEntityID, "session_id": a.SessionID, "account_id": a.AccountID,
		"accrual_account_id": a.AccrualAccountID, "cost_center_id": a.CostCenterID, "profit_center_id": a.ProfitCenterID, "supplier_id": a.SupplierID,
		"amount": amount, "description": a.Description, "reason": a.Reason, "evidence": a.Evidence, "parked_entry_id": a.ParkedEntryID,
		"posting_date": a.PostingDate.Format(time.DateOnly), "status": a.Status, "version": a.Version}
	return audit.Record(ctx, "closing.accrual_proposal.propose", "closing.accrual_proposal", a.ID, detail,
		outbox.Event{Context: "closing", Name: "accrual_proposed", AggregateType: "closing.accrual_proposal", AggregateID: a.ID,
			Payload: map[string]any{"accrual_proposal_id": a.ID, "period_close_id": a.CloseID, "legal_entity_id": a.LegalEntityID, "fiscal_year": a.FiscalYear,
				"fiscal_period": a.FiscalPeriod, "session_id": a.SessionID, "account_id": a.AccountID, "accrual_account_id": a.AccrualAccountID,
				"cost_center_id": a.CostCenterID, "profit_center_id": a.ProfitCenterID, "supplier_id": a.SupplierID, "amount": amount, "evidence": cited,
				"parked_entry_id": a.ParkedEntryID, "posting_date": a.PostingDate.Format(time.DateOnly), "status": a.Status, "version": a.Version}})
}
