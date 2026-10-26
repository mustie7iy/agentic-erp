package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A statement's page reads the statement with its lines, its matches, its
// company's rules, its house bank and the principals' names: with
// getCurrentPrincipal six calls (ADR-0052 §10). Each line shows its live
// match (a rule's, a person's, or the reconciliation agent's proposal,
// whose reason is inert text) and the decisions the API offers a person
// on it in its status (statement_actions.go).

// The line statuses, match kinds, matching rules, match statuses, agents'
// proposals and their outcomes the catalog names; any other value of
// these open enumerations is shown as it is.
var (
	lineStatuses = map[string]i18n.Key{"unmatched": i18n.LineStatusUnmatched, "proposed": i18n.LineStatusProposed,
		"matched": i18n.LineStatusMatched, "excluded": i18n.LineStatusExcluded, "pending_approval": i18n.LineStatusPending,
		"posted": i18n.LineStatusPosted}
	// lineStatusOrder is the order lines' statuses are counted and
	// filtered in: what is left to do first.
	lineStatusOrder = []string{"unmatched", "proposed", "matched", "pending_approval", "posted", "excluded"}
	matchKinds      = map[string]i18n.Key{"rule": i18n.MatchKindRule, "manual": i18n.MatchKindManual, "agent": i18n.MatchKindAgent}
	matchRules      = map[string]i18n.Key{"end_to_end_id": i18n.RuleEndToEnd, "batch_id": i18n.RuleBatch, "transaction_code": i18n.RuleCode,
		"amount": i18n.RuleAmount}
	matchStatuses = map[string]i18n.Key{"open": i18n.MatchStatusOpen, "matched": i18n.MatchStatusMatched, "excluded": i18n.MatchStatusExcluded,
		"rejected": i18n.MatchStatusRejected}
	proposals = map[string]i18n.Key{"match": i18n.ProposalMatch, "exclude": i18n.ProposalExclude}
	outcomes  = map[string]i18n.Key{"accepted": i18n.OutcomeAccepted, "rejected": i18n.OutcomeRejected, "overridden": i18n.OutcomeOverridden}
	lineDone  = map[string]i18n.Key{"matched": i18n.LineDoneMatched, "excluded": i18n.LineDoneExcluded, "accepted": i18n.LineDoneAccepted,
		"rejected": i18n.LineDoneRejected, "posted": i18n.LineDonePosted, "pending": i18n.LineDonePending,
		"imported": i18n.BankDoneImported, "reimported": i18n.BankDoneReimported}
)

// decision is what the person sent in a line's form that the API
// refused or that was not valid: the decision's verb and line, its text,
// date and choice, and their errors; the page renders that form with it.
type decision struct {
	verb                        string
	line                        int32
	text, date, choice          string
	textErr, dateErr, choiceErr i18n.Key
}

// statement is a statement's page.
func (s *Server) statement(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showStatement(w, r, p, m, id, http.StatusOK, decision{}, nil)
}

// showStatement reads the statement and renders its page with status:
// the forms fresh (the statement's current ETag, new idempotency keys)
// holding what the person sent, and the API's refusal of it.
func (s *Server) showStatement(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, d decision, refused *ui.Refusal) {
	res, err := s.API.GetBankStatement(r.Context(), gen.GetBankStatementParams{ID: id})
	got, ok := res.(*gen.BankStatementHeaders)
	if !ok {
		s.statementRefused(w, r, p, m, "getBankStatement", res, err)
		return
	}
	mres, err := s.API.ListBankStatementMatches(r.Context(), gen.ListBankStatementMatchesParams{ID: id})
	matches, ok := mres.(*gen.StatementMatchList)
	if !ok {
		s.statementRefused(w, r, p, m, "listBankStatementMatches", mres, err)
		return
	}
	st := got.Response
	rules := s.statementRules(r.Context(), id)
	banks := s.houseBanks(r.Context(), m, st.LegalEntityID)
	names := s.names(r.Context(), p, statementPrincipals(st, matches.Items))
	sp := ui.StatementPage{Header: statementHeader(st, banks[st.HouseBankID].Code, names), Gap: st.Status == "gap",
		Report: reportPath(st.HouseBankID), Done: lineDone[r.URL.Query().Get("done")], Refusal: refused}
	if n, err := strconv.ParseInt(r.URL.Query().Get("line"), 10, 32); err == nil && sp.Done != "" {
		sp.DoneLine = int32(n)
	}
	c := lineContext{m: m, p: p, base: statementPath(id), tag: got.ETag.Or(etag.Format(st.Version)), rules: rules, names: names, sent: d,
		open: st.Status != "reconciled", gap: st.Status == "gap"}
	sp.Lines = lineViews(st.Lines, matches.Items, c)
	sp.Why = linesWhy(st, sp.Lines, m)
	s.render(w, r, status, ui.Statement(s.page(i18n.StatementTitle, ui.BankPath, p, m), sp))
}

// statementRefused answers a statement the API would not show: not found,
// or its refusal on a page of its own.
func (s *Server) statementRefused(w http.ResponseWriter, r *http.Request, p *person, m me, operation string, res any, err error) {
	prob, show := s.refusal(w, r, p, operation, res, err)
	switch {
	case !show:
	case prob.Status == http.StatusNotFound:
		s.notFound(w, r)
	default:
		s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(i18n.StatementTitle, ui.BankPath, p, m), i18n.BankRefusedTitle, ui.BankPath,
			i18n.StatementBack, *explainIn(i18n.BankProblems, i18n.BankRefusedTitle, prob, m)))
	}
}

// statementRules are the posting rules of the statement's company
// (getBankStatementRules), by transaction code; none when they could not
// be read, logged: a person then matches to payments or the incoming
// account alone.
func (s *Server) statementRules(ctx context.Context, id uuid.UUID) []gen.BankStatementRulesPostingRulesItem {
	res, err := s.API.GetBankStatementRules(ctx, gen.GetBankStatementRulesParams{ID: id})
	rules, ok := res.(*gen.BankStatementRules)
	if !ok {
		s.Log.WarnContext(ctx, "web: a statement's rules could not be read", "answer", res, "error", err)
		return nil
	}
	return rules.PostingRules
}

// statementPrincipals are the principals a statement's page names: who
// imported and reconciled it, and who made each match.
func statementPrincipals(st gen.BankStatement, matches []gen.StatementMatch) []uuid.UUID {
	ids := []uuid.UUID{st.CreatedBy}
	if by, ok := st.ReconciledBy.Get(); ok {
		ids = append(ids, by)
	}
	for _, mt := range matches {
		ids = append(ids, mt.CreatedBy)
	}
	return ids
}

// statementHeader is a statement's summary.
func statementHeader(st gen.BankStatement, bank string, names map[uuid.UUID]ui.Name) ui.StatementHeader {
	h := ui.StatementHeader{Ref: st.StatementRef, Sequence: st.Sequence, Format: st.Format, Bank: bank, Account: ui.Masked(st.Account),
		OpeningDay: st.OpeningDate, ClosingDay: st.ClosingDate, Opening: moneyOf(st.OpeningBalance), Closing: moneyOf(st.ClosingBalance),
		Lines: st.LineCount, Status: labelOf(statementStatuses, st.Status)}
	if prev, ok := st.PreviousStatementID.Get(); ok {
		h.Previous = statementPath(prev)
	}
	if at, ok := st.ReconciledAt.Get(); ok {
		h.ReconciledAt = &at
	}
	if by, ok := st.ReconciledBy.Get(); ok {
		n := names[by]
		h.ReconciledBy = &n
	}
	return h
}

// lineContext is what a statement's lines are shown with: the person, the
// statement's path and ETag, its company's posting rules, the names, what
// the person sent, and whether the statement is still open to decisions.
type lineContext struct {
	m     me
	p     *person
	base  string
	tag   string
	rules []gen.BankStatementRulesPostingRulesItem
	names map[uuid.UUID]ui.Name
	sent  decision
	open  bool
	gap   bool // the statement is in gap: nothing posts for the bank
}

// lineViews are a statement's lines, each with its live match, the
// outcome of the agent's latest proposal a person decided, and the
// person's decisions.
func lineViews(lines []gen.BankStatementLine, matches []gen.StatementMatch, c lineContext) []ui.LineView {
	out := make([]ui.LineView, 0, len(lines))
	for _, l := range lines {
		v := ui.LineView{No: l.LineNo, Anchor: lineAnchor(l.LineNo), Amount: moneyOf(l.Amount), Reversal: l.IsReversal, BookingDay: l.BookingDate,
			ValueDay: l.ValueDate, Code: l.TransactionCode, BankRef: l.BankReference, OwnerRef: l.OwnerReference, Text: l.Text,
			Status: labelOf(lineStatuses, l.Status)}
		live, outcome := lineMatches(matches, l.LineNo)
		if live != nil {
			v.Match = matchInfo(*live, c)
		}
		if outcome != "" {
			lbl := labelOf(outcomes, outcome)
			v.LastOutcome = &lbl
		}
		if c.open {
			lineForms(&v, l, live, c)
		}
		out = append(out, v)
	}
	return out
}

// lineAnchor is a line's element id, the fragment a decision returns to.
func lineAnchor(no int32) string { return "line-" + strconv.Itoa(int(no)) }

// lineMatches are a line's live match (the latest not replaced; nil for
// none) and the outcome of the agent's latest proposal a person decided
// ("" for none), from the statement's matches in the order made.
func lineMatches(matches []gen.StatementMatch, line int32) (*gen.StatementMatch, string) {
	var live *gen.StatementMatch
	outcome := ""
	for i := range matches {
		mt := &matches[i]
		if mt.LineNo != line {
			continue
		}
		if mt.Status != "rejected" {
			live = mt
		}
		if o, ok := mt.Outcome.Get(); ok {
			outcome = o
		}
	}
	return live, outcome
}

// matchInfo is a match as the page shows it.
func matchInfo(mt gen.StatementMatch, c lineContext) *ui.MatchInfo {
	info := &ui.MatchInfo{Kind: labelOf(matchKinds, mt.Kind), By: c.names[mt.CreatedBy], Agent: mt.Kind == "agent",
		Proposal: labelOf(proposals, mt.Proposal.Or("")), Status: labelOf(matchStatuses, mt.Status), Reason: mt.Reason,
		Open: mt.Status == "open" && mt.Kind != "agent", PostingReason: mt.PostingReason.Or("")}
	if rule, ok := mt.Rule.Get(); ok {
		lbl := labelOf(matchRules, rule)
		info.Rule = &lbl
	}
	for _, t := range mt.Targets {
		info.Targets = append(info.Targets, ui.Target{Candidate: t.Candidate, Payment: idText(t.PaymentID), BankFile: idText(t.BankFileID),
			Rule: t.PostingRule.Or(""), RuleText: ruleText(c.rules, t.PostingRule.Or("")), Incoming: t.Incoming.Or(false)})
	}
	if d, ok := mt.PostingDate.Get(); ok {
		info.PostingDay = &d
	}
	if req, ok := mt.ApprovalRequestID.Get(); ok && !mt.JournalEntryID.Set {
		info.Approval = ui.ApprovalsPath + "/" + req.String()
	}
	if je, ok := mt.JournalEntryID.Get(); ok {
		info.Entry = fmt.Sprintf("%s / %d", je, mt.JournalEntryYear.Or(0))
	}
	return info
}

// idText is an optional id's text, "" when absent.
func idText(id gen.OptUUID) string {
	if v, ok := id.Get(); ok {
		return v.String()
	}
	return ""
}

// ruleText is what the company's posting rule of the code posts, "" when
// its rules do not name it.
func ruleText(rules []gen.BankStatementRulesPostingRulesItem, code string) string {
	for _, r := range rules {
		if r.TransactionCode == code && code != "" {
			return r.Description
		}
	}
	return ""
}

// lineForms are the decisions the API offers a person on the line in its
// status (ADR-0047), each by the operation's x-permission (advisory,
// ADR-0052 §7): a line not matched is matched by hand or excluded, an
// agent's open proposal accepted or rejected too, and a matched line
// posted (or matched again), unless the statement is in gap, which the
// API refuses to post (the page's gap notice says why); an excluded line
// may be matched by hand after all. A line on its way to the ledger or
// posted takes none.
func lineForms(v *ui.LineView, l gen.BankStatementLine, live *gen.StatementMatch, c lineContext) {
	switch l.Status {
	case "unmatched", "proposed", "matched", "excluded":
	default:
		return
	}
	if c.m.may(permMatchLines) {
		if live != nil && live.Kind == "agent" && live.Status == "open" {
			v.Accept, v.Reject = c.form(ui.LineAccept, l.LineNo), c.form(ui.LineReject, l.LineNo)
		}
		if choices := matchChoices(l, live, c.rules); len(choices) > 0 {
			f := &ui.MatchForm{LineForm: *c.form(ui.LineMatch, l.LineNo), Choices: choices}
			if c.sent.verb == ui.LineMatch && c.sent.line == l.LineNo {
				f.Choice, f.ChoiceError = c.sent.choice, c.sent.choiceErr
			}
			v.Manual = f
		}
		if l.Status != "excluded" {
			v.Exclude = c.form(ui.LineExclude, l.LineNo)
		}
	}
	if l.Status == "matched" && !c.gap && c.m.may(permPostLines) {
		v.Post = c.form(ui.LinePost, l.LineNo)
	}
}

// form is one decision's form on the line, holding what the person sent
// in it.
func (c lineContext) form(verb string, line int32) *ui.LineForm {
	f := &ui.LineForm{Verb: verb, Line: line, ID: lineAnchor(line) + "-" + verb, Action: c.base + "/" + verb, CSRF: c.p.CSRF, ETag: c.tag,
		Key: uuid.NewString()}
	if c.sent.verb == verb && c.sent.line == line {
		f.Text, f.Date, f.TextError, f.DateError, f.Open = c.sent.text, c.sent.date, c.sent.textErr, c.sent.dateErr, true
	}
	return f
}

// matchChoices are what a person may match the line to by hand: the
// candidates a rule left open (each its payments), the company's posting
// rules, and, for a credit that reverses nothing, the incoming payments
// clearing account. The API checks each again (§7).
func matchChoices(l gen.BankStatementLine, live *gen.StatementMatch, rules []gen.BankStatementRulesPostingRulesItem) []ui.Choice {
	var out []ui.Choice
	if live != nil && live.Status == "open" && live.Kind != "agent" {
		out = candidates(live.Targets)
	}
	for _, r := range rules {
		out = append(out, ui.Choice{Value: ui.ChoiceRule + ":" + r.TransactionCode, Kind: ui.ChoiceRule, Rule: r.TransactionCode, RuleText: r.Description})
	}
	if !l.IsReversal && !strings.HasPrefix(l.Amount.Amount, "-") && !ui.Zero(l.Amount.Amount) {
		out = append(out, ui.Choice{Value: ui.ChoiceIncoming, Kind: ui.ChoiceIncoming})
	}
	return out
}

// candidates are an open match's candidates, each its payments, in their
// numbers' order.
func candidates(targets []gen.StatementMatchTarget) []ui.Choice {
	var out []ui.Choice
	for _, t := range targets {
		pay, ok := t.PaymentID.Get()
		if !ok {
			continue
		}
		if n := len(out); n == 0 || out[n-1].Number != t.Candidate {
			out = append(out, ui.Choice{Kind: ui.ChoicePayments, Number: t.Candidate})
		}
		c := &out[len(out)-1]
		c.Payments = append(c.Payments, pay.String())
		c.Value = ui.ChoicePayments + ":" + strings.Join(c.Payments, ",")
	}
	return out
}

// linesWhy is why the statement's lines offer the person no decision: it
// is reconciled, or they may decide none; "" when they may.
func linesWhy(st gen.BankStatement, lines []ui.LineView, m me) i18n.Key {
	switch {
	case st.Status == "reconciled":
		return i18n.StatementReconciledWhy
	case m.may(permMatchLines) || m.may(permPostLines):
		return ""
	}
	for _, l := range lines {
		switch l.Status.Raw {
		case "unmatched", "proposed", "matched", "excluded":
			return i18n.LinesNotYours
		}
	}
	return ""
}
