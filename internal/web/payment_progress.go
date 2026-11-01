package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A run's latest operation (ADR-0012): the proposal that builds it, or
// its release. Its progress is a section htmx loads into the run's page
// and then asks again every two seconds for its status alone while it
// runs (hx-trigger "every 2s"; no script of the workspace's own); once it
// has stopped, that answer has the browser load the run's page again
// (HX-Redirect), which shows what the operation left. Without htmx it is
// a page of its own with a link to refresh it. While it runs it may be
// cancelled, and a release that stopped is resumed by releasing again.
// The section reads the run and the operation: with getCurrentPrincipal
// three calls.

// The kinds and statuses of operations the catalog names.
var (
	operationKinds    = map[string]i18n.Key{"payments.propose": i18n.OperationPropose, "payments.release": i18n.OperationRelease}
	operationStatuses = map[string]i18n.Key{"queued": i18n.OperationQueued, "running": i18n.OperationRunning, "succeeded": i18n.OperationSucceeded,
		"failed": i18n.OperationFailed, "cancelled": i18n.OperationCancelled}
)

// running reports whether an operation is still to finish, so it may be
// cancelled and its progress moves.
func running(op gen.Operation) bool { return op.Status == "queued" || op.Status == "running" }

// progressPath is the progress section of a run.
func progressPath(id uuid.UUID) string { return runPath(id) + "/progress" }

// progress is a run's operation: the section htmx loads into the run's
// page (HX-Request), its status alone when the section asks again
// (part=status), or a page of its own for a browser without htmx.
func (s *Server) progress(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Add("Vary", "HX-Request") // its refusals too: a page or a section
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showProgress(w, r, p, m, id, http.StatusOK, nil)
}

// showProgress reads the run and its operation and renders the progress
// as the request asks, with the API's refusal of a cancellation.
func (s *Server) showProgress(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, refused *ui.Refusal) {
	run, tag, ok := s.readRun(w, r, p, m, id)
	if !ok {
		return
	}
	v, op, ok := s.progressView(w, r, p, m, run, tag)
	if !ok {
		return
	}
	if refused != nil {
		v.Refusal = refused
	}
	if v.Running {
		v.Shown = shown(r.Context(), v)
	}
	switch {
	case r.Header.Get("HX-Request") != "true":
		s.render(w, r, status, ui.ProgressPage(s.page(i18n.ProgressPageTitle, ui.PaymentsPath, p, m), v))
	case r.URL.Query().Get("part") == "status":
		s.pollAnswer(w, r, id, op, v)
	default:
		s.render(w, r, status, ui.ProgressSection(v))
	}
}

// pollAnswer answers the status' poll. Once the operation has stopped,
// the run's page is read again to show what it left (HX-Redirect),
// without the notice of what was asked for then. While it runs, a status
// as the poll last showed it is answered 204, which htmx leaves in place,
// so a screen reader does not read the region's same status out again
// every two seconds (T73f2); a status that moved is put in its place.
func (s *Server) pollAnswer(w http.ResponseWriter, r *http.Request, id uuid.UUID, op *gen.Operation, v ui.ProgressView) {
	switch {
	case op == nil || !v.Running:
		w.Header().Set("HX-Redirect", runPath(id))
		s.render(w, r, http.StatusOK, ui.ProgressStatus(v))
	case r.URL.Query().Get(ui.ShownParam) == v.Shown:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		s.render(w, r, http.StatusOK, ui.ProgressPoll(v))
	}
}

// shown is a digest of the status as the person reads it, which the poll
// sends back: equal, nothing they would hear has changed. It is of the
// status rendered in their language, so a change of wording counts and a
// change they cannot see (the operation's version alone) does not.
func shown(ctx context.Context, v ui.ProgressView) string {
	h := sha256.New()
	// A status that does not render answers the page's own render with a
	// 500; its digest then matches no poll's, so the status is sent again.
	_ = ui.ProgressStatus(v).Render(ctx, h)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// progressView reads the run's latest operation (its release's, else its
// proposal's) and is it as the section shows it, with the cancellation
// while it runs and the resumption of a release that stopped. ok is
// false when the answer is already written; a refusal is shown in its
// place, op nil.
func (s *Server) progressView(w http.ResponseWriter, r *http.Request, p *person, m me, run gen.PaymentRun, tag string) (ui.ProgressView, *gen.Operation, bool) {
	v := ui.ProgressView{Run: runPath(run.ID), Self: progressPath(run.ID)}
	opID, ok := run.ReleaseOperationID.Get()
	if !ok {
		opID = run.OperationID.Or(uuid.Nil) // every run has the proposal's
	}
	res, err := s.API.GetOperation(r.Context(), gen.GetOperationParams{ID: opID})
	got, ok := res.(*gen.GetOperationOKHeaders)
	if !ok {
		v.Refusal, ok = s.inPlace(w, r, p, m, i18n.PaymentProblems, "getOperation", res, err, i18n.ProgressRefusedTitle, "progress-refusal")
		return v, nil, ok
	}
	op := got.Response
	// A proposal cancelled leaves its run being proposed until the worker
	// fails it: the section asks again until then, so the page shows the
	// run as it ends.
	v.Kind, v.Status, v.Done = labelOf(operationKinds, op.Kind), labelOf(operationStatuses, op.Status), op.Progress.Done
	v.Running = running(op) || run.Status == "proposing"
	if total, ok := op.Progress.Total.Get(); ok {
		v.Total = &total
	}
	if prob, ok := op.Error.Get(); ok {
		v.Error = explainIn(i18n.PaymentProblems, i18n.ProgressErrorTitle, prob, m)
		v.Error.ID = "progress-error"
	}
	if running(op) && m.may(permCancelOperations) {
		v.Cancel = &ui.CancelForm{Action: runPath(run.ID) + "/cancel", CSRF: p.CSRF, Key: uuid.NewString(), Operation: op.ID.String()}
	}
	v.Resume = resumeForm(run, op, m, p, tag)
	return v, &op, true
}

// resumeForm is the resumption of a release that stopped (failed or
// cancelled), offered as the release itself is: to a releaser who
// neither proposed the run nor approved its release; nil otherwise.
func resumeForm(run gen.PaymentRun, op gen.Operation, m me, p *person, tag string) *ui.RunForm {
	if run.Status != "releasing" || op.Kind != "payments.release" || running(op) || decider(run, m) != "" || approver(run, m) {
		return nil
	}
	return &ui.RunForm{Verb: ui.RunRelease, ID: "resume", Action: runPath(run.ID) + "/" + ui.RunRelease, CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
}

// cancelRunOperation cancels the run's running operation
// (cancelOperation): the one the form names, at its version read now,
// since that moves with its progress; the person asked to cancel this
// operation, whatever it has done since. Done, the run's page says so;
// refused (it has finished meanwhile), the progress is shown with why.
func (s *Server) cancelRunOperation(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	key, opID := r.PostForm.Get(ui.FieldIdempotency), r.PostForm.Get(ui.FieldOperation)
	operation, err := uuid.Parse(opID)
	if _, kerr := uuid.Parse(key); kerr != nil || err != nil {
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody, runPath(id), i18n.ProgressBack))
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	res, err := s.API.GetOperation(r.Context(), gen.GetOperationParams{ID: operation})
	got, ok := res.(*gen.GetOperationOKHeaders)
	if !ok {
		if refused, show := s.inPlace(w, r, p, m, i18n.PaymentProblems, "getOperation", res, err, i18n.CancelRefusedTitle, ""); show {
			s.showProgress(w, r, p, m, id, refusedStatus(res), refused)
		}
		return
	}
	if run, ok := runOf(got.Response); !ok || run != id {
		s.notFound(w, r) // not an operation of this run
		return
	}
	cres, err := s.API.CancelOperation(r.Context(), gen.CancelOperationParams{ID: operation, IfMatch: got.ETag.Or(etag.Format(got.Response.Version)),
		IdempotencyKey: key})
	if _, ok := cres.(*gen.CancelOperationOKHeaders); ok {
		http.Redirect(w, r, runPath(id)+"?done=cancelled", http.StatusSeeOther)
		return
	}
	if refused, show := s.inPlace(w, r, p, m, i18n.PaymentProblems, "cancelOperation", cres, err, i18n.CancelRefusedTitle, ""); show {
		s.showProgress(w, r, p, m, id, refusedStatus(cres), refused)
	}
}

// refusedStatus is the status of an API answer that is a problem.
func refusedStatus(res any) int {
	prob, _ := problemOf(res) // a problem: inPlace said so
	return int(prob.Status)
}
