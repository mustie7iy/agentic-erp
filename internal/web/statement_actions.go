package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The decisions a person takes on a statement's lines (ADR-0047,
// ADR-0052 §7): each a real form with the statement's ETag as If-Match
// and the form's own idempotency key, the line in the form, so a double
// click or a resubmit replays the first answer and a sign-in again
// returns to the statement. Done, the statement's page says so, with a
// link to the line; refused, it is rendered again, fresh from the API, with the
// refusal and what was sent. The API decides every call: an agent's
// proposal is accepted or rejected by a person alone, and whoever matched
// a line by hand may not approve its posting.

// lineDoneOf is the notice of each decision done; a posting that waits
// for its approval says so instead (pending).
var lineDoneOf = map[string]string{ui.LineMatch: "matched", ui.LineExclude: "excluded", ui.LinePost: "posted", ui.LineAccept: "accepted",
	ui.LineReject: "rejected"}

// decideLine takes the decision verb on a statement's line.
func (s *Server) decideLine(verb string) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		ifMatch, key, ok := s.formFields(w, r, statementPath(id), i18n.StatementBack)
		if !ok {
			return
		}
		line, err := strconv.ParseInt(r.PostForm.Get(ui.FieldLine), 10, 32)
		if err != nil || line < 1 {
			s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody,
				statementPath(id), i18n.StatementBack))
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		d := decision{verb: verb, line: int32(line), text: r.PostForm.Get(ui.FieldComment), date: strings.TrimSpace(r.PostForm.Get(ui.FieldDate)),
			choice: r.PostForm.Get(ui.FieldChoice)}
		date, match := d.check()
		if d.textErr != "" || d.dateErr != "" || d.choiceErr != "" {
			s.showStatement(w, r, p, m, id, http.StatusUnprocessableEntity, d, nil)
			return
		}
		res, err := s.sendDecision(r.Context(), d, id, ifMatch, key, date, match)
		if done, ok := res.(*gen.StatementMatchHeaders); ok {
			http.Redirect(w, r, statementPath(id)+"?done="+doneOf(verb, done.Response)+"&line="+strconv.Itoa(int(d.line)), http.StatusSeeOther)
			return
		}
		if prob, show := s.refusal(w, r, p, lineOps[verb].ID, res, err); show {
			s.showStatement(w, r, p, m, id, int(prob.Status), d, explainIn(i18n.BankProblems, i18n.StatementRefusedTitle, prob, m))
		}
	}
}

// check is what is wrong with what was sent, checked before the API is
// called so it stays on the page: a reason where the decision needs one,
// a posting date that is a date and goes with its reason, and a manual
// match's choice. It returns the date and the match to send.
func (d *decision) check() (gen.OptDate, gen.StatementLineMatchInput) {
	d.textErr = reasonError(d.text, ui.LineRequires(d.verb))
	var date gen.OptDate
	var match gen.StatementLineMatchInput
	switch d.verb {
	case ui.LinePost:
		date, d.dateErr = dateParam(d.date)
		switch {
		case d.dateErr != "":
			d.dateErr = i18n.ActionDateInvalid
		case date.Set && strings.TrimSpace(d.text) == "":
			d.textErr = i18n.LinePostReasonNeeded
		case !date.Set && d.text != "":
			d.textErr = i18n.LinePostReasonWithoutDate
		}
	case ui.LineMatch:
		match, d.choiceErr = matchInput(d.choice, d.text)
	}
	return date, match
}

// matchInput is a manual match's body from its choice: the candidate's
// payments, a posting rule, or the incoming payments clearing account.
func matchInput(choice, reason string) (gen.StatementLineMatchInput, i18n.Key) {
	in := gen.StatementLineMatchInput{Reason: reason}
	kind, value, _ := strings.Cut(choice, ":")
	switch {
	case kind == ui.ChoicePayments && value != "":
		for _, v := range strings.Split(value, ",") {
			id, err := uuid.Parse(v)
			if err != nil {
				return in, i18n.LineChoiceNeeded
			}
			in.PaymentIds = append(in.PaymentIds, id)
		}
	case kind == ui.ChoiceRule && value != "":
		in.PostingRule = gen.NewOptString(value)
	case choice == ui.ChoiceIncoming:
		in.Incoming = gen.NewOptBool(true)
	default:
		return in, i18n.LineChoiceNeeded
	}
	return in, ""
}

// sendDecision calls the decision's operation.
func (s *Server) sendDecision(ctx context.Context, d decision, id uuid.UUID, ifMatch, key string, date gen.OptDate, match gen.StatementLineMatchInput) (any, error) {
	switch d.verb {
	case ui.LineMatch:
		return s.API.MatchBankStatementLine(ctx, &match, gen.MatchBankStatementLineParams{ID: id, LineNo: d.line, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.LineExclude:
		return s.API.ExcludeBankStatementLine(ctx, &gen.ReasonInput{Reason: d.text},
			gen.ExcludeBankStatementLineParams{ID: id, LineNo: d.line, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.LinePost:
		var body gen.OptStatementLinePostInput
		if date.Set {
			body = gen.NewOptStatementLinePostInput(gen.StatementLinePostInput{PostingDate: date, Reason: gen.NewOptString(d.text)})
		}
		return s.API.PostBankStatementLine(ctx, body, gen.PostBankStatementLineParams{ID: id, LineNo: d.line, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.LineAccept:
		var body gen.OptStatementLineProposalAccept
		if d.text != "" {
			body = gen.NewOptStatementLineProposalAccept(gen.StatementLineProposalAccept{Comment: gen.NewOptString(d.text)})
		}
		return s.API.AcceptBankStatementLineProposal(ctx, body,
			gen.AcceptBankStatementLineProposalParams{ID: id, LineNo: d.line, IfMatch: ifMatch, IdempotencyKey: key})
	default: // ui.LineReject: the routes are the verbs of lineOps
		return s.API.RejectBankStatementLineProposal(ctx, &gen.ReasonInput{Reason: d.text},
			gen.RejectBankStatementLineProposalParams{ID: id, LineNo: d.line, IfMatch: ifMatch, IdempotencyKey: key})
	}
}

// doneOf is the notice of a decision done: a posting that waits for its
// approval (no journal entry yet) is pending.
func doneOf(verb string, mt gen.StatementMatch) string {
	if verb == ui.LinePost && !mt.JournalEntryID.Set {
		return "pending"
	}
	return lineDoneOf[verb]
}
