package web

import (
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A run's page reads the run, a page of its payments and of its
// exclusions, its bank files once it is released or being released, and
// the principals' names: with getCurrentPrincipal six calls (ADR-0052
// §10). Its operation's progress is a section of its own
// (payment_progress.go), which htmx loads into the page once it is
// there, so no page's reads pass six.

// The run page's lists show runListSize payments and exclusions a page,
// each paged by its own token; a run writes a bank file per house bank
// and payment method, so a page of runListSize holds them.
const runListSize = 50

// The values of the open enumerations of a run's page the catalog names:
// the release approval's states, the reasons a supplier is left out, the
// bank files' statuses, and the notices of what was just done.
var (
	releaseApprovals = map[string]i18n.Key{"pending": i18n.ReleaseApprovalPending, "approved": i18n.ReleaseApprovalApproved,
		"rejected": i18n.ReleaseApprovalRejected, "withdrawn": i18n.ReleaseApprovalWithdrawn}
	exclusionReasons = map[string]i18n.Key{"supplier_blocked": i18n.ExclusionSupplierBlocked, "payment_blocked": i18n.ExclusionPaymentBlocked,
		"no_bank_account": i18n.ExclusionNoBankAccount, "bank_change_pending": i18n.ExclusionBankChangePending,
		"bank_change_hold": i18n.ExclusionBankChangeHold, "debit_balance": i18n.ExclusionDebitBalance, "item_reversed": i18n.ExclusionItemReversed,
		"item_not_open": i18n.ExclusionItemNotOpen, "tax_code_invalid": i18n.ExclusionTaxCodeInvalid, "no_house_bank": i18n.ExclusionNoHouseBank}
	fileStatuses = map[string]i18n.Key{"created": i18n.FileStatusCreated, "to_send": i18n.FileStatusToSend, "sending": i18n.FileStatusSending,
		"transmitted": i18n.FileStatusTransmitted, "failed": i18n.FileStatusFailed, "acknowledged": i18n.FileStatusAcknowledged,
		"rejected": i18n.FileStatusRejected}
	runDone = map[string]i18n.Key{"proposing": i18n.RunDoneProposing, "releasing": i18n.RunDoneReleasing, "rejected": i18n.RunDoneRejected,
		"submitted": i18n.RunDoneSubmitted, "transmitting": i18n.RunDoneTransmitting, "cancelled": i18n.RunDoneCancelled}
)

// runTyped is what the person sent in a run's form that the API refused
// or that was not valid: the decision's verb, its text and its error,
// and whether the refusal asks for the second approval, which the page
// then offers.
type runTyped struct {
	verb, text  string
	textErr     i18n.Key
	askApproval bool
}

// run is a run's page.
func (s *Server) run(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showRun(w, r, p, m, id, http.StatusOK, runTyped{}, nil)
}

// readRun reads the run (getPaymentRun) and its ETag. ok is false when
// the answer is already written: not found, or the refusal on a page of
// its own.
func (s *Server) readRun(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID) (gen.PaymentRun, string, bool) {
	res, err := s.API.GetPaymentRun(r.Context(), gen.GetPaymentRunParams{ID: id})
	got, ok := res.(*gen.PaymentRunHeaders)
	if ok {
		return got.Response, got.ETag.Or(etag.Format(got.Response.Version)), true
	}
	prob, show := s.refusal(w, r, p, "getPaymentRun", res, err)
	switch {
	case !show:
	case prob.Status == http.StatusNotFound:
		s.notFound(w, r)
	default:
		s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(i18n.RunTitle, ui.PaymentsPath, p, m), i18n.PaymentsRefusedTitle, ui.PaymentsPath,
			i18n.RunBack, *explainIn(i18n.PaymentProblems, i18n.PaymentsRefusedTitle, prob, m)))
	}
	return gen.PaymentRun{}, "", false
}

// showRun reads the run and renders its page with status: the forms
// fresh (the run's current ETag, new idempotency keys) holding what the
// person typed, and the API's refusal of what they asked.
func (s *Server) showRun(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, t runTyped, refused *ui.Refusal) {
	run, tag, ok := s.readRun(w, r, p, m, id)
	if !ok {
		return
	}
	q := r.URL.Query()
	rp := ui.RunPage{Number: run.DocumentNumber, Done: runDone[q.Get("done")], Refusal: refused, Building: run.Status == "proposing",
		FilesShown: run.Status == "released" || run.Status == "releasing"}
	// A failed run's progress says why its proposal stopped.
	if (run.Status == "proposing" || run.Status == "releasing" || run.Status == "failed") && m.may(permReadOperations) {
		rp.Progress = runPath(id) + "/progress"
	}
	if !rp.Building && !s.runLists(w, r, p, m, id, q, &rp) {
		return
	}
	if rp.FilesShown && !s.runFiles(w, r, p, m, run, &rp) {
		return
	}
	names := s.names(r.Context(), p, runPrincipals(run))
	rp.Header, rp.Approval = runHeader(run, names), releaseApproval(run, names)
	rp.Actions, rp.Why = runForms(run, m, p, tag, t)
	s.render(w, r, status, ui.Run(s.page(i18n.RunTitle, ui.PaymentsPath, p, m), rp))
}

// runLists reads a page of the run's payments and one of its exclusions,
// each at its own token. ok is false when the answer is already written;
// a refusal is shown in the list's place.
func (s *Server) runLists(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, q url.Values, rp *ui.RunPage) bool {
	pp := gen.ListPaymentsParams{ID: id, PageSize: gen.NewOptInt32(runListSize)}
	if v := q.Get("payments_page"); v != "" {
		pp.PageToken = gen.NewOptString(v)
	}
	res, err := s.API.ListPayments(r.Context(), pp)
	if page, ok := res.(*gen.PaymentPageHeaders); ok {
		rp.Payments = paymentViews(page.Response.Items)
		if next := page.Response.NextPageToken.Or(""); next != "" {
			rp.PaymentsMore = runPath(id) + "?" + with(without(q, "done"), "payments_page", next).Encode()
		}
	} else if rp.PaymentsError, ok = s.inPlace(w, r, p, m, i18n.PaymentProblems, "listPayments", res, err, i18n.PaymentsRefusedTitle, "payments-refusal"); !ok {
		return false
	}
	ep := gen.ListPaymentExclusionsParams{ID: id, PageSize: gen.NewOptInt32(runListSize)}
	if v := q.Get("exclusions_page"); v != "" {
		ep.PageToken = gen.NewOptString(v)
	}
	eres, err := s.API.ListPaymentExclusions(r.Context(), ep)
	if page, ok := eres.(*gen.PaymentExclusionPageHeaders); ok {
		rp.Exclusions = exclusionViews(page.Response.Items)
		if next := page.Response.NextPageToken.Or(""); next != "" {
			rp.ExclusionsMore = runPath(id) + "?" + with(without(q, "done"), "exclusions_page", next).Encode()
		}
		return true
	}
	var ok bool
	rp.ExclusionsError, ok = s.inPlace(w, r, p, m, i18n.PaymentProblems, "listPaymentExclusions", eres, err, i18n.PaymentsRefusedTitle, "exclusions-refusal")
	return ok
}

// runFiles reads the bank files of a run released or being released,
// each with its transmission's form while it may be sent. ok is false
// when the answer is already written; a refusal is shown in their place.
func (s *Server) runFiles(w http.ResponseWriter, r *http.Request, p *person, m me, run gen.PaymentRun, rp *ui.RunPage) bool {
	res, err := s.API.ListBankFiles(r.Context(), gen.ListBankFilesParams{PaymentRunID: gen.NewOptUUID(run.ID), PageSize: gen.NewOptInt32(runListSize),
		OrderBy: gen.NewOptString("created_at")})
	page, ok := res.(*gen.BankFilePageHeaders)
	if !ok {
		rp.FilesError, ok = s.inPlace(w, r, p, m, i18n.PaymentProblems, "listBankFiles", res, err, i18n.PaymentsRefusedTitle, "files-refusal")
		return ok
	}
	for _, f := range page.Response.Items {
		v := ui.BankFileView{Format: f.Format, Method: f.PaymentMethod.Or(""), Status: labelOf(fileStatuses, f.Status), Created: f.CreatedAt}
		if (f.Status == "created" || f.Status == "failed") && m.may(permTransmitFiles) {
			v.Transmit = &ui.RunForm{Verb: ui.RunTransmit, ID: "file-" + f.ID.String(), Action: runPath(run.ID) + "/" + ui.RunTransmit, CSRF: p.CSRF,
				ETag: etag.Format(f.Version), Key: uuid.NewString(), File: f.ID.String()}
		}
		rp.Files = append(rp.Files, v)
	}
	return true
}

// paymentViews are a page of a run's payments.
func paymentViews(items []gen.Payment) []ui.PaymentView {
	out := make([]ui.PaymentView, 0, len(items))
	for _, pay := range items {
		v := ui.PaymentView{Supplier: pay.SupplierID.String(), Amount: moneyOf(pay.Amount), Method: pay.PaymentMethod.Or(""), EndToEnd: pay.EndToEndID,
			Items: int32(len(pay.Items)), DownPayment: pay.Kind.Or("items") == "down_payment", Posted: pay.JournalEntryID.Set}
		v.Discount, v.Withholding = nonZero(pay.DiscountAmount), nonZero(pay.WithholdingAmount)
		out = append(out, v)
	}
	return out
}

// nonZero is an optional amount that is not zero, nil otherwise.
func nonZero(m gen.OptMoney) *ui.Money {
	if v, ok := m.Get(); ok && !ui.Zero(v.Amount) {
		out := moneyOf(v)
		return &out
	}
	return nil
}

// exclusionViews are a page of what a run left out.
func exclusionViews(items []gen.PaymentExclusion) []ui.ExclusionView {
	out := make([]ui.ExclusionView, 0, len(items))
	for _, ex := range items {
		out = append(out, ui.ExclusionView{Supplier: ex.SupplierID.String(), Reason: labelOf(exclusionReasons, ex.ReasonCode), Amount: moneyOf(ex.Amount),
			DownPayment: idText(ex.DownPaymentRequestID)})
	}
	return out
}

// runPrincipals are the principals a run's page names: who proposed it,
// who decided it, and who approved its release.
func runPrincipals(run gen.PaymentRun) []uuid.UUID {
	ids := []uuid.UUID{run.ProposedBy}
	if by, ok := run.DecidedBy.Get(); ok {
		ids = append(ids, by)
	}
	if a, ok := run.ReleaseApproval.Get(); ok && a.ApprovedBy.Set {
		ids = append(ids, a.ApprovedBy.Value)
	}
	return ids
}

// runHeader is a run's summary.
func runHeader(run gen.PaymentRun, names map[uuid.UUID]ui.Name) ui.RunHeader {
	h := ui.RunHeader{Status: labelOf(runStatuses, run.Status), RunDate: run.RunDate, DueBy: run.DueBy, Total: moneyOf(run.TotalAmount),
		ProposedBy: names[run.ProposedBy]}
	if by, ok := run.DecidedBy.Get(); ok {
		n := names[by]
		h.DecidedBy = &n
	}
	if at, ok := run.DecidedAt.Get(); ok {
		h.DecidedAt = &at
	}
	return h
}

// releaseApproval is the run's request for the second approval of its
// release, nil when none was asked for.
func releaseApproval(run gen.PaymentRun, names map[uuid.UUID]ui.Name) *ui.ReleaseApproval {
	a, ok := run.ReleaseApproval.Get()
	if !ok {
		return nil
	}
	v := &ui.ReleaseApproval{Status: labelOf(releaseApprovals, a.Status), Requested: moneyOf(a.RequestedTotal), Request: ui.ApprovalsPath + "/" + a.RequestID.String(),
		Pending: a.Status == "pending"}
	if by, ok := a.ApprovedBy.Get(); ok {
		n := names[by]
		v.ApprovedBy = &n
	}
	if at, ok := a.ApprovedAt.Get(); ok {
		v.ApprovedAt = &at
	}
	return v
}

// decider is why the person may not decide the run, "" when they may
// (advisory, ADR-0052 §7): they lack the permission, or segregation of
// duties keeps its proposer from releasing or rejecting it (ADR-0038).
func decider(run gen.PaymentRun, m me) i18n.Key {
	switch {
	case !m.may(permReleaseRuns):
		return i18n.RunNotYours
	case run.ProposedBy == m.ID:
		return i18n.RunOwn
	}
	return ""
}

// approver reports whether the person approved the run's release, which
// keeps them from releasing it, not from rejecting it (ADR-0038 as
// amended by T72m).
func approver(run gen.PaymentRun, m me) bool {
	a, asked := run.ReleaseApproval.Get()
	return asked && a.ApprovedBy.Or(uuid.Nil) == m.ID
}

// runForms are the person's decisions on a proposed run (advisory,
// ADR-0052 §7: the API decides each call), each a form with the run's
// ETag and a fresh idempotency key, the one they sent holding what they
// typed; without any, why. Its proposer decides nothing and the
// approver of its release may only reject it (segregation of duties);
// while the second approval waits, the run may be rejected but not
// released; the second approval is asked for once the release answered
// that it needs one (or a new one), or after a request was withdrawn.
func runForms(run gen.PaymentRun, m me, p *person, tag string, t runTyped) ([]ui.RunForm, i18n.Key) {
	if run.Status != "proposed" {
		return nil, ""
	}
	if why := decider(run, m); why != "" {
		return nil, why
	}
	a, asked := run.ReleaseApproval.Get()
	var verbs []string
	why := i18n.Key("")
	switch {
	case approver(run, m):
		why = i18n.RunApprover
	case !asked || a.Status != "pending":
		verbs = append(verbs, ui.RunRelease)
	}
	if t.askApproval || (asked && a.Status == "withdrawn") {
		verbs = append(verbs, ui.RunSubmit)
	}
	verbs = append(verbs, ui.RunReject)
	out := make([]ui.RunForm, 0, len(verbs))
	for _, verb := range verbs {
		f := ui.RunForm{Verb: verb, ID: "run-" + verb, Action: runPath(run.ID) + "/" + verb, CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
		if t.verb == verb {
			f.Text, f.TextError = t.text, t.textErr
		}
		out = append(out, f)
	}
	return out, why
}
