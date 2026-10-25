package web

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The decisions a person takes on a payment run (ADR-0038, ADR-0052 §7):
// its release (an operation; again on a release that stopped, which
// resumes it), its rejection with a reason, the request for a second
// person's approval of its release above the company's limit (T72m:
// asked for once the release answers payments.release_approval_required,
// decided in the approvals inbox) and the transmission of a bank file
// (an operation). Each is a real form with the run's ETag (the bank
// file's for a transmission) as If-Match and the form's own idempotency
// key, so a double click or a resubmit replays the first answer. Done,
// the run's page says so; refused, it is rendered again, fresh from the
// API, with the refusal and what was typed.

// runDoneOf is the notice of each decision done.
var runDoneOf = map[string]string{ui.RunRelease: "releasing", ui.RunReject: "rejected", ui.RunSubmit: "submitted", ui.RunTransmit: "transmitting"}

// decideRun takes the decision verb on the run.
func (s *Server) decideRun(verb string) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		ifMatch, key, ok := s.formFields(w, r, runPath(id), i18n.RunBack)
		if !ok {
			return
		}
		file, err := uuid.Parse(r.PostForm.Get(ui.FieldBankFile))
		if verb == ui.RunTransmit && err != nil {
			s.render(w, r, http.StatusBadRequest, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.ProblemInvoiceBody, runPath(id), i18n.RunBack))
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		t := runTyped{verb: verb, text: r.PostForm.Get(ui.FieldComment)}
		if t.textErr = reasonError(t.text, ui.RunRequires(verb)); t.textErr != "" {
			s.showRun(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
			return
		}
		res, err := s.sendRun(r.Context(), verb, id, file, ifMatch, key, t.text)
		switch res.(type) {
		case *gen.OperationHeaders, *gen.RejectPaymentRunOKHeaders:
			http.Redirect(w, r, runPath(id)+"?done="+runDoneOf[verb], http.StatusSeeOther)
			return
		}
		if prob, show := s.refusal(w, r, p, runOps[verb].ID, res, err); show {
			t.askApproval = prob.Code == "payments.release_approval_required"
			s.showRun(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.PaymentProblems, i18n.RunRefusedTitle, prob, m))
		}
	}
}

// sendRun calls the verb's operation: a reason where it needs one, a
// comment where it takes one.
func (s *Server) sendRun(ctx context.Context, verb string, id, file uuid.UUID, ifMatch, key, text string) (any, error) {
	switch verb {
	case ui.RunRelease:
		var body gen.OptDecisionInput
		if text != "" {
			body = gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString(text)})
		}
		return s.API.ReleasePaymentRun(ctx, body, gen.ReleasePaymentRunParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.RunReject:
		return s.API.RejectPaymentRun(ctx, &gen.ReasonInput{Reason: text}, gen.RejectPaymentRunParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.RunSubmit:
		return s.API.SubmitPaymentRun(ctx, gen.SubmitPaymentRunParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	default: // ui.RunTransmit: the routes are the verbs of runOps
		return s.API.TransmitBankFile(ctx, gen.TransmitBankFileParams{ID: file, IfMatch: ifMatch, IdempotencyKey: key})
	}
}
