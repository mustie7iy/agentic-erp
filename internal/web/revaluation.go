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

// The foreign-currency revaluation (ADR-0042): the preview of what a run
// at a key date, the last day of a period, would value in a company's
// ledger (previewRevaluation, reads only), the post of that run
// (postRevaluationRun, people's alone), and the company's runs. The page
// makes, with getCurrentPrincipal, listLegalEntities, listLedgers,
// listRevaluationRuns and listPrincipalNames, five calls; a preview names
// its accounts besides (listAccounts), and a run's page reads the run and
// its accounts.

// revaluationPageSize is how many of the company's runs the page lists.
const revaluationPageSize = 25

// The values of the open enumerations of a revaluation the catalog names.
var (
	directions   = map[string]i18n.Key{"gain": i18n.DirectionGain, "loss": i18n.DirectionLoss, "none": i18n.DirectionNone}
	rateTypes    = map[string]i18n.Key{"average": i18n.RateAverage, "bank_buying": i18n.RateBankBuying, "bank_selling": i18n.RateBankSelling, "planning": i18n.RatePlanning}
	revaluations = map[string]i18n.Key{"posted": i18n.RevaluationPosted, "cancelled": i18n.RevaluationCancelled}
)

// revaluationRunPath is a run's page.
func revaluationRunPath(id uuid.UUID) string { return ui.RevaluationPath + "/" + id.String() }

// revalued is what the person asked of the revaluation: its company,
// ledger and key date, what is wrong with them, the API's refusal, and
// the preview it answered.
type revalued struct {
	company, ledger, keyDate string
	companyErr, keyDateErr   i18n.Key
	refusal                  *ui.Refusal
	preview                  *gen.RevaluationPreview
}

// revaluation is the revaluation's page, its form holding what the query
// names: a period's page links it with the period's company, ledger and
// end.
func (s *Server) revaluation(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	s.showRevaluation(w, r, p, m, http.StatusOK, revalued{company: q.Get(ui.FieldCompany), ledger: q.Get(ui.FieldLedger), keyDate: q.Get(ui.FieldKeyDate)})
}

// showRevaluation renders the page with status: the form fresh (a new
// idempotency key) holding what was asked, the preview with the post of
// its run, and the company's runs.
func (s *Server) showRevaluation(w http.ResponseWriter, r *http.Request, p *person, m me, status int, rv revalued) {
	ctx := r.Context()
	companies, ledgers := s.companies(ctx, m), s.ledgers(ctx, m)
	options := companyOptions(companies, rv.company)
	company := askedOrFirst(rv.company, options)
	rp := ui.RevaluationPage{Refusal: rv.refusal}
	if m.may(permReadEntries) && len(options) > 0 {
		rp.Form = &ui.RevaluationForm{CSRF: p.CSRF, Key: uuid.NewString(), Companies: options, Ledgers: ledgerOptions(ledgers, rv.ledger),
			KeyDate: rv.keyDate, CompanyError: rv.companyErr, KeyDateError: rv.keyDateErr}
	}
	if rv.preview != nil {
		rp.Preview = s.previewView(ctx, p, m, *rv.preview)
	}
	if !s.revaluationRuns(w, r, p, m, company, ledgers, &rp) {
		return
	}
	s.render(w, r, status, ui.Revaluation(s.page(i18n.RevaluationTitle, ui.ClosePath, p, m), rp))
}

// revaluationRuns reads a page of the company's runs (of every company
// the person may see when none is chosen). ok is false when the answer
// is already written; a refusal is shown in their place.
func (s *Server) revaluationRuns(w http.ResponseWriter, r *http.Request, p *person, m me, company string, ledgers []gen.Ledger, rp *ui.RevaluationPage) bool {
	params := gen.ListRevaluationRunsParams{PageSize: gen.NewOptInt32(revaluationPageSize), OrderBy: gen.NewOptString("key_date desc")}
	q := r.URL.Query()
	if id, err := uuid.Parse(company); err == nil {
		params.LegalEntityID = gen.NewOptUUID(id)
	}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	res, err := s.API.ListRevaluationRuns(r.Context(), params)
	page, ok := res.(*gen.RevaluationRunPageHeaders)
	if !ok {
		rp.RunsError, ok = s.inPlace(w, r, p, m, i18n.RevaluationProblems, "listRevaluationRuns", res, err, i18n.RevaluationRunsRefusedTitle, "runs-refusal")
		return ok
	}
	ids := make([]uuid.UUID, 0, len(page.Response.Items))
	for _, run := range page.Response.Items {
		ids = append(ids, run.CreatedBy)
	}
	names := s.names(r.Context(), p, ids)
	for _, run := range page.Response.Items {
		rp.Runs = append(rp.Runs, ui.RevaluationRow{Path: revaluationRunPath(run.ID), KeyDate: run.KeyDate, Ledger: ledgerName(ledgers, run.LedgerID),
			Status: labelOf(revaluations, run.Status), Items: run.ItemCount, CreatedBy: names[run.CreatedBy], CreatedAt: run.CreatedAt})
	}
	if next := page.Response.NextPageToken.Or(""); next != "" {
		rp.RunsMore = ui.RevaluationFor(url.Values{ui.FieldCompany: {company}, "page": {next}})
	}
	return true
}

// check is what is wrong with the request, checked before the API is
// called so it stays on the page, and the body to send.
func (rv *revalued) check() gen.RevaluationPreviewRequest {
	var body gen.RevaluationPreviewRequest
	var err error
	if body.LegalEntityID, err = uuid.Parse(rv.company); err != nil {
		rv.companyErr = i18n.ProposeCompanyNeeded
	}
	if ledger, err := uuid.Parse(rv.ledger); err == nil {
		body.LedgerID = gen.NewOptUUID(ledger)
	}
	body.KeyDate, rv.keyDateErr = requiredDate(rv.keyDate)
	return body
}

// revaluationForm reads the revaluation's fields of a posted form. ok is false, the
// answer written, when the form is not the page's: no idempotency key,
// or a ledger that is no id (the form offers ids, or none).
func (s *Server) revaluationForm(w http.ResponseWriter, r *http.Request) (revalued, string, bool) {
	f := r.PostForm
	rv := revalued{company: f.Get(ui.FieldCompany), ledger: f.Get(ui.FieldLedger), keyDate: strings.TrimSpace(f.Get(ui.FieldKeyDate))}
	key := f.Get(ui.FieldIdempotency)
	if _, err := uuid.Parse(key); err != nil || (rv.ledger != "" && uuid.Validate(rv.ledger) != nil) {
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.ProblemInvoiceBody,
			ui.RevaluationPath, i18n.RevaluationRunBack))
		return revalued{}, "", false
	}
	return rv, key, true
}

// previewRevaluation previews the revaluation the form asks for: the page
// shows what it would value, with the post of its run; refused, the page
// says why and keeps what was sent.
func (s *Server) previewRevaluation(w http.ResponseWriter, r *http.Request, p *person) {
	rv, key, ok := s.revaluationForm(w, r)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	body := rv.check()
	if rv.companyErr != "" || rv.keyDateErr != "" {
		s.showRevaluation(w, r, p, m, http.StatusUnprocessableEntity, rv)
		return
	}
	res, err := s.API.PreviewRevaluation(r.Context(), &body, gen.PreviewRevaluationParams{IdempotencyKey: key})
	if got, ok := res.(*gen.RevaluationPreviewHeaders); ok {
		rv.preview = &got.Response
		s.showRevaluation(w, r, p, m, http.StatusOK, rv)
		return
	}
	if prob, show := s.refusal(w, r, p, "previewRevaluation", res, err); show {
		rv.refusal = explainIn(i18n.RevaluationProblems, i18n.PreviewRefusedTitle, prob, m)
		s.showRevaluation(w, r, p, m, int(prob.Status), rv)
	}
}

// postRevaluation posts the run the preview valued: done, the person goes
// to the run's page; refused (a run of the key date posted already, a
// period closed meanwhile), the page says why.
func (s *Server) postRevaluation(w http.ResponseWriter, r *http.Request, p *person) {
	rv, key, ok := s.revaluationForm(w, r)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	body := rv.check()
	if rv.companyErr != "" || rv.keyDateErr != "" {
		s.showRevaluation(w, r, p, m, http.StatusUnprocessableEntity, rv)
		return
	}
	res, err := s.API.PostRevaluationRun(r.Context(), &body, gen.PostRevaluationRunParams{IdempotencyKey: key})
	if run, ok := res.(*gen.RevaluationRunHeaders); ok {
		http.Redirect(w, r, revaluationRunPath(run.Response.ID)+"?done=posted", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "postRevaluationRun", res, err); show {
		rv.refusal = explainIn(i18n.RevaluationProblems, i18n.RevaluationPostRefusedTitle, prob, m)
		s.showRevaluation(w, r, p, m, int(prob.Status), rv)
	}
}

// previewView is a preview as the page shows it, its accounts by number,
// with the post of its run for a person who may post.
func (s *Server) previewView(ctx context.Context, p *person, m me, pr gen.RevaluationPreview) *ui.PreviewView {
	accounts := s.accounts(ctx, m, pr.LegalEntityID)
	v := &ui.PreviewView{KeyDate: pr.KeyDate, Year: pr.FiscalYear, Period: pr.FiscalPeriod, RateType: labelOf(rateTypes, pr.RateType),
		Totals: totalViews(pr.Totals, accounts), Items: itemViews(pr.Items, accounts)}
	if m.may(permPostEntries) {
		v.Post = &ui.PostForm{CSRF: p.CSRF, Key: uuid.NewString(), Company: pr.LegalEntityID.String(), Ledger: pr.LedgerID.String(),
			KeyDate: pr.KeyDate.Format(time.DateOnly)}
	}
	return v
}

// revaluationRun is a run's page.
func (s *Server) revaluationRun(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	res, err := s.API.GetRevaluationRun(r.Context(), gen.GetRevaluationRunParams{ID: id})
	got, ok := res.(*gen.GetRevaluationRunOKHeaders)
	if !ok {
		prob, show := s.refusal(w, r, p, "getRevaluationRun", res, err)
		switch {
		case !show:
		case prob.Status == http.StatusNotFound:
			s.notFound(w, r)
		default:
			s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(i18n.RevaluationRunTitle, ui.ClosePath, p, m), i18n.RevaluationRunsRefusedTitle,
				ui.RevaluationPath, i18n.RevaluationRunBack, *explainIn(i18n.RevaluationProblems, i18n.RevaluationRunsRefusedTitle, prob, m)))
		}
		return
	}
	run := got.Response
	ctx := r.Context()
	companies, ledgers, accounts := s.companies(ctx, m), s.ledgers(ctx, m), s.accounts(ctx, m, run.LegalEntityID)
	names := s.names(ctx, p, []uuid.UUID{run.CreatedBy})
	rp := ui.RevaluationRunPage{Done: revaluationDone[r.URL.Query().Get("done")], Totals: totalViews(run.Totals, accounts), Items: itemViews(run.Items, accounts),
		Back: ui.RevaluationFor(url.Values{ui.FieldCompany: {run.LegalEntityID.String()}, ui.FieldLedger: {run.LedgerID.String()}}),
		Header: ui.RunSummary{Company: companies[run.LegalEntityID], Ledger: ledgerName(ledgers, run.LedgerID), KeyDate: run.KeyDate,
			RateType: labelOf(rateTypes, run.RateType), Status: labelOf(revaluations, run.Status), Items: run.ItemCount, CreatedBy: names[run.CreatedBy],
			CreatedAt: run.CreatedAt}}
	rp.Valuation, rp.Reset = idText(run.ValuationEntryID), idText(run.ReversalEntryID)
	s.render(w, r, http.StatusOK, ui.RevaluationRun(s.page(i18n.RevaluationRunTitle, ui.ClosePath, p, m), rp))
}

// revaluationDone is the notice of a run just posted.
var revaluationDone = map[string]i18n.Key{"posted": i18n.RevaluationDonePosted}

// maxAccounts is the most accounts a page names, the API's largest page.
const maxAccounts = 500

// accounts are the numbers and names of the company's accounts, by id,
// as its chart's page holds them (chartOf); none without the permission
// to list them or when they could not be read (logged): an account's
// number is no reason to refuse a page.
func (s *Server) accounts(ctx context.Context, m me, company uuid.UUID) map[uuid.UUID]string {
	return s.chartOf(ctx, m, company).names()
}

// accountText is an account by number and name, its id when it could not
// be named.
func accountText(accounts map[uuid.UUID]string, id uuid.UUID) string {
	if v, ok := accounts[id]; ok {
		return v
	}
	return id.String()
}

// totalViews are a revaluation's totals.
func totalViews(totals []gen.RevaluationTotal, accounts map[uuid.UUID]string) []ui.TotalView {
	out := make([]ui.TotalView, 0, len(totals))
	for _, t := range totals {
		out = append(out, ui.TotalView{Account: accountText(accounts, t.AccountID), Currency: t.Currency, Direction: labelOf(directions, t.Direction),
			Items: t.ItemCount, Amount: moneyOf(t.Amount), Booked: moneyOf(t.AmountLc), Valued: moneyOf(t.ValuedLc), Difference: moneyOf(t.DifferenceLc),
			DifferenceGC: moneyOf(t.DifferenceGc)})
	}
	return out
}

// itemViews are a revaluation's valued items.
func itemViews(items []gen.RevaluationItem, accounts map[uuid.UUID]string) []ui.ItemView {
	out := make([]ui.ItemView, 0, len(items))
	for _, it := range items {
		out = append(out, ui.ItemView{Account: accountText(accounts, it.AccountID), Partner: idText(it.PartnerID), Entry: idText(it.JournalEntryID),
			Line: it.LineNo.Or(0), Amount: moneyOf(it.Amount), Booked: moneyOf(it.AmountLc), Rate: string(it.Rate.Or("")), Valued: moneyOf(it.ValuedLc),
			Difference: moneyOf(it.DifferenceLc), DifferenceGC: moneyOf(it.DifferenceGc), Direction: labelOf(directions, it.Direction)})
	}
	return out
}
