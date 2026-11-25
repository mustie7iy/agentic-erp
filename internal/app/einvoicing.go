package app

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// EInvoicing is the einvoicing context as the API wires it (ADR-0061 §2):
// a leaf that reads the integration, tenancy and operations contexts
// through the adapters below, so none of them imports it and it imports
// none of them. Its providers are called through cfg.Providers, Evatra at
// cfg.EvatraHosts (nil: Evatra's own hosts).
func EInvoicing(cfg Config) *einvoicing.Endpoints {
	return &einvoicing.Endpoints{
		Connectors: einvoiceConnectors{integration.EInvoiceProviders{Secrets: resolver(cfg.Secrets), RefuseStandIn: cfg.RefuseEBICS}},
		Companies:  einvoiceCompanies{}, Runs: runOperations{kind: einvoicing.KindFetch}, Objects: cfg.Objects,
		Adapter: einvoicing.Adapters(cfg.Providers, cfg.EvatraHosts, cfg.Now), Pages: cfg.Pages, Now: cfg.Now,
		Sales: einvoiceSales{keys: cfg.FieldKeys}, Sends: runOperations{kind: einvoicing.KindSend}, TaxData: runOperations{kind: einvoicing.KindTaxData},
		Settings: configuration.Settings{}, Payables: einvoicePayables{e: Payables(cfg)}}
}

// einvoicePayables gives the einvoicing context payables' entering of
// e-invoices, the entered invoices' numbers and suppliers (ADR-0061 §2,
// §6), and a held e-invoice's link to an invoice entered by hand: the
// invoice read under its lock, the supplier's tax identifiers by
// fingerprint (payables' partner port) and the document attached (§7, T65f).
type einvoicePayables struct{ e *payables.Endpoints }

func (a einvoicePayables) Enter(ctx context.Context, in einvoicing.Entry) (einvoicing.Entered, error) {
	d := in.Document
	lines := make([]payables.EInvoiceLine, len(d.Lines))
	for i, l := range d.Lines {
		lines[i] = payables.EInvoiceLine{OrderLine: l.OrderLine, Description: l.Description, Quantity: l.Quantity, Net: l.Net, Category: l.Category,
			Percent: l.Percent}
	}
	// Only a credit note names the invoices it credits and its reason: an
	// invoice may state a preceding invoice (IBG-03), which capture would
	// refuse it for.
	e := payables.EInvoice{LegalEntityID: in.LegalEntityID, Kind: payables.KindInvoice, Number: d.Number, IssueDate: d.IssueDate, Currency: d.Currency,
		Exponent: d.Exponent, OrderReference: d.OrderReference, SellerTRN: d.Seller.TRN, Lines: lines, Tax: d.Tax, Gross: d.TaxInclusive,
		SHA256: in.SHA256, ObjectRef: in.ContentRef}
	// A document not in AED is handed over stating each line's VAT in AED
	// (heldBefore holds one that does not), kept as its tax's local amount.
	if lc, stated, err := einvoicing.LineTaxInAED(d); d.Currency != einvoicing.AccountingCurrency && stated && err == nil {
		e.LocalTax, e.LocalCurrency = &lc, einvoicing.AccountingCurrency
	}
	if d.Kind == einvoicing.KindTaxCreditNote {
		e.Kind = payables.KindCreditMemo
		for _, c := range d.Cited {
			e.Cited = append(e.Cited, payables.Citation{Number: c.Number, Date: c.Date})
		}
		if d.Reason != nil {
			e.Reason = &payables.CreditReason{Code: d.Reason.Code, Text: d.Reason.Text}
		}
	}
	v, err := a.e.EnterEInvoice(ctx, e)
	return einvoicing.Entered{InvoiceID: v.ID, SupplierID: v.SupplierID}, err
}

func (a einvoicePayables) Invoices(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]einvoicing.EnteredInvoice, error) {
	invs, err := payables.Invoices{}.Sources(ctx, ids)
	if err != nil {
		return nil, err
	}
	suppliers := make([]uuid.UUID, 0, len(invs))
	for _, v := range invs {
		suppliers = append(suppliers, v.SupplierID)
	}
	names, err := partner.Payees{}.Names(ctx, suppliers)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]einvoicing.EnteredInvoice, len(invs))
	for id, v := range invs {
		out[id] = einvoicing.EnteredInvoice{DocumentNumber: v.DocumentNumber, SupplierReference: v.SupplierReference, Status: v.Status,
			SupplierID: v.SupplierID, SupplierName: names[v.SupplierID].Name}
	}
	return out, nil
}

func (einvoicePayables) Linkable(ctx context.Context, id uuid.UUID) (einvoicing.LinkTarget, bool, error) {
	a, ok, err := payables.Invoices{}.Attachable(ctx, id)
	return einvoicing.LinkTarget{LegalEntityID: a.LegalEntityID, SupplierID: a.SupplierID, SupplierReference: a.SupplierReference, Source: a.Source,
		Status: a.Status, Kind: a.Kind, InvoiceDate: a.InvoiceDate, Currency: a.Currency, Gross: a.Gross, Tax: a.Tax, HasDocument: a.HasDocument}, ok, err
}

func (a einvoicePayables) HoldsTaxNumber(ctx context.Context, supplier uuid.UUID, trn string) (bool, error) {
	return a.e.TaxNumbers.HasTaxNumber(ctx, supplier, trn)
}

func (einvoicePayables) Attach(ctx context.Context, id uuid.UUID, sha []byte, ref string) error {
	return payables.Invoices{}.Attach(ctx, id, sha, ref)
}

// einvoiceConnectors gives the einvoicing context the einvoice_provider
// connectors, over integration's published EInvoiceProviders.
type einvoiceConnectors struct{ p integration.EInvoiceProviders }

func (c einvoiceConnectors) Provider(ctx context.Context, id uuid.UUID) (einvoicing.Connector, error) {
	p, err := c.p.Provider(ctx, id)
	return einvoicing.Connector{ID: p.ID, Tenant: p.Tenant, Name: p.Name, Adapter: p.Adapter, BaseURL: p.BaseURL, Environment: p.Environment,
		CompanyID: p.CompanyID, Participants: p.Participants, Format: p.Format, SecretRef: p.SecretRef}, err
}

func (c einvoiceConnectors) Credentials(ctx context.Context, conn einvoicing.Connector) (string, error) {
	return c.p.Credentials(ctx, integration.EInvoiceConnector{ID: conn.ID, Tenant: conn.Tenant, Name: conn.Name, SecretRef: conn.SecretRef})
}

func (c einvoiceConnectors) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return c.p.Names(ctx, ids)
}

// einvoiceCompanies gives the einvoicing context legal entities by code
// and by id.
type einvoiceCompanies struct{}

func (einvoiceCompanies) ByCode(ctx context.Context, code string) (einvoicing.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.EntityByCode(ctx, code)
	return einvoicing.Company{ID: le.ID, Code: le.Code, Name: le.Name}, ok, err
}

func (einvoiceCompanies) Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]einvoicing.Company, error) {
	es, err := tenancy.Directory{}.Entities(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[uuid.UUID]einvoicing.Company{}
	for _, e := range es {
		if want[e.ID] {
			out[e.ID] = einvoicing.Company{ID: e.ID, Code: e.Code, Name: e.Name}
		}
	}
	return out, nil
}

// EInvoiceFetches is the einvoicing context as the fetch workflow's port
// (workflows.EInvoices, ADR-0061 §4).
func EInvoiceFetches(cfg Config) workflows.EInvoices {
	return einvoiceFetches{EInvoicing(cfg)}
}

// einvoiceFetches adapts the einvoicing context's fetch steps to the
// workflow's port, whose messages and counts are the workflow's own types.
type einvoiceFetches struct{ e *einvoicing.Endpoints }

func (einvoiceFetches) Fetcher(tenant uuid.UUID) reqctx.Principal { return einvoicing.Fetcher(tenant) }

func (f einvoiceFetches) PendingEInvoices(ctx context.Context, connector uuid.UUID, in workflows.Within) ([]workflows.EInvoiceMessage, error) {
	ms, err := f.e.PendingEInvoices(ctx, connector, in)
	out := make([]workflows.EInvoiceMessage, len(ms))
	for i, m := range ms {
		out[i] = workflows.EInvoiceMessage{ID: m.ID, Receiver: m.Receiver}
	}
	return out, err
}

func (f einvoiceFetches) RecordEInvoice(ctx context.Context, connector, operation uuid.UUID, m workflows.EInvoiceMessage, in workflows.Within) (string, error) {
	return f.e.RecordEInvoice(ctx, connector, operation, einvoicing.Message{ID: m.ID, Receiver: m.Receiver}, in)
}

func (f einvoiceFetches) ProcessEInvoices(ctx context.Context, connector, operation uuid.UUID, in workflows.Within) (workflows.EInvoiceProcessed, error) {
	p, err := f.e.ProcessEInvoices(ctx, connector, operation, in)
	return workflows.EInvoiceProcessed{Refused: p.Refused, Entered: p.Entered, Held: p.Held, Waiting: p.Waiting}, err
}

func (f einvoiceFetches) QueueEInvoiceFetch(ctx context.Context, operation, connector uuid.UUID, workflowID string) error {
	return f.e.QueueEInvoiceFetch(ctx, operation, connector, workflowID)
}

func (f einvoiceFetches) FinishEInvoiceFetch(ctx context.Context, operation uuid.UUID, counts workflows.EInvoiceCounts, failed []workflows.EInvoiceFailure) error {
	out := make([]einvoicing.Failure, len(failed))
	for i, m := range failed {
		out[i] = einvoicing.Failure{ID: m.ID, Code: m.Code}
	}
	return f.e.FinishEInvoiceFetch(ctx, operation, einvoicing.Counts{Recorded: counts.Recorded, AlreadyRecorded: counts.AlreadyRecorded,
		Changed: counts.Changed, Refused: counts.Refused, Entered: counts.Entered, Held: counts.Held, Waiting: counts.Waiting}, out)
}

func (f einvoiceFetches) FailEInvoiceFetch(ctx context.Context, operation uuid.UUID, why *problem.Problem) error {
	return f.e.FailEInvoiceFetch(ctx, operation, why)
}

// EInvoiceSends is the einvoicing context as the send workflow's port
// (workflows.EInvoiceSends, ADR-0061 As built E3).
func EInvoiceSends(cfg Config) workflows.EInvoiceSends {
	return einvoiceSends{EInvoicing(cfg)}
}

// einvoiceSends adapts the einvoicing context's send step to the
// workflow's port, whose step is the workflow's own type.
type einvoiceSends struct{ e *einvoicing.Endpoints }

func (einvoiceSends) Sender(tenant uuid.UUID) reqctx.Principal { return einvoicing.Sender(tenant) }

func (s einvoiceSends) UnfinishedEInvoiceSends(ctx context.Context, tx pgx.Tx, now time.Time) (map[uuid.UUID]uuid.UUID, error) {
	return s.e.UnfinishedSends(ctx, tx, now)
}

func (s einvoiceSends) FollowedEInvoiceTenants(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error) {
	return einvoicing.FollowedTenants(ctx, tx, now)
}

func (s einvoiceSends) AdvanceEInvoiceSubmission(ctx context.Context, outgoing, operation uuid.UUID, in workflows.Within) (workflows.EInvoiceSendStep, error) {
	step, err := s.e.AdvanceSubmission(ctx, outgoing, operation, in)
	return workflows.EInvoiceSendStep{Done: step.Done, Wait: step.Wait}, err
}
