package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// EInvoicing is the einvoicing context as the API wires it (ADR-0061 §2):
// a leaf that reads the integration, tenancy and operations contexts
// through the adapters below, so none of them imports it and it imports
// none of them. Its providers are called through cfg.Providers.
func EInvoicing(cfg Config) *einvoicing.Endpoints {
	return &einvoicing.Endpoints{
		Connectors: einvoiceConnectors{integration.EInvoiceProviders{Secrets: resolver(cfg.Secrets), RefuseStandIn: cfg.RefuseEBICS}},
		Companies:  einvoiceCompanies{}, Runs: runOperations{kind: einvoicing.KindFetch}, Objects: cfg.Objects,
		Adapter: einvoicing.Adapters(cfg.Providers), Pages: cfg.Pages, Now: cfg.Now}
}

// einvoiceConnectors gives the einvoicing context the einvoice_provider
// connectors, over integration's published EInvoiceProviders.
type einvoiceConnectors struct{ p integration.EInvoiceProviders }

func (c einvoiceConnectors) Provider(ctx context.Context, id uuid.UUID) (einvoicing.Connector, error) {
	p, err := c.p.Provider(ctx, id)
	return einvoicing.Connector{ID: p.ID, Name: p.Name, Adapter: p.Adapter, BaseURL: p.BaseURL, Participants: p.Participants, Format: p.Format,
		Secret: p.Secret}, err
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

func (f einvoiceFetches) FinishEInvoiceFetch(ctx context.Context, operation uuid.UUID, counts workflows.EInvoiceCounts, failed []workflows.EInvoiceFailure) error {
	out := make([]einvoicing.Failure, len(failed))
	for i, m := range failed {
		out[i] = einvoicing.Failure{ID: m.ID, Code: m.Code}
	}
	return f.e.FinishEInvoiceFetch(ctx, operation, einvoicing.Counts{Recorded: counts.Recorded, AlreadyRecorded: counts.AlreadyRecorded}, out)
}

func (f einvoiceFetches) FailEInvoiceFetch(ctx context.Context, operation uuid.UUID, why *problem.Problem) error {
	return f.e.FailEInvoiceFetch(ctx, operation, why)
}
