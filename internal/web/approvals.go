package web

import (
	"maps"
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

// The approvals (ADR-0052 §2, T73d, T73d2): the person's lists of
// requests, those they may decide now (listApprovalRequests
// decidable_by=me, ADR-0052 G1), those they asked for (requested_by=me)
// and all they may see, filtered by status and kind of object; each
// request's page with its object's own summary, its steps and decisions;
// the decision itself, one request and one click at a time (§7); and the
// requester's withdrawal of their own.

// A list's page shows inboxPageSize requests, reading at most inboxReads
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
		"center_change": i18n.ObjectCenterChange, "payment_run_release": i18n.ObjectRunRelease,
		"gr_ir_clearing": i18n.ObjectGRIRClearing,
	}
	statuses = map[string]i18n.Key{"pending": i18n.StatusPending, "approved": i18n.StatusApproved, "rejected": i18n.StatusRejected,
		"cancelled": i18n.StatusCancelled}
	decisionKinds = map[string]i18n.Key{"approve": i18n.StatusApproved, "reject": i18n.StatusRejected, "cancel": i18n.StatusCancelled}
	// done are the notices of a decision just made (?done=…).
	done = map[string]i18n.Key{"approved": i18n.InboxDoneApproved, "rejected": i18n.InboxDoneRejected, "cancelled": i18n.InboxDoneCancelled}
	// views are the lists' tabs, in their order.
	views = []struct {
		view  string
		label i18n.Key
	}{{ui.ViewDecide, i18n.TabDecide}, {ui.ViewMine, i18n.TabMine}, {ui.ViewAll, i18n.TabAll}}
)

func labelOf(known map[string]i18n.Key, v string) ui.Label { return ui.Label{Key: known[v], Raw: v} }

// listQuery is a list's view and filters, from the page's query: an
// unknown view is the inbox, which takes no status (it holds pending
// requests alone).
type listQuery struct{ view, status, kind string }

func listQueryOf(q url.Values) listQuery {
	l := listQuery{view: q.Get(ui.FieldView), status: q.Get(ui.FieldStatus), kind: q.Get(ui.FieldObjectKind)}
	if l.view != ui.ViewMine && l.view != ui.ViewAll {
		l.view, l.status = ui.ViewDecide, ""
	}
	return l
}

// values are the list's query, with the page token when there is one.
func (l listQuery) values(token string) url.Values {
	return url.Values{ui.FieldView: {l.view}, ui.FieldStatus: {l.status}, ui.FieldObjectKind: {l.kind}, "page": {token}}
}

// params are the list's listApprovalRequests parameters, but its page.
func (l listQuery) params() gen.ListApprovalRequestsParams {
	var params gen.ListApprovalRequestsParams
	switch l.view {
	case ui.ViewDecide:
		params.DecidableBy = gen.NewOptString("me")
	case ui.ViewMine:
		params.RequestedBy = gen.NewOptString("me")
	}
	if l.status != "" {
		params.Status = gen.NewOptString(l.status)
	}
	if l.kind != "" {
		params.ObjectType = gen.NewOptString(l.kind)
	}
	return params
}

// tabs are the lists, the current one marked; each keeps the kind of
// object chosen.
func (l listQuery) tabs() []ui.StatusTab {
	out := make([]ui.StatusTab, 0, len(views))
	for _, v := range views {
		out = append(out, ui.StatusTab{Label: v.label, Path: ui.ListPath(ui.ApprovalsPath, url.Values{ui.FieldView: {v.view}, ui.FieldObjectKind: {l.kind}}),
			Current: v.view == l.view})
	}
	return out
}

// filters are the list's filters as chosen: the statuses on every list
// but the inbox, and the kinds of objects the catalog names, a kind it
// does not name kept as it is.
func (l listQuery) filters() ui.ApprovalFilters {
	f := ui.ApprovalFilters{View: l.view, Kinds: choices(objectKinds, l.kind), Clear: ui.ListPath(ui.ApprovalsPath, url.Values{ui.FieldView: {l.view}})}
	if l.view != ui.ViewDecide {
		f.Statuses = choices(statuses, l.status)
	}
	return f
}

// choices are a select's options, the catalog's values in order and the
// chosen one, also when the catalog does not name it.
func choices(known map[string]i18n.Key, chosen string) []ui.LabelOption {
	values := slices.Sorted(maps.Keys(known))
	if chosen != "" && !slices.Contains(values, chosen) {
		values = append(values, chosen)
	}
	out := make([]ui.LabelOption, 0, len(values))
	for _, v := range values {
		out = append(out, ui.LabelOption{Value: v, Label: labelOf(known, v), Selected: v == chosen})
	}
	return out
}

// inbox is one of the person's lists: as many requests as fill a page,
// the requesters by name, and the next page's link, whose token is the
// API's.
func (s *Server) inbox(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	l := listQueryOf(r.URL.Query())
	in := ui.InboxPage{View: l.view, Tabs: l.tabs(), Filters: l.filters(), Done: done[r.URL.Query().Get("done")]}
	if l.view == ui.ViewDecide && slices.Contains(m.Barred, permDecide) {
		in.Barred = m.Barred
	}
	reqs, token, status, ok := s.readList(w, r, p, m, l, &in)
	if !ok {
		return
	}
	if token != "" {
		in.More = ui.ListPath(ui.ApprovalsPath, l.values(token))
	}
	ids := make([]uuid.UUID, 0, len(reqs))
	for _, a := range reqs {
		ids = append(ids, a.RequesterID)
	}
	names := s.names(r.Context(), p, ids)
	for _, a := range reqs {
		in.Approvals = append(in.Approvals, approvalOf(a, names))
	}
	s.render(w, r, status, ui.Inbox(s.page(ui.ViewTitle(l.view), ui.ApprovalsPath, p, m), in))
}

// readList reads the list's requests, page after page of the API's until
// they fill a page, and the next page's token ("" on the last). A refused
// read is the page's refusal alone (in.Refusal, with its status); ok is
// false when the answer was written instead (apiFailed).
func (s *Server) readList(w http.ResponseWriter, r *http.Request, p *person, m me, l listQuery, in *ui.InboxPage) ([]gen.ApprovalRequest, string, int, bool) {
	token := r.URL.Query().Get("page")
	var reqs []gen.ApprovalRequest
	for range inboxReads {
		params := l.params()
		params.PageSize = gen.NewOptInt32(int32(inboxPageSize - len(reqs)))
		if token != "" {
			params.PageToken = gen.NewOptString(token)
		}
		res, err := s.API.ListApprovalRequests(r.Context(), params)
		page, ok := res.(*gen.ApprovalRequestPage)
		if !ok {
			prob, show := s.refusal(w, r, p, "listApprovalRequests", res, err)
			if show {
				in.Refusal = explain(i18n.InboxRefusedTitle, prob, m)
			}
			return nil, "", int(prob.Status), show
		}
		reqs = append(reqs, page.Items...)
		if token = page.NextPageToken.Or(""); token == "" || len(reqs) >= inboxPageSize {
			break
		}
	}
	return reqs, token, http.StatusOK, true
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

// request is one request's page, with what the person typed before a
// step-up sign-in came between, if anything (keptDraft).
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
	typed, kept := s.keptDraft(w, r, p, id)
	s.showRequest(w, r, p, m, id, http.StatusOK, typed, nil, kept)
}

// notFound is the page of an address that is no page, or not the
// person's: the API's 404 means both (04 §4.9). A signed-in person keeps
// their navigation (T73d2).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, ui.NotFound(s.chrome(r, i18n.NotFoundTitle)))
}

// showRequest reads the request and renders its page with status: its
// object's summary, the decision forms fresh (the request's current ETag,
// new idempotency keys) holding what the person typed (typed), and the
// API's refusal of their decision, if any. kept is the decision whose text
// came back after a step-up sign-in ("" none): in its form when the page
// still offers it, else shown as text that can no longer be sent.
func (s *Server) showRequest(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, typed ui.DecideForms, refused *ui.Refusal,
	kept string) {
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
				ui.InboxPage{Tabs: listQuery{}.tabs(), Filters: listQuery{}.filters(), Refusal: explain(i18n.InboxRefusedTitle, prob, m)}))
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
	if !s.object(w, r, p, m, a, &rp) {
		return
	}
	for _, st := range a.Steps {
		rp.Steps = append(rp.Steps, ui.Step{No: st.StepNo, Required: st.Required, Approvals: st.Approvals, Current: a.Status == "pending" && st.StepNo == a.CurrentStep})
	}
	for _, d := range a.Decisions {
		rp.Decisions = append(rp.Decisions, ui.Decision{Label: labelOf(decisionKinds, d.Decision), By: names[d.ActorID], At: d.DecidedAt, Comment: d.Comment.Or("")})
	}
	typed.Action, typed.CSRF, typed.ETag = rp.Path, p.CSRF, got.ETag.Or(etag.Format(a.Version))
	typed.ApproveKey, typed.RejectKey, typed.WithdrawKey = uuid.NewString(), uuid.NewString(), uuid.NewString()
	if rp.Why = why(a, m); rp.Why == "" {
		rp.Decide = &typed
	}
	if a.Status == "pending" && a.RequesterID == m.ID && m.may(permCancelRequests) {
		rp.Withdraw = &typed
	}
	keptIn(&rp, kept, typed)
	s.render(w, r, status, ui.Request(s.page(i18n.RequestTitle, ui.ApprovalsPath, p, m), rp))
}

// keptIn says on the page what became of a decision's text kept across a
// step-up sign-in (verb, "" none): in its form, not sent, when the page
// still offers that decision; otherwise (the request decided or withdrawn
// meanwhile) as text that can no longer be sent, so it is not lost unseen.
func keptIn(rp *ui.RequestPage, verb string, typed ui.DecideForms) {
	offered := rp.Decide != nil
	text := typed.Comment
	switch verb {
	case "":
		return
	case ui.VerbReject:
		text = typed.Reason
	case ui.VerbWithdraw:
		offered, text = rp.Withdraw != nil, typed.Withdrawal
	}
	if offered {
		rp.Kept = true
		return
	}
	rp.Unsent = text
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

// decide records the person's decision on the request, by verb:
// approveApprovalRequest, rejectApprovalRequest, or the requester's
// cancelApprovalRequest, with the ETag the form carried as If-Match and
// the form's idempotency key, so a double click or a resubmit replays the
// first answer (ADR-0052 §7). Done, it goes back to the list (the
// requester's own, after a withdrawal); refused, the request's page says
// why, with what the person typed kept; a step-up sign-in asked keeps it
// across the sign-in (keepDraft).
func (s *Server) decide(verb string) func(http.ResponseWriter, *http.Request, *person) {
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
			s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvalidBody, back, i18n.RequestBack))
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		typed, bad := typedOf(verb, text)
		if bad != "" {
			s.showRequest(w, r, p, m, id, http.StatusUnprocessableEntity, typed, nil, "")
			return
		}
		res, err := s.send(r, verb, id, ifMatch, key, text)
		if _, ok := res.(*gen.ApproveApprovalRequestOKHeaders); ok {
			http.Redirect(w, r, decidedPath(verb), http.StatusSeeOther)
			return
		}
		if u, ok := res.(*gen.UnauthorizedHeaders); ok && s.stepsUp(p, u) {
			s.keepDraft(w, r, p, draft{Request: id, Verb: verb, Text: text})
		}
		prob, show := s.refusal(w, r, p, decideOps[verb].ID, res, err)
		if show {
			s.showRequest(w, r, p, m, id, int(prob.Status), typed, explain(i18n.RefusalTitle, prob, m), "")
		}
	}
}

// typedOf is what the person typed for the decision verb, in its form's
// field, with what is wrong with it: checked before the API is called so
// the text stays on the page.
func typedOf(verb, text string) (ui.DecideForms, i18n.Key) {
	switch verb {
	case ui.VerbReject:
		bad := commentError(text, true, i18n.DecideReasonNeeded)
		return ui.DecideForms{Reason: text, ReasonError: bad}, bad
	case ui.VerbWithdraw:
		bad := commentError(text, true, i18n.WithdrawReasonNeeded)
		return ui.DecideForms{Withdrawal: text, WithdrawError: bad}, bad
	}
	bad := commentError(text, false, "")
	return ui.DecideForms{Comment: text, CommentError: bad}, bad
}

// decidedPath is where a decision done goes: the inbox, or the requester's own
// list after a withdrawal, with its notice.
func decidedPath(verb string) string {
	switch verb {
	case ui.VerbReject:
		return ui.ApprovalsPath + "?done=rejected"
	case ui.VerbWithdraw:
		return ui.ListPath(ui.ApprovalsPath, url.Values{ui.FieldView: {ui.ViewMine}, "done": {"cancelled"}})
	}
	return ui.ApprovalsPath + "?done=approved"
}

// send calls approveApprovalRequest (with the comment, when one was
// written), rejectApprovalRequest or cancelApprovalRequest.
func (s *Server) send(r *http.Request, verb string, id uuid.UUID, ifMatch, key, text string) (any, error) {
	switch verb {
	case ui.VerbReject:
		return s.API.RejectApprovalRequest(r.Context(), &gen.CommentRequiredInput{Comment: text},
			gen.RejectApprovalRequestParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.VerbWithdraw:
		return s.API.CancelApprovalRequest(r.Context(), &gen.CommentRequiredInput{Comment: text},
			gen.CancelApprovalRequestParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	}
	var body gen.OptDecisionInput
	if text != "" {
		body = gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString(text)})
	}
	return s.API.ApproveApprovalRequest(r.Context(), body, gen.ApproveApprovalRequestParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
}

// commentError is what is wrong with a comment, checked before the API
// is called so the person's text stays on the page: a reason is required
// to reject and to withdraw (CommentRequiredInput; missing, it is said by
// needed), and any has at most 2,000 characters.
func commentError(text string, required bool, needed i18n.Key) i18n.Key {
	switch {
	case required && strings.TrimSpace(text) == "":
		return needed
	case utf8.RuneCountInString(text) > maxComment:
		return i18n.DecideTooLong
	}
	return ""
}
