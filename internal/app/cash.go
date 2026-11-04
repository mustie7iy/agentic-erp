package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Cash is the cash context as the API wires it (ADR-0051 §2): a leaf that
// reads the tenancy, payments, ledger, payables and agents contexts through the
// adapters below, and procurement's orders through payables, so none of
// them imports it and it imports none of them.
func Cash(cfg Config) *cash.Endpoints {
	// Posting's own dependencies: the forecast reads an invoice, and an
	// order's open value, as posting would (T53e2, T53g).
	invoices := payablesEndpoints(cfg, nil)
	return &cash.Endpoints{LegalEntities: cashCompanies{}, Settings: configuration.Settings{},
		Payments: cashPayments{position: payments.CashPosition{Accounts: ledger.Accounts{}, Owed: ledger.OpenItems{}, Payees: partner.Payees{},
			Unposted: invoices, Requests: downPayments{}, Orders: invoices}},
		Ledger: cashLedger{}, Sessions: agents.Sessions{}, Partners: cashPartners{},
		Treasury: cashTreasury{integration.TreasuryChannels{Secrets: resolver(cfg.Secrets)}},
		Pages:    cfg.Pages, Now: cfg.Now, Consistent: consistent(cfg)}
}

// cashTreasury gives the cash context the integration context's treasury
// channels, the treasury_export connectors its snapshots are exported to
// (ADR-0051 §14, T53h).
type cashTreasury struct{ channels integration.TreasuryChannels }

func (t cashTreasury) ForCompany(ctx context.Context, code string) ([]cash.TreasuryChannel, error) {
	chans, err := t.channels.ForCompany(ctx, code)
	out := make([]cash.TreasuryChannel, len(chans))
	for i, ch := range chans {
		out[i] = treasuryChannel(ch)
	}
	return out, err
}

func (t cashTreasury) Channel(ctx context.Context, id uuid.UUID) (cash.TreasuryChannel, bool, error) {
	ch, found, err := t.channels.Channel(ctx, id)
	return treasuryChannel(ch), found, err
}

func (t cashTreasury) Endpoint(ctx context.Context, id uuid.UUID) (sftpclient.Endpoint, error) {
	return t.channels.Endpoint(ctx, id)
}

func treasuryChannel(ch integration.TreasuryChannel) cash.TreasuryChannel {
	return cash.TreasuryChannel{ID: ch.ID, Name: ch.Name, Active: ch.Active, Format: ch.Format, Companies: ch.Companies}
}

// consistent is the snapshots' consistent read on its own pool, none
// without one.
func consistent(cfg Config) cash.Consistency {
	if cfg.Consistent == nil {
		return nil
	}
	return uow.Consistent{Begin: cfg.Consistent.Begin, Role: cfg.AppRole}
}

// cashPartners names the suppliers of the forecast's drill-down, as the
// partner context publishes them (T53e4).
type cashPartners struct{}

func (cashPartners) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]cash.PartnerName, error) {
	named, err := partner.Payees{}.Names(ctx, ids)
	out := make(map[uuid.UUID]cash.PartnerName, len(named))
	for id, n := range named {
		out[id] = cash.PartnerName{ID: id, Number: n.Number, Name: n.Name}
	}
	return out, err
}

// cashCompanies gives the cash context a legal entity's code, name and
// currency, the legal entity of a company code, every legal entity and the
// tenant's group currency.
type cashCompanies struct{}

func (cashCompanies) Company(ctx context.Context, id uuid.UUID) (cash.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return cashCompany(le), ok, err
}

func (cashCompanies) Entities(ctx context.Context) ([]cash.Entity, error) {
	les, err := tenancy.Directory{}.Entities(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cash.Entity, len(les))
	for i, le := range les {
		out[i] = cash.Entity{ID: le.ID, Company: cashCompany(le)}
	}
	return out, nil
}

func (cashCompanies) GroupCurrency(ctx context.Context) (string, error) {
	return tenancy.Directory{}.GroupCurrency(ctx)
}

func cashCompany(le tenancy.Entity) cash.Company {
	return cash.Company{Code: le.Code, Name: le.Name, Currency: le.LocalCurrency}
}

func (cashCompanies) ByCode(ctx context.Context, code string) (uuid.UUID, bool, error) {
	le, ok, err := tenancy.Directory{}.EntityByCode(ctx, code)
	return le.ID, ok, err
}

// cashPayments gives the cash context the payments context's house banks,
// statements and payments.
type cashPayments struct{ position payments.CashPosition }

func (c cashPayments) Banks(ctx context.Context, legalEntity uuid.UUID, before time.Time) ([]cash.Bank, error) {
	banks, err := c.position.Banks(ctx, legalEntity, before)
	out := make([]cash.Bank, len(banks))
	for i, b := range banks {
		out[i] = cash.Bank{ID: b.ID, Code: b.Code, Last4: b.Last4, Currency: b.Currency, Exponent: b.Exponent, ClearingAccountID: b.ClearingAccountID}
		if s := b.Statement; s != nil {
			f := s.Figures
			out[i].Statement = &cash.Statement{Date: f.AsOf, Closing: s.Closing, Gap: s.Gap, Book: cash.Book{Ledger: f.Ledger, Unexplained: f.Unexplained(),
				NotPosted: f.InTransit, Excluded: f.Excluded, PostedLater: f.PostedLater}}
		}
	}
	return out, err
}

func (c cashPayments) Released(ctx context.Context, ids []uuid.UUID) ([]cash.Payment, error) {
	ps, err := c.position.Released(ctx, ids)
	return cashPaymentsOf(ps), err
}

func (c cashPayments) Pending(ctx context.Context, legalEntity uuid.UUID) ([]cash.Payment, error) {
	ps, err := c.position.Pending(ctx, legalEntity)
	return cashPaymentsOf(ps), err
}

func (c cashPayments) HouseBanks(ctx context.Context, legalEntity uuid.UUID) ([]cash.HouseBank, error) {
	banks, err := c.position.HouseBanks(ctx, legalEntity)
	out := make([]cash.HouseBank, len(banks))
	for i, b := range banks {
		out[i] = cash.HouseBank{ID: b.ID, Code: b.Code, Currency: b.Currency}
	}
	return out, err
}

func (c cashPayments) StatementLine(ctx context.Context, statement uuid.UUID, lineNo int32) (cash.StatementLine, bool, error) {
	l, found, err := c.position.StatementLine(ctx, statement, lineNo)
	return cash.StatementLine{LegalEntityID: l.LegalEntityID, HouseBankID: l.HouseBankID, Currency: l.Currency, Amount: l.Amount, ValueDate: l.ValueDate},
		found, err
}

func (c cashPayments) HouseBankCodes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return c.position.HouseBankCodes(ctx, ids)
}

func (c cashPayments) HouseBankAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]cash.BankAccount, error) {
	accounts, err := c.position.HouseBankAccounts(ctx, ids)
	out := make(map[uuid.UUID]cash.BankAccount, len(accounts))
	for id, a := range accounts {
		out[id] = cash.BankAccount{Code: a.Code, Last4: a.Last4}
	}
	return out, err
}

// Payables gives the forecast the supplier open items, the unposted
// invoices and the down payment requests with the payments context's
// routing and the ledger's due-date and discount rules.
func (c cashPayments) Payables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]cash.Payable, error) {
	ps, err := c.position.Payables(ctx, legalEntity, on)
	out := make([]cash.Payable, len(ps))
	for i, p := range ps {
		out[i] = cash.Payable{Source: cashSources[p.Kind], Supplier: p.PartnerID, Currency: p.Currency, Due: p.Due, Blocked: p.Blocked != "",
			BankCode: p.HouseBankCode, Method: p.Method, Dates: p.Dates(), Pays: p.Pays, Lines: cashLines(p.Lines), PlanningGroup: p.PlanningGroup,
			Document: cash.Document{Kind: cashDocuments[p.Kind], ID: p.Document, Number: p.Number}}
	}
	return out, err
}

// Paying gives the forecast what the payments of runs not released pay,
// to derive their liquidity items from (T53e3), and the variance the
// documents a payment paid (T53f3).
func (c cashPayments) Paying(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, on time.Time) (map[uuid.UUID]cash.Paying, error) {
	ps, err := c.position.Paying(ctx, legalEntity, ids, on)
	out := make(map[uuid.UUID]cash.Paying, len(ps))
	for id, p := range ps {
		items := make([]cash.Paid, len(p.Items))
		for i, it := range p.Items {
			items[i] = cash.Paid{Amount: it.Amount, Lines: cashLines(it.Lines), Entry: it.Entry, Invoice: it.Invoice, Order: it.Order}
		}
		out[id] = cash.Paying{PlanningGroup: p.PlanningGroup, Items: items, Request: p.Request}
	}
	return out, err
}

// Actuals gives the cash actuals the statement lines with what their
// decided matches clear (T53f2).
func (c cashPayments) Actuals(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) ([]cash.ActualLine, error) {
	ls, err := c.position.Actuals(ctx, legalEntity, from, to)
	out := make([]cash.ActualLine, len(ls))
	for i, l := range ls {
		out[i] = cash.ActualLine{StatementID: l.StatementID, LineNo: l.LineNo, StatementRef: l.StatementRef,
			BankReference: l.BankReference, HouseBankID: l.HouseBankID, Currency: l.Currency, Amount: l.Amount,
			ValueDate: l.ValueDate, Status: l.Status, Match: cashMatches[l.Match], PostingRule: l.PostingRule, Payments: cashPaymentsOf(l.Payments)}
	}
	return out, err
}

// cashMatches are the cash actuals' kinds of match of the payments
// context's (ADR-0051 §4, T53f2).
var cashMatches = map[string]string{payments.CashMatchPayment: cash.MatchPayment, payments.CashMatchPostingRule: cash.MatchPostingRule,
	payments.CashMatchIncoming: cash.MatchIncoming, payments.CashMatchExcluded: cash.MatchExcluded, payments.CashMatchNone: cash.MatchNone}

// cashLines are a document's offsetting lines as the cash context reads
// them.
func cashLines(ls []ledger.OffsettingLine) []cash.DocumentLine {
	out := make([]cash.DocumentLine, len(ls))
	for i, l := range ls {
		out[i] = cash.DocumentLine{Account: l.Account, Amount: l.Amount, TaxCode: l.TaxCode, TaxKey: l.TaxKey}
	}
	return out
}

// cashSources are the forecast's sources of the payments context's kinds
// of payable (ADR-0051 §4).
var cashSources = map[string]string{payments.CashItem: cash.SourcePayable, payments.CashUnposted: cash.SourceInvoiceUnposted,
	payments.CashDownPayment: cash.SourceDownPayment, payments.CashOrder: cash.SourcePurchaseOrder}

// cashDocuments are the kinds of document the forecast's drill-down names
// for the payments context's kinds of payable (T53e4): a posted item's
// journal entry, an unposted invoice, a down payment request, a purchase
// order (T53g).
var cashDocuments = map[string]string{payments.CashItem: cash.DocJournalEntry, payments.CashUnposted: cash.DocSupplierInvoice,
	payments.CashDownPayment: cash.DocDownPaymentRequest, payments.CashOrder: cash.DocPurchaseOrder}

func cashPaymentsOf(ps []payments.CashPayment) []cash.Payment {
	out := make([]cash.Payment, len(ps))
	for i, p := range ps {
		out[i] = cash.Payment{ID: p.ID, HouseBankID: p.HouseBankID, RunID: p.RunID, SupplierID: p.SupplierID, RunNumber: p.RunNumber, Method: p.Method,
			RunDate: p.RunDate, Amount: p.Amount, Currency: p.Currency, MatchedOn: p.MatchedOn}
	}
	return out
}

// cashLedger gives the cash context the ledger's open lines and rates.
type cashLedger struct{}

func (cashLedger) OpenLines(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) ([]cash.OpenLine, error) {
	lines, err := ledger.Accounts{}.OpenLines(ctx, legalEntity, accounts)
	out := make([]cash.OpenLine, len(lines))
	for i, l := range lines {
		out[i] = cash.OpenLine{AccountID: l.AccountID, Currency: l.Currency, Amount: l.Amount, SourceContext: l.SourceContext, SourceID: l.SourceID}
	}
	return out, err
}

func (cashLedger) Rate(ctx context.Context, rateType, from, to string, date time.Time) (cash.Rate, bool, error) {
	c, found, err := ledger.Rates{}.OfType(ctx, rateType, from, to, date)
	return cash.Rate{Rate: c.Rate, ValidFrom: c.ValidFrom, Convert: c.Convert}, found, err
}

// cashSimulation is the cash context's derivation simulation (ADR-0051
// §4, T53e3) as a change request's simulation runs it: it classifies the
// open flows of the forecast, not the window's transactions it is given.
type cashSimulation struct{ sim cash.Simulation }

func (c cashSimulation) Simulate(ctx context.Context, base, proposed *config.Config, _ time.Duration) (configuration.Simulation, error) {
	r, err := c.sim.Simulate(ctx, base, proposed)
	return configuration.Simulation{Passed: r.Passed, Summary: r.Summary, Detail: r.Detail}, err
}

// cashDisclosure shows a reader of a change request the derivation
// simulation's companies they may access alone (cash.Disclose).
type cashDisclosure struct{}

func (cashDisclosure) Disclose(_ context.Context, e configuration.Evidence, access authz.Access) (configuration.Evidence, error) {
	if e.Kind != "simulation" {
		return e, nil
	}
	detail, summary, err := cash.Disclose(e.Detail, e.Summary, access)
	if err != nil {
		return configuration.Evidence{}, err
	}
	e.Detail, e.Summary = detail, summary
	return e, nil
}
