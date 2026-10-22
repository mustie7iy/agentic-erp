package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The approvals inbox (ADR-0052 §2, T73d): the requests the person may
// decide now (listApprovalRequests decidable_by=me, ADR-0052 G1), each
// request's page with its steps and decisions, and the decision itself,
// one request and one click at a time (§7).

// An inbox page shows inboxPageSize requests, reading at most inboxReads
// pages of the API to fill it: a page of decidable_by=me may hold fewer
// requests than asked, even none, while it has a next page (T73c). With
// getCurrentPrincipal and listPrincipalNames a page makes at most five
// calls, within ADR-0052 §10's six.
const (
	inboxPageSize = 25
	inboxReads    = 3
	// maxComment is DecisionInput's and CommentRequiredInput's maxLength.
	maxComment = 2000
)

// The kinds of objects, the statuses and the decisions the catalog names;
// any other value of these open enumerations is shown as it is.
var (
	objectKinds = map[string]i18n.Key{
		"requisition": i18n.ObjectRequisition, "partner_change": i18n.ObjectPartner, "config_change": i18n.ObjectConfig,
		"invoice_exception": i18n.ObjectInvoice, "period_unlock": i18n.ObjectPeriodUnlock, "manual_entry": i18n.ObjectManualEntry,
		"purchase_order": i18n.ObjectOrder, "order_change": i18n.ObjectOrderChange, "bank_clearing": i18n.ObjectBankClearing,
		"center_change": i18n.ObjectCenterChange,
	}
	statuses = map[string]i18n.Key{"pending": i18n.StatusPending, "approved": i18n.StatusApproved, "rejected": i18n.StatusRejected,
		"cancelled": i18n.StatusCancelled}
	decisionKinds = map[string]i18n.Key{"approve": i18n.StatusApproved, "reject": i18n.StatusRejected, "cancel": i18n.StatusCancelled}
	// done are the notices of a decision just made (?done=…).
	done = map[string]i18n.Key{"approved": i18n.InboxDoneApproved, "rejected": i18n.InboxDoneRejected}
)

func labelOf(known map[string]i18n.Key, v string) ui.Label { return ui.Label{Key: known[v], Raw: v} }

// inbox is the person's inbox: as many requests as fill a page, the
// requesters by name, and the next page's link, whose token is the API's.
func (s *Server) inbox(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	in := ui.InboxPage{Done: done[r.URL.Query().Get("done")]}
	if slices.Contains(m.Barred, permDecide) {
		in.Barred = m.Barred
	}
	status := http.StatusOK
	token := r.URL.Query().Get("page")
	var reqs []gen.ApprovalRequest
	for range inboxReads {
		params := gen.ListApprovalRequestsParams{DecidableBy: gen.NewOptString("me"), PageSize: gen.NewOptInt32(int32(inboxPageSize - len(reqs)))}
		if token != "" {
			params.PageToken = gen.NewOptString(token)
		}
		res, err := s.API.ListApprovalRequests(r.Context(), params)
		page, ok := res.(*gen.ApprovalRequestPageHeaders)
		if !ok {
			prob, show := s.refusal(w, r, p, "listApprovalRequests", res, err)
			if !show {
				return
			}
			in.Refusal, status, token = explain(i18n.InboxRefusedTitle, prob, m), int(prob.Status), ""
			break
		}
		reqs = append(reqs, page.Response.Items...)
		if token = page.Response.NextPageToken.Or(""); token == "" || len(reqs) >= inboxPageSize {
			break
		}
	}
	if token != "" {
		in.More = ui.ApprovalsPath + "?page=" + url.QueryEscape(token)
	}
	if in.Refusal != nil {
		reqs = nil // a refused read shows the refusal alone
	}
	ids := make([]uuid.UUID, 0, len(reqs))
	for _, a := range reqs {
		ids = append(ids, a.RequesterID)
	}
	names := s.names(r.Context(), p, ids)
	for _, a := range reqs {
		in.Approvals = append(in.Approvals, approvalOf(a, names))
	}
	s.render(w, r, status, ui.Inbox(s.page(i18n.InboxTitle, ui.ApprovalsPath, p, m), in))
}

// approvalOf is a request as the pages show it.
func approvalOf(a gen.ApprovalRequest, names map[uuid.UUID]ui.Name) ui.Approval {
	out := ui.Approval{Path: ui.ApprovalsPath + "/" + a.ID.String(), Title: a.Title, Kind: labelOf(objectKinds, a.ObjectType),
		Status: labelOf(statuses, a.Status), Requester: names[a.RequesterID], Requested: a.CreatedAt, Step: a.CurrentStep,
		StepCount: int32(len(a.Steps)), ObjectID: a.ObjectID.String()}
	if m, ok := a.Amount.Get(); ok {
		out.Amount = &ui.Money{Amount: m.Amount, Currency: m.Currency}
	}
	return out
}

// request is one request's page.
func (s *Server) request(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showRequest(w, r, p, m, id, http.StatusOK, ui.DecideForms{}, nil)
}

// notFound is the page of an address that is no page, or not the
// person's: the API's 404 means both (04 §4.9).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, ui.NotFound(s.shell(i18n.NotFoundTitle, "")))
}

// showRequest reads the request and renders its page with status: the
// decision forms fresh (the request's current ETag, new idempotency keys)
// holding what the person typed (typed), and the API's refusal of their
// decision, if any.
func (s *Server) showRequest(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, typed ui.DecideForms, refused *ui.Refusal) {
	res, err := s.API.GetApprovalRequest(r.Context(), gen.GetApprovalRequestParams{ID: id})
	got, ok := res.(*gen.ApprovalRequestHeaders)
	if !ok {
		prob, show := s.refusal(w, r, p, "getApprovalRequest", res, err)
		switch {
		case !show:
		case prob.Status == http.StatusNotFound:
			s.notFound(w, r)
		default:
			s.render(w, r, int(prob.Status), ui.Inbox(s.page(i18n.InboxTitle, ui.ApprovalsPath, p, m),
				ui.InboxPage{Refusal: explain(i18n.InboxRefusedTitle, prob, m)}))
		}
		return
	}
	a := got.Response
	ids := []uuid.UUID{a.RequesterID}
	for _, d := range a.Decisions {
		ids = append(ids, d.ActorID)
	}
	names := s.names(r.Context(), p, ids)
	rp := ui.RequestPage{Approval: approvalOf(a, names), Refusal: refused}
	for _, st := range a.Steps {
		rp.Steps = append(rp.Steps, ui.Step{No: st.StepNo, Required: st.Required, Approvals: st.Approvals, Current: a.Status == "pending" && st.StepNo == a.CurrentStep})
	}
	for _, d := range a.Decisions {
		rp.Decisions = append(rp.Decisions, ui.Decision{Label: labelOf(decisionKinds, d.Decision), By: names[d.ActorID], At: d.DecidedAt, Comment: d.Comment.Or("")})
	}
	if rp.Why = why(a, m); rp.Why == "" {
		typed.Action, typed.CSRF, typed.ETag = rp.Path, p.CSRF, got.ETag.Or(etag.Format(a.Version))
		typed.ApproveKey, typed.RejectKey = uuid.NewString(), uuid.NewString()
		rp.Decide = &typed
	}
	s.render(w, r, status, ui.Request(s.page(i18n.RequestTitle, ui.ApprovalsPath, p, m), rp))
}

// why says why the person is offered no decision (advisory, ADR-0052 §7:
// the API decides): the request is not pending, it is their own, they
// approved it already (a person approves a request once), or they may not
// decide it (the
// decide permission or the object type's approval permission); "" when
// the forms are offered.
func why(a gen.ApprovalRequest, m me) i18n.Key {
	switch {
	case a.Status != "pending":
		return i18n.DecideClosed
	case a.RequesterID == m.ID:
		return i18n.DecideOwn
	case slices.ContainsFunc(a.Decisions, func(d gen.Decision) bool { return d.ActorID == m.ID && d.Decision == "approve" }):
		return i18n.ProblemAlreadyApproved
	case !m.may(permDecide) || !m.may(objectApprovers[a.ObjectType]):
		return i18n.DecideNotYours
	}
	return ""
}

// decide records the person's decision on the request (approve true:
// approveApprovalRequest, else rejectApprovalRequest), with the ETag the
// form carried as If-Match and the form's idempotency key, so a double
// click or a resubmit replays the first answer (ADR-0052 §7). Done, it
// goes back to the inbox; refused, the request's page says why, with what
// the person typed kept.
func (s *Server) decide(approve bool) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		back := ui.ApprovalsPath + "/" + id.String()
		// csrf parsed the form unless htmx sent the token as a header; a
		// body that does not parse leaves the fields empty, refused below.
		_ = r.ParseForm()
		ifMatch, key, text := r.PostForm.Get(ui.FieldIfMatch), r.PostForm.Get(ui.FieldIdempotency), r.PostForm.Get(ui.FieldComment)
		if _, err := uuid.Parse(key); err != nil || ifMatch == "" {
			// Not the page's form: nothing to keep or explain.
			s.render(w, r, http.StatusBadRequest, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.ProblemInvalidBody, back, i18n.RequestBack))
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		bad := commentError(text, !approve)
		typed, op, verb := ui.DecideForms{Comment: text, CommentError: bad}, "approveApprovalRequest", "approved"
		if !approve {
			typed, op, verb = ui.DecideForms{Reason: text, ReasonError: bad}, "rejectApprovalRequest", "rejected"
		}
		if bad != "" {
			s.showRequest(w, r, p, m, id, http.StatusUnprocessableEntity, typed, nil)
			return
		}
		res, err := s.send(r, approve, id, ifMatch, key, text)
		if _, ok := res.(*gen.ApproveApprovalRequestOKHeaders); ok {
			http.Redirect(w, r, ui.ApprovalsPath+"?done="+verb, http.StatusSeeOther)
			return
		}
		prob, show := s.refusal(w, r, p, op, res, err)
		if show {
			s.showRequest(w, r, p, m, id, int(prob.Status), typed, explain(i18n.RefusalTitle, prob, m))
		}
	}
}

// send calls approveApprovalRequest (with the comment, when one was
// written) or rejectApprovalRequest.
func (s *Server) send(r *http.Request, approve bool, id uuid.UUID, ifMatch, key, text string) (any, error) {
	if !approve {
		return s.API.RejectApprovalRequest(r.Context(), &gen.CommentRequiredInput{Comment: text},
			gen.RejectApprovalRequestParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	}
	var body gen.OptDecisionInput
	if text != "" {
		body = gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString(text)})
	}
	return s.API.ApproveApprovalRequest(r.Context(), body, gen.ApproveApprovalRequestParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
}

// commentError is what is wrong with a comment, checked before the API
// is called so the person's text stays on the page: a reason is required
// to reject (CommentRequiredInput), and either has at most 2,000
// characters.
func commentError(text string, required bool) i18n.Key {
	switch {
	case required && strings.TrimSpace(text) == "":
		return i18n.DecideReasonNeeded
	case utf8.RuneCountInString(text) > maxComment:
		return i18n.DecideTooLong
	}
	return ""
}
