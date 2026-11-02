package web

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// An invoice's page reads the invoice, its latest match, the agents'
// recommendations on it, the companies' names and the principals' names:
// with getCurrentPrincipal six calls at most (ADR-0052 §10). The order
// and its receipts are a section of their own (order, in
// invoice_actions.go), which htmx
// loads once the page is there.

// maxRecommendations is the most recommendations an invoice's page shows,
// newest first: an agent recommends once per exception it reviews.
const maxRecommendations = 20

// invoiceDone are the notices of what was just done (?done=…).
var invoiceDone = map[string]i18n.Key{"matched": i18n.InvoiceDoneMatched, "posted": i18n.InvoiceDonePosted,
	"held": i18n.InvoiceDoneHeld, "released": i18n.InvoiceDoneReleased, "cancelled": i18n.InvoiceDoneCancelled,
	"reversed": i18n.InvoiceDoneReversed, "decided": i18n.InvoiceDoneDecided, "changed": i18n.InvoiceDoneChanged,
	"unchanged": i18n.InvoiceDoneUnchanged}

// typed is what the person sent in a form the API refused or that was
// not valid: the action's verb (or the recommendation's id), its text,
// date or outcome, and their errors; the page renders that form with it.
type typed struct {
	verb, rec           string
	text, date, outcome string
	textErr, dateErr    i18n.Key
	outcomeErr          i18n.Key
}

// invoice is an invoice's page.
func (s *Server) invoice(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showInvoice(w, r, p, m, id, http.StatusOK, typed{}, nil)
}

// showInvoice reads the invoice and renders its page with status: the
// forms fresh (the invoice's current ETag, new idempotency keys) holding
// what the person typed, and the API's refusal of what they asked.
func (s *Server) showInvoice(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, t typed, refused *ui.Refusal) {
	inv, tag, ok := s.readInvoice(w, r, p, m, id)
	if !ok {
		return
	}
	ip := ui.InvoicePage{Number: inv.DocumentNumber, Lines: linesOf(inv), HasOrder: inv.PurchaseOrderID.Set, Refusal: refused,
		Done: invoiceDone[r.URL.Query().Get("done")], Pending: inv.Status == "on_hold" && inv.WorkflowRequestID.Set}
	match, ok := s.latestMatch(w, r, p, m, inv, &ip)
	if !ok {
		return
	}
	recs, ok := s.recommendations(w, r, p, m, id, &ip)
	if !ok {
		return
	}
	names := s.names(r.Context(), p, principalsOf(inv, match, recs))
	companies := s.companies(r.Context(), m)
	ip.Header = headerOf(inv, names, companies)
	ip.AgentMade = ip.Header.EnteredBy.Agent
	if match != nil {
		ip.Match = matchOf(*match, names)
	}
	ip.Recs = recommendationViews(recs, names, p, m, id, t)
	if !m.may(permDecideRecommendations) && slices.ContainsFunc(recs, func(rc gen.Recommendation) bool { return !rc.Outcome.Set }) {
		ip.RecsWhy = i18n.RecNotYours
	}
	if inv.PurchaseOrderID.Set && m.may(permReadOrders) && m.may(permReadReceipts) {
		ip.Order = invoicePath(id) + "/order"
	}
	if inv.Source == "capture" {
		ip.Document = invoicePath(id) + "/document"
	}
	ip.Actions, ip.Why = actionForms(inv, m, p, tag, t)
	if inv.Status == "parked" && m.may(permCaptureInvoices) {
		ip.Edit = invoicePath(id) + "/edit"
	}
	s.render(w, r, status, ui.Invoice(s.page(i18n.InvoiceTitle, ui.InvoicesPath, p, m), ip))
}

// readInvoice reads the invoice, its supplier expanded, and its ETag. ok
// is false when the answer is already written: not found, or the API's
// refusal on the queue's page.
func (s *Server) readInvoice(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID) (gen.SupplierInvoice, string, bool) {
	res, err := s.API.GetSupplierInvoice(r.Context(), gen.GetSupplierInvoiceParams{ID: id, Expand: []string{"supplier"}})
	got, ok := res.(*gen.GetSupplierInvoiceOKHeaders)
	if ok {
		return got.Response, got.ETag.Or(etag.Format(got.Response.Version)), true
	}
	prob, show := s.refusal(w, r, p, "getSupplierInvoice", res, err)
	switch {
	case !show:
	case prob.Status == http.StatusNotFound:
		s.notFound(w, r)
	default:
		s.render(w, r, int(prob.Status), ui.Invoices(s.page(i18n.InvoicesTitle, ui.InvoicesPath, p, m),
			ui.InvoiceList{Tabs: statusTabs(nil), Refusal: explainIn(i18n.InvoiceProblems, i18n.InvoicesRefusedTitle, prob, m)}))
	}
	return gen.SupplierInvoice{}, "", false
}

// latestMatch reads the invoice's latest match (listMatchResults, newest
// first), none for an invoice on no order. ok is false when the answer is
// already written; a refusal is shown in the match's place.
func (s *Server) latestMatch(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, ip *ui.InvoicePage) (*gen.MatchResult, bool) {
	if !inv.PurchaseOrderID.Set {
		return nil, true
	}
	res, err := s.API.ListMatchResults(r.Context(), gen.ListMatchResultsParams{ID: inv.ID, PageSize: gen.NewOptInt32(1)})
	page, ok := res.(*gen.MatchResultPage)
	if !ok {
		prob, show := s.refusal(w, r, p, "listMatchResults", res, err)
		if show {
			ip.MatchError = explainIn(i18n.InvoiceProblems, i18n.MatchTitle, prob, m)
			ip.MatchError.ID = "match-refusal"
		}
		return nil, show
	}
	if len(page.Items) == 0 {
		return nil, true
	}
	return &page.Items[0], true
}

// recommendations reads the agents' recommendations on the invoice, for
// a person who may read them. ok is false when the answer is already
// written; a refusal is shown in their place.
func (s *Server) recommendations(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, ip *ui.InvoicePage) ([]gen.Recommendation, bool) {
	if !m.may(permReadRecommendations) {
		return nil, true
	}
	ip.RecsShown = true
	res, err := s.API.ListRecommendations(r.Context(), gen.ListRecommendationsParams{ObjectType: gen.NewOptString("supplier_invoice"),
		ObjectID: gen.NewOptUUID(id), PageSize: gen.NewOptInt32(maxRecommendations)})
	page, ok := res.(*gen.RecommendationPage)
	if !ok {
		prob, show := s.refusal(w, r, p, "listRecommendations", res, err)
		if show {
			ip.RecsError = explainIn(i18n.InvoiceProblems, i18n.RecsRefusedTitle, prob, m)
			ip.RecsError.ID = "recs-refusal"
		}
		return nil, show
	}
	return page.Items, true
}

// principalsOf are the principals an invoice's page names: who entered
// it, who matched it, the agents that recommended and the people who
// decided.
func principalsOf(inv gen.SupplierInvoice, match *gen.MatchResult, recs []gen.Recommendation) []uuid.UUID {
	ids := []uuid.UUID{inv.CreatedBy}
	if match != nil {
		ids = append(ids, match.EvaluatedBy)
	}
	for _, rc := range recs {
		ids = append(ids, rc.AgentID)
		if d, ok := rc.DecidedBy.Get(); ok {
			ids = append(ids, d)
		}
	}
	return ids
}

// headerOf is an invoice's summary.
func headerOf(inv gen.SupplierInvoice, names map[uuid.UUID]ui.Name, companies map[uuid.UUID]string) ui.InvoiceHeader {
	h := ui.InvoiceHeader{Supplier: supplierOf(inv), Reference: inv.SupplierReference, Company: companies[inv.LegalEntityID],
		Kind: labelOf(invoiceKinds, inv.Kind.Or("invoice")), Status: labelOf(invoiceStatuses, inv.Status), HoldReason: inv.HoldReason.Or(""),
		Date: inv.InvoiceDate, Gross: moneyOf(inv.GrossAmount), Tax: moneyOf(inv.TaxAmount), Source: labelOf(invoiceSources, inv.Source),
		EnteredBy: names[inv.CreatedBy], NoOrder: !inv.PurchaseOrderID.Set}
	if d, ok := inv.PostingDate.Get(); ok {
		h.PostingDay = &d
	}
	if d, ok := inv.TaxDate.Get(); ok {
		h.TaxDay = &d
	}
	if req, ok := inv.WorkflowRequestID.Get(); ok {
		h.Exception = ui.ApprovalsPath + "/" + req.String()
	}
	return h
}

// linesOf are an invoice's lines.
func linesOf(inv gen.SupplierInvoice) []ui.Line {
	out := make([]ui.Line, 0, len(inv.Items))
	for i, it := range inv.Items {
		out = append(out, ui.Line{No: it.ItemNo.Or(int32(i + 1)), OrderItem: it.PoItemNo.Or(0), Description: it.Description.Or(""),
			Quantity: it.Quantity.Value, Unit: it.Quantity.Unit, Amount: moneyOf(it.Amount), TaxCode: it.TaxCode.Or("")})
	}
	return out
}

// matchOf is a match as the page shows it.
func matchOf(mr gen.MatchResult, names map[uuid.UUID]ui.Name) *ui.MatchView {
	v := &ui.MatchView{When: mr.EvaluatedAt, By: names[mr.EvaluatedBy], Config: mr.ConfigVersion, Clean: mr.IsClean}
	for _, is := range mr.Issues {
		v.Issues = append(v.Issues, ui.Issue{Label: labelOf(matchIssues, is.Code), Line: is.ItemNo.Or(0), Message: is.Message})
	}
	return v
}

// recommendationViews are the recommendations as the page shows them,
// newest first, each open one with its decision form for a person who
// may decide it (decideRecommendation; advisory, ADR-0052 §7).
func recommendationViews(recs []gen.Recommendation, names map[uuid.UUID]ui.Name, p *person, m me, invoice uuid.UUID, t typed) []ui.RecommendationView {
	out := make([]ui.RecommendationView, 0, len(recs))
	for _, rc := range recs {
		v := ui.RecommendationView{Kind: labelOf(recommendationKinds, rc.Kind), Content: rc.Content, Agent: names[rc.AgentID], At: rc.CreatedAt,
			Outcome: labelOf(recommendationOutcomes, rc.Outcome.Or(""))}
		if d, ok := rc.DecidedBy.Get(); ok {
			n := names[d]
			v.DecidedBy = &n
		}
		if !rc.Outcome.Set && m.may(permDecideRecommendations) {
			f := &ui.RecommendationForm{Action: invoicePath(invoice) + "/recommendations/" + rc.ID.String() + "/decide",
				ID: "rec-" + rc.ID.String(), CSRF: p.CSRF, ETag: etag.Format(rc.Version), Key: uuid.NewString()}
			if t.rec == rc.ID.String() {
				f.Outcome, f.Comment, f.OutcomeError, f.CommentError = t.outcome, t.text, t.outcomeErr, t.textErr
			}
			v.Decide = f
		}
		out = append(out, v)
	}
	return out
}

// invoiceActions are the actions the API offers on an invoice in each
// status, with the permission each needs (advisory, ADR-0052 §7: the API
// decides each call). Matching needs an order.
var invoiceActions = map[string][]string{
	"parked":  {ui.ActMatch, ui.ActPost, ui.ActHold, ui.ActCancel},
	"on_hold": {ui.ActMatch, ui.ActRelease, ui.ActCancel},
	"posted":  {ui.ActReverse},
}

// actionForms are the person's actions on the invoice, each a form with
// the invoice's ETag and a fresh idempotency key, the one they sent
// holding what they typed; without any, why.
func actionForms(inv gen.SupplierInvoice, m me, p *person, tag string, t typed) ([]ui.ActionForm, i18n.Key) {
	verbs, open := invoiceActions[inv.Status]
	var out []ui.ActionForm
	for _, verb := range verbs {
		if !m.may(actionOps[verb].HiddenBy) || (verb == ui.ActMatch && !inv.PurchaseOrderID.Set) {
			continue
		}
		f := ui.ActionForm{Verb: verb, Action: invoicePath(inv.ID) + "/" + verb, CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
		if t.verb == verb {
			f.Text, f.Date, f.TextError, f.DateError = t.text, t.date, t.textErr, t.dateErr
		}
		out = append(out, f)
	}
	switch {
	case len(out) > 0:
		return out, ""
	case open:
		return nil, i18n.ActionsNotYours
	default:
		return nil, i18n.ActionsNone
	}
}

// sortOptions orders a select's choices by their text, then their value.
func sortOptions(o []ui.Option) {
	slices.SortFunc(o, func(a, b ui.Option) int { return cmp.Or(cmp.Compare(a.Text, b.Text), cmp.Compare(a.Value, b.Value)) })
}

// explainIn is explain with a screen's own messages, looked up before the
// catalog's general ones.
func explainIn(screen map[string]i18n.Key, title i18n.Key, prob gen.Problem, m me) *ui.Refusal {
	r := explain(title, prob, m)
	if k, ok := screen[prob.Code]; ok {
		r.Message = k
	}
	return r
}
