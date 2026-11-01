package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A period's page reads the period, the companies, the ledgers and the
// name of who changed it last: with getCurrentPrincipal five calls
// (ADR-0052 §10). Its pre-close checks are a section of their own
// (close_checks.go), which htmx loads into the page once it is there.
//
// The decisions on it (T32): open a closed period, close an open one,
// lock a closed one for good, and ask for a locked one's unlock with a
// reason, which opens a period_unlock request the controller decides in
// the approvals inbox; approved, the period is closed again and opened by
// :open. Each is a real form with the period's ETag as If-Match and the
// form's own idempotency key. Done, the page says so; refused, it is
// rendered again, fresh from the API, with the refusal and what was
// typed.

// The notices of what was just done, by the verb's.
var (
	periodDoneOf = map[string]string{ui.PeriodOpen: "opened", ui.PeriodClose: "closed", ui.PeriodLock: "locked", ui.PeriodUnlock: "unlock"}
	periodDone   = map[string]i18n.Key{"opened": i18n.PeriodDoneOpened, "closed": i18n.PeriodDoneClosed, "locked": i18n.PeriodDoneLocked,
		"unlock": i18n.PeriodDoneUnlock}
)

// periodTyped is what the person sent in a period's form that the API
// refused or that was not valid: the verb, its reason and its error.
type periodTyped struct {
	verb, text string
	textErr    i18n.Key
}

// period is a period's page.
func (s *Server) period(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showPeriod(w, r, p, m, id, http.StatusOK, periodTyped{}, nil)
}

// readPeriod reads the period (getFiscalPeriod) and its ETag. ok is false
// when the answer is already written: not found, or the refusal on a page
// of its own.
func (s *Server) readPeriod(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID) (gen.FiscalPeriod, string, bool) {
	res, err := s.API.GetFiscalPeriod(r.Context(), gen.GetFiscalPeriodParams{ID: id})
	if got, ok := res.(*gen.FiscalPeriodHeaders); ok {
		return got.Response, got.ETag.Or(etag.Format(got.Response.Version)), true
	}
	prob, show := s.refusal(w, r, p, "getFiscalPeriod", res, err)
	switch {
	case !show:
	case prob.Status == http.StatusNotFound:
		s.notFound(w, r)
	default:
		s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(i18n.PeriodTitle, ui.ClosePath, p, m), i18n.PeriodsRefusedTitle, ui.ClosePath,
			i18n.PeriodBack, *explainIn(i18n.PeriodProblems, i18n.PeriodsRefusedTitle, prob, m)))
	}
	return gen.FiscalPeriod{}, "", false
}

// showPeriod reads the period and renders its page with status: the
// forms fresh (the period's current ETag, new idempotency keys) holding
// what the person typed, and the API's refusal of what they asked.
func (s *Server) showPeriod(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, t periodTyped, refused *ui.Refusal) {
	fp, tag, ok := s.readPeriod(w, r, p, m, id)
	if !ok {
		return
	}
	ctx := r.Context()
	pp := ui.PeriodPage{Year: fp.FiscalYear, Period: fp.Period, Back: periodsOf(fp.LegalEntityID, fp.LedgerID, fp.FiscalYear),
		Header: s.periodHeader(ctx, p, m, fp), Done: periodDone[r.URL.Query().Get("done")], Refusal: refused, Audit: auditLink(m, fp.ID)}
	if req, ok := fp.UnlockRequestID.Get(); ok {
		pp.Unlock = ui.ApprovalsPath + "/" + req.String()
	}
	pp.Actions, pp.Why = periodForms(fp, m, p, tag, t)
	if fp.Status != "locked" {
		pp.Checks = checksOf(fp)
	}
	// A revaluation at the period's end posts in it and in the next
	// period, so it is offered while the period is open.
	if fp.Status == "open" && !fp.IsSpecial && m.may(permReadEntries) {
		pp.Revalue = ui.RevaluationFor(url.Values{ui.FieldCompany: {fp.LegalEntityID.String()}, ui.FieldLedger: {fp.LedgerID.String()},
			ui.FieldKeyDate: {fp.EndDate.Format(time.DateOnly)}})
	}
	s.render(w, r, status, ui.Period(s.page(i18n.PeriodTitle, ui.ClosePath, p, m), pp))
}

// periodHeader is a period's summary, its company and ledger named.
func (s *Server) periodHeader(ctx context.Context, p *person, m me, fp gen.FiscalPeriod) ui.PeriodHeader {
	companies, ledgers := s.companies(ctx, m), s.ledgers(ctx, m)
	names := s.names(ctx, p, []uuid.UUID{fp.UpdatedBy})
	return ui.PeriodHeader{Company: companies[fp.LegalEntityID], Ledger: ledgerName(ledgers, fp.LedgerID), Start: fp.StartDate, End: fp.EndDate,
		Special: fp.IsSpecial, Status: labelOf(periodStatuses, fp.Status), UpdatedAt: fp.UpdatedAt, UpdatedBy: names[fp.UpdatedBy]}
}

// checksOf is the pre-close checks of a period, as at its end.
func checksOf(fp gen.FiscalPeriod) string {
	return ui.ChecksPath + "?" + url.Values{ui.FieldCompany: {fp.LegalEntityID.String()}, ui.FieldLedger: {fp.LedgerID.String()},
		ui.FieldYear: {strconv.Itoa(int(fp.FiscalYear))}, ui.FieldPeriod: {strconv.Itoa(int(fp.Period))}, ui.FieldKeyDate: {fp.EndDate.Format(time.DateOnly)},
		"period_id": {fp.ID.String()}}.Encode()
}

// periodForms are the person's decisions on the period in its status
// (advisory, ADR-0052 §7: the API decides each call), each a form with
// the period's ETag and a fresh idempotency key, the one they sent
// holding what they typed; without any, why: a locked period whose
// unlock is asked for already waits for its approval, which the page
// shows instead.
func periodForms(fp gen.FiscalPeriod, m me, p *person, tag string, t periodTyped) ([]ui.PeriodForm, i18n.Key) {
	var verbs []string
	switch {
	case fp.Status == "open":
		verbs = []string{ui.PeriodClose}
	case fp.Status == "closed":
		verbs = []string{ui.PeriodOpen, ui.PeriodLock}
	case fp.Status == "locked" && !fp.UnlockRequestID.Set:
		verbs = []string{ui.PeriodUnlock}
	}
	out := make([]ui.PeriodForm, 0, len(verbs))
	for _, verb := range verbs {
		if !m.may(periodOps[verb].HiddenBy) {
			continue
		}
		f := ui.PeriodForm{Verb: verb, Action: periodPath(fp.ID) + "/" + verb, CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
		if t.verb == verb {
			f.Text, f.TextError = t.text, t.textErr
		}
		out = append(out, f)
	}
	if len(out) == 0 && len(verbs) > 0 {
		return nil, i18n.PeriodNotYours
	}
	return out, ""
}

// decidePeriod takes the decision verb on the period.
func (s *Server) decidePeriod(verb string) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		ifMatch, key, ok := s.formFields(w, r, periodPath(id), i18n.PeriodBack)
		if !ok {
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		t := periodTyped{verb: verb, text: r.PostForm.Get(ui.FieldComment)}
		if t.textErr = reasonError(t.text, ui.PeriodRequires(verb)); t.textErr != "" {
			s.showPeriod(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
			return
		}
		res, err := s.sendPeriod(r.Context(), verb, id, ifMatch, key, t.text)
		if _, ok := res.(*gen.CloseFiscalPeriodOKHeaders); ok {
			http.Redirect(w, r, periodPath(id)+"?done="+periodDoneOf[verb], http.StatusSeeOther)
			return
		}
		if prob, show := s.refusal(w, r, p, periodOps[verb].ID, res, err); show {
			s.showPeriod(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.PeriodProblems, i18n.PeriodRefusedTitle, prob, m))
		}
	}
}

// sendPeriod calls the verb's operation, with the unlock's reason.
func (s *Server) sendPeriod(ctx context.Context, verb string, id uuid.UUID, ifMatch, key, text string) (any, error) {
	switch verb {
	case ui.PeriodOpen:
		return s.API.OpenFiscalPeriod(ctx, gen.OpenFiscalPeriodParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.PeriodClose:
		return s.API.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.PeriodLock:
		return s.API.LockFiscalPeriod(ctx, gen.LockFiscalPeriodParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	default: // ui.PeriodUnlock: the routes are the verbs of periodOps
		return s.API.UnlockFiscalPeriod(ctx, &gen.ReasonInput{Reason: text}, gen.UnlockFiscalPeriodParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	}
}
