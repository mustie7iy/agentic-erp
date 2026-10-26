package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Supplier invoices and the AP exception queue (ADR-0052 §2, T73e): the
// queue by status (an invoice held at matching is an exception, decided
// on its approval request), an invoice's page with its lines, latest
// match, order, document and the agents' recommendations, and the
// actions the API offers a person (invoice_actions.go).

// A queue page shows invoicePageSize invoices; with getCurrentPrincipal,
// listLegalEntities and listPrincipalNames it makes at most four calls
// (ADR-0052 §10: six).
const invoicePageSize = 25

// The invoice statuses, kinds and sources, the match issues and the
// recommendations' kinds and outcomes the catalog names; any other value
// of these open enumerations is shown as it is.
var (
	invoiceStatuses = map[string]i18n.Key{"parked": i18n.InvoiceStatusParked, "on_hold": i18n.InvoiceStatusOnHold,
		"posted": i18n.InvoiceStatusPosted, "rejected": i18n.InvoiceStatusRejected, "cancelled": i18n.InvoiceStatusCancelled,
		"reversed": i18n.InvoiceStatusReversed}
	invoiceKinds = map[string]i18n.Key{"invoice": i18n.InvoiceKindInvoice, "credit_memo": i18n.InvoiceKindMemo,
		"subsequent_debit": i18n.InvoiceKindDebit, "subsequent_credit": i18n.InvoiceKindLaterReduction}
	invoiceSources = map[string]i18n.Key{"manual": i18n.InvoiceSourceManual, "capture": i18n.InvoiceSourceCapture,
		"edi": i18n.InvoiceSourceEDI, "migration": i18n.InvoiceSourceMigration}
	matchIssues = map[string]i18n.Key{"tax_or_total_mismatch": i18n.MatchIssueTax, "order_not_open": i18n.MatchIssueOrderClosed,
		"order_item_missing": i18n.MatchIssueItemMissing, "price_variance": i18n.MatchIssuePrice,
		"quantity_exceeds_received": i18n.MatchIssueQuantity}
	recommendationKinds = map[string]i18n.Key{"approve_with_variance": i18n.RecKindVariance, "request_credit_note": i18n.RecKindSupplierNote,
		"reject_duplicate": i18n.RecKindDuplicate, "wait_for_goods_receipt": i18n.RecKindWait, "correct_tax": i18n.RecKindTax,
		"other": i18n.RecKindOther}
	recommendationOutcomes = map[string]i18n.Key{"": i18n.RecOpen, "accepted": i18n.RecAccepted, "overridden": i18n.RecOverridden,
		"ignored": i18n.RecIgnored}
	// queueStatuses are the queue's tabs, every status first.
	queueStatuses = []string{"", "parked", "on_hold", "posted", "rejected", "cancelled", "reversed"}
)

// invoicePath is an invoice's page.
func invoicePath(id uuid.UUID) string { return ui.InvoicesPath + "/" + id.String() }

// invoices is the queue: a page of the invoices the filters ask for, with
// the company's name and the agent badge of a capture agent's proposal.
func (s *Server) invoices(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	params, f, valid := queueParams(q)
	l := ui.InvoiceList{Filters: f, Tabs: statusTabs(q)}
	companies := s.companies(r.Context(), m)
	l.Companies = companyOptions(companies, q.Get("legal_entity_id"))
	status := http.StatusOK
	switch {
	case !valid:
		l.Invalid, status = true, http.StatusBadRequest
	case f.FromError != "" || f.ToError != "":
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.ListSupplierInvoices(r.Context(), params)
		page, ok := res.(*gen.SupplierInvoicePage)
		if !ok {
			prob, show := s.refusal(w, r, p, "listSupplierInvoices", res, err)
			if !show {
				return
			}
			l.Refusal, status = explainIn(i18n.InvoiceProblems, i18n.InvoicesRefusedTitle, prob, m), int(prob.Status)
			break
		}
		s.queueRows(r.Context(), p, q, &l, *page, companies)
	}
	s.render(w, r, status, ui.Invoices(s.page(i18n.InvoicesTitle, ui.InvoicesPath, p, m), l))
}

// queueRows fills the queue with a page of invoices, the creators named
// (an agent's invoice is badged), the next page's link and the filtered
// supplier's name.
func (s *Server) queueRows(ctx context.Context, p *person, q url.Values, l *ui.InvoiceList, page gen.SupplierInvoicePage, companies map[uuid.UUID]string) {
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, inv := range page.Items {
		ids = append(ids, inv.CreatedBy)
	}
	names := s.names(ctx, p, ids)
	for _, inv := range page.Items {
		row := ui.InvoiceRow{Path: invoicePath(inv.ID), Number: inv.DocumentNumber, Reference: inv.SupplierReference,
			Supplier: supplierOf(inv), FilterPath: ui.QueuePath(with(q, "supplier_id", inv.SupplierID.String())),
			Company: companies[inv.LegalEntityID], Date: inv.InvoiceDate, Gross: moneyOf(inv.GrossAmount),
			Status: labelOf(invoiceStatuses, inv.Status), Kind: labelOf(invoiceKinds, inv.Kind.Or("invoice")), HoldReason: inv.HoldReason.Or("")}
		if n := names[inv.CreatedBy]; n.Agent {
			row.Agent = &n
		}
		if row.Supplier.ID == l.Filters.Supplier.ID {
			l.Filters.Supplier = row.Supplier
		}
		l.Rows = append(l.Rows, row)
	}
	if next := page.NextPageToken.Or(""); next != "" {
		l.More = ui.QueuePath(with(q, "page", next))
	}
}

// queueParams are the API's filters the query asks for, the filters as
// the page shows them, and whether the query's ids are ids; a date that
// is not one is the filter's error.
func queueParams(q url.Values) (gen.ListSupplierInvoicesParams, ui.InvoiceFilters, bool) {
	params := gen.ListSupplierInvoicesParams{PageSize: gen.NewOptInt32(invoicePageSize), OrderBy: gen.NewOptString("invoice_date desc"),
		Expand: []string{"supplier"}}
	f := ui.InvoiceFilters{Status: q.Get("status"), Number: strings.TrimSpace(q.Get("document_number")),
		From: q.Get("invoice_date_from"), To: q.Get("invoice_date_to"), SupplierClear: ui.QueuePath(with(q, "supplier_id", ""))}
	valid := true
	if v := q.Get("legal_entity_id"); v != "" {
		id, err := uuid.Parse(v)
		valid = err == nil
		params.LegalEntityID = gen.NewOptUUID(id)
	}
	if v := q.Get("supplier_id"); v != "" {
		id, err := uuid.Parse(v)
		valid = valid && err == nil
		params.SupplierID, f.Supplier.ID = gen.NewOptUUID(id), v
	}
	if f.Status != "" {
		params.Status = gen.NewOptString(f.Status)
	}
	if f.Number != "" {
		params.DocumentNumber = gen.NewOptString(f.Number)
	}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	params.InvoiceDateFrom, f.FromError = dateParam(f.From)
	params.InvoiceDateTo, f.ToError = dateParam(f.To)
	return params, f, valid
}

// dateParam is a year-month-day filter, unset when empty, an error when
// it is no date.
func dateParam(v string) (gen.OptDate, i18n.Key) {
	if v == "" {
		return gen.OptDate{}, ""
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return gen.OptDate{}, i18n.FilterDateInvalid
	}
	return gen.NewOptDate(t), ""
}

// with is a copy of q with key set to v ("" removes it), on the first
// page.
func with(q url.Values, key, v string) url.Values {
	out := url.Values{}
	for k, vs := range q {
		out[k] = append([]string(nil), vs...)
	}
	out.Del("page")
	out.Set(key, v)
	return out
}

// statusTabs are the queue's statuses, each keeping the other filters.
func statusTabs(q url.Values) []ui.StatusTab {
	tabs := make([]ui.StatusTab, 0, len(queueStatuses))
	for _, st := range queueStatuses {
		k := invoiceStatuses[st]
		if st == "" {
			k = i18n.InvoicesStatusAll
		}
		tabs = append(tabs, ui.StatusTab{Label: k, Path: ui.QueuePath(with(q, "status", st)), Current: q.Get("status") == st})
	}
	return tabs
}

// companies are the names of the companies the person may see, by id
// (listLegalEntities), none without the permission to list them. A call
// that fails leaves them unnamed, logged: names are no reason to refuse a
// page.
func (s *Server) companies(ctx context.Context, m me) map[uuid.UUID]string {
	entities := s.entities(ctx, m)
	if entities == nil {
		return nil
	}
	out := make(map[uuid.UUID]string, len(entities))
	for id, e := range entities {
		out[id] = e.Name
	}
	return out
}

// entities are the companies the person may see, by id
// (listLegalEntities), as companies reads them: their currencies besides
// their names.
func (s *Server) entities(ctx context.Context, m me) map[uuid.UUID]gen.LegalEntity {
	if !m.may(permReadCompanies) {
		return nil
	}
	res, err := s.API.ListLegalEntities(ctx, gen.ListLegalEntitiesParams{PageSize: gen.NewOptInt32(maxCompanies)})
	page, ok := res.(*gen.LegalEntityPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the companies could not be read", "answer", res, "error", err)
		return nil
	}
	out := make(map[uuid.UUID]gen.LegalEntity, len(page.Items))
	for _, e := range page.Items {
		out[e.ID] = e
	}
	return out
}

// maxCompanies is the most companies a page names, the API's largest page.
const maxCompanies = 500

// companyOptions are the company filter's choices, by name; nil when the
// companies could not be read.
func companyOptions(companies map[uuid.UUID]string, chosen string) []ui.Option {
	if companies == nil {
		return nil
	}
	out := make([]ui.Option, 0, len(companies))
	for id, name := range companies {
		out = append(out, ui.Option{Value: id.String(), Text: name, Selected: id.String() == chosen})
	}
	sortOptions(out)
	return out
}

// supplierOf is an invoice's supplier, named by the API's expansion.
func supplierOf(inv gen.SupplierInvoice) ui.Supplier {
	out := ui.Supplier{ID: inv.SupplierID.String()}
	if e, ok := inv.Expanded.Get(); ok {
		if sup, ok := e.Supplier.Get(); ok {
			out.Number, out.Name = sup.Number, sup.Name
		}
	}
	return out
}

// moneyOf is an amount as the pages show it.
func moneyOf(m gen.Money) ui.Money { return ui.Money{Amount: m.Amount, Currency: m.Currency} }
