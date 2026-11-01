package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Payment runs (ADR-0052 §2, ADR-0038, T73f): the runs by status, the
// proposal of a run (an operation, its progress polled on the run's
// page), a run's payments, exclusions and bank files (payment_run.go),
// its operation's progress, cancellation and the resumption of a release
// that stopped (payment_progress.go), and the decisions on it: release,
// reject, the second approval above the company's limit (T72m) and the
// transmission of its bank files (payment_actions.go). Segregation of
// duties is the API's: the proposer, and the approver of the release,
// are refused, and the page says so by the problem's code.

// A list page shows runPageSize runs; with getCurrentPrincipal,
// listLegalEntities, listHouseBanks and listPrincipalNames it makes at
// most five calls (ADR-0052 §10: six).
const runPageSize = 25

// The run statuses the catalog names, and the list's tabs (every status
// first).
var (
	runStatuses = map[string]i18n.Key{"proposing": i18n.RunStatusProposing, "proposed": i18n.RunStatusProposed,
		"releasing": i18n.RunStatusReleasing, "released": i18n.RunStatusReleased, "rejected": i18n.RunStatusRejected, "failed": i18n.RunStatusFailed}
	runTabs = []string{"", "proposing", "proposed", "releasing", "released", "rejected", "failed"}
)

// currencyCode is an ISO 4217 code as PaymentRunPropose takes it.
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// runPath is a run's page.
func runPath(id uuid.UUID) string { return ui.PaymentsPath + "/" + id.String() }

// proposal is what the person sent in the propose form, and what is
// wrong with it; the form is rendered again with it.
type proposal struct {
	company, bank, currency, runDate, dueBy string
	fresh                                   bool // not sent yet: dated its company's today (datedCompany)
	companyErr, currencyErr                 i18n.Key
	runDateErr, dueByErr                    i18n.Key
	refusal                                 *ui.Refusal
}

// runs is the payment runs' list.
func (s *Server) runs(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showRuns(w, r, p, m, http.StatusOK, proposal{fresh: true})
}

// showRuns renders the list as the query asks, with the propose form
// holding what was sent, and its refusal.
func (s *Server) showRuns(w http.ResponseWriter, r *http.Request, p *person, m me, status int, pr proposal) {
	q := r.URL.Query()
	companies := s.companies(r.Context(), m)
	banks := s.houseBanks(r.Context(), m, uuid.Nil)
	l := ui.RunList{Tabs: runStatusTabs(q), ProposeRefusal: pr.refusal,
		Filters: ui.RunFilters{Status: q.Get("status"), Number: strings.TrimSpace(q.Get("document_number")), Companies: companyOptions(companies, q.Get("legal_entity_id"))}}
	if m.may(permProposeRuns) && len(companies) > 0 {
		if pr.fresh {
			// Of the company the list is filtered by, or the only one;
			// among several, the person chooses and types the dates.
			pr.company = datedCompany(q.Get("legal_entity_id"), companyOptions(companies, ""))
			pr.runDate = s.today(r.Context(), m, pr.company)
			pr.dueBy = pr.runDate
		}
		l.Propose = proposeForm(p, companies, banks, pr)
	}
	params, valid := runParams(q, l.Filters)
	if !valid {
		l.Invalid, status = true, http.StatusBadRequest
	} else {
		res, err := s.API.ListPaymentRuns(r.Context(), params)
		page, ok := res.(*gen.PaymentRunPage)
		if !ok {
			prob, show := s.refusal(w, r, p, "listPaymentRuns", res, err)
			if !show {
				return
			}
			l.Refusal = explainIn(i18n.PaymentProblems, i18n.PaymentsRefusedTitle, prob, m)
			if status == http.StatusOK {
				status = int(prob.Status)
			}
		} else {
			l.Rows = s.runRows(r, p, *page, companies, banks)
			if next := page.NextPageToken.Or(""); next != "" {
				l.More = ui.RunsPath(with(q, "page", next))
			}
		}
	}
	s.render(w, r, status, ui.Runs(s.page(i18n.PaymentsTitle, ui.PaymentsPath, p, m), l))
}

// runParams are the API's filters the query asks for, and whether its
// company is an id.
func runParams(q url.Values, f ui.RunFilters) (gen.ListPaymentRunsParams, bool) {
	params := gen.ListPaymentRunsParams{PageSize: gen.NewOptInt32(runPageSize), OrderBy: gen.NewOptString("run_date desc")}
	valid := true
	if v := q.Get("legal_entity_id"); v != "" {
		id, err := uuid.Parse(v)
		valid = err == nil
		params.LegalEntityID = gen.NewOptUUID(id)
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
	return params, valid
}

// runStatusTabs are the run statuses, each keeping the other filters.
func runStatusTabs(q url.Values) []ui.StatusTab {
	tabs := make([]ui.StatusTab, 0, len(runTabs))
	for _, st := range runTabs {
		k := runStatuses[st]
		if st == "" {
			k = i18n.InvoicesStatusAll
		}
		tabs = append(tabs, ui.StatusTab{Label: k, Path: ui.RunsPath(with(q, "status", st)), Current: q.Get("status") == st})
	}
	return tabs
}

// runRows are a page of runs, each with its company, its house bank's
// code and its proposer by name.
func (s *Server) runRows(r *http.Request, p *person, page gen.PaymentRunPage, companies map[uuid.UUID]string, banks map[uuid.UUID]gen.HouseBank) []ui.RunRow {
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, run := range page.Items {
		ids = append(ids, run.ProposedBy)
	}
	names := s.names(r.Context(), p, ids)
	out := make([]ui.RunRow, 0, len(page.Items))
	for _, run := range page.Items {
		row := ui.RunRow{Path: runPath(run.ID), Number: run.DocumentNumber, Status: labelOf(runStatuses, run.Status), Company: companies[run.LegalEntityID],
			RunDate: run.RunDate, DueBy: run.DueBy, Total: moneyOf(run.TotalAmount), ProposedBy: names[run.ProposedBy]}
		if bank, ok := run.HouseBankID.Get(); ok {
			row.Bank = banks[bank].Code
			if row.Bank == "" {
				row.Bank = bank.String() // a bank the person may not read: by its id
			}
		}
		out = append(out, row)
	}
	return out
}

// proposeForm is the propose form, fresh (a new idempotency key) and
// holding what was sent.
func proposeForm(p *person, companies map[uuid.UUID]string, banks map[uuid.UUID]gen.HouseBank, pr proposal) *ui.ProposeForm {
	return &ui.ProposeForm{CSRF: p.CSRF, Key: uuid.NewString(), Companies: companyOptions(companies, pr.company), Banks: bankOptions(banks, pr.bank, true),
		Currency: pr.currency, RunDate: pr.runDate, DueBy: pr.dueBy, CompanyError: pr.companyErr, CurrencyError: pr.currencyErr,
		RunDateError: pr.runDateErr, DueByError: pr.dueByErr}
}

// check is what is wrong with the proposal, checked before the API is
// called so it stays on the page, and the body to send.
func (pr *proposal) check() gen.PaymentRunPropose {
	var body gen.PaymentRunPropose
	var err error
	if body.LegalEntityID, err = uuid.Parse(pr.company); err != nil {
		pr.companyErr = i18n.ProposeCompanyNeeded
	}
	if bank, err := uuid.Parse(pr.bank); err == nil {
		body.HouseBankID = gen.NewOptUUID(bank)
	}
	if pr.currency != "" {
		if !currencyCode.MatchString(pr.currency) {
			pr.currencyErr = i18n.ProposeCurrencyInvalid
		}
		body.Currency = gen.NewOptString(pr.currency)
	}
	body.RunDate, pr.runDateErr = requiredDate(pr.runDate)
	body.DueBy, pr.dueByErr = requiredDate(pr.dueBy)
	return body
}

// valid reports whether the proposal has nothing wrong with it.
func (pr *proposal) valid() bool {
	return pr.companyErr == "" && pr.currencyErr == "" && pr.runDateErr == "" && pr.dueByErr == ""
}

// requiredDate is a year-month-day the form needs, or what is wrong.
func requiredDate(v string) (time.Time, i18n.Key) {
	d, err := dateParam(v)
	if err != "" || !d.Set {
		return time.Time{}, i18n.ActionDateInvalid
	}
	return d.Value, ""
}

// propose proposes a payment run (proposePaymentRun, an operation): done,
// the person goes to the run's page, where its progress is shown;
// refused, the list says why and keeps what was sent.
func (s *Server) propose(w http.ResponseWriter, r *http.Request, p *person) {
	key, bank := r.PostForm.Get(ui.FieldIdempotency), r.PostForm.Get(ui.FieldHouseBank)
	if _, err := uuid.Parse(key); err != nil || (bank != "" && uuid.Validate(bank) != nil) {
		// Not the page's form (its bank is one the form offers, or none):
		// nothing to keep or explain.
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.ProblemInvoiceBody, ui.PaymentsPath, i18n.RunBack))
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	f := r.PostForm
	pr := proposal{company: f.Get(ui.FieldCompany), bank: bank, currency: strings.TrimSpace(f.Get(ui.FieldCurrency)),
		runDate: strings.TrimSpace(f.Get(ui.FieldRunDate)), dueBy: strings.TrimSpace(f.Get(ui.FieldDueBy))}
	body := pr.check()
	if !pr.valid() {
		s.showRuns(w, r, p, m, http.StatusUnprocessableEntity, pr)
		return
	}
	res, err := s.API.ProposePaymentRun(r.Context(), &body, gen.ProposePaymentRunParams{IdempotencyKey: key})
	if op, ok := res.(*gen.OperationHeaders); ok {
		// The operation names its run from the start; one that does not
		// leaves the list to find it in.
		next := ui.PaymentsPath
		if id, ok := runOf(op.Response); ok {
			next = runPath(id) + "?done=proposing"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "proposePaymentRun", res, err); show {
		pr.refusal = explainIn(i18n.PaymentProblems, i18n.ProposeRefusedTitle, prob, m)
		pr.refusal.ID = "propose-refusal"
		s.showRuns(w, r, p, m, int(prob.Status), pr)
	}
}

// runResource is the API path of a payment run, as an operation names
// the run it builds or releases.
const runResource = "/v1/payment-runs/"

// runOf is the run an operation is of, by its resource: a run's from the
// moment it is proposed (proposePaymentRun).
func runOf(op gen.Operation) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(op.Resource.Value, runResource)
	if !ok || op.Resource.Null {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	return id, err == nil
}
