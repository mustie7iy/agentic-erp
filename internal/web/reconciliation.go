package web

import (
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A house bank's reconciliation report (ADR-0047, T49h): per statement
// closing in the period, the bank's closing balance against the ledger's
// balance of the bank's main account on that date, what explains the
// difference (lines excluded, in transit, posted later) and what is left
// unexplained; with getCurrentPrincipal and listHouseBanks three calls.

// reconPageSize is how many statements a report's page shows, each with
// its lines.
const reconPageSize = 10

// reconciliation is a house bank's reconciliation report.
func (s *Server) reconciliation(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	rp := ui.ReconciliationPage{Bank: ui.HouseBank{Code: id.String()}}
	if b, ok := s.houseBanks(r.Context(), m, uuid.Nil)[id]; ok {
		rp.Bank = bankView(b, nil)
	}
	params := reconParams(id, q, &rp.Filters)
	status := http.StatusOK
	switch {
	case rp.Filters.FromError != "" || rp.Filters.ToError != "":
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.GetBankReconciliation(r.Context(), params)
		rec, ok := res.(*gen.BankReconciliationHeaders)
		if !ok {
			prob, show := s.refusal(w, r, p, "getBankReconciliation", res, err)
			if !show {
				return
			}
			if prob.Status == http.StatusNotFound {
				s.notFound(w, r)
				return
			}
			rp.Refusal, status = explainIn(i18n.BankProblems, i18n.ReconRefusedTitle, prob, m), int(prob.Status)
			break
		}
		fillReport(&rp, rec.Response, id, q)
	}
	s.render(w, r, status, ui.Reconciliation(s.page(i18n.ReconTitle, ui.BankPath, p, m), rp))
}

// reconParams are the API's parameters the query asks for, the filters
// as the page shows them; a date that is not one is the filter's error.
func reconParams(id uuid.UUID, q url.Values, f *ui.ReconFilters) gen.GetBankReconciliationParams {
	params := gen.GetBankReconciliationParams{ID: id, PageSize: gen.NewOptInt32(reconPageSize)}
	*f = ui.ReconFilters{Path: reportPath(id), From: q.Get("closing_date_from"), To: q.Get("closing_date_to"), LineStatus: q.Get("line_status")}
	for _, st := range lineStatusOrder {
		f.Statuses = append(f.Statuses, ui.KeyOption{Value: st, Label: lineStatuses[st], Selected: st == f.LineStatus})
	}
	params.ClosingDateFrom, f.FromError = dateParam(f.From)
	params.ClosingDateTo, f.ToError = dateParam(f.To)
	if f.LineStatus != "" {
		params.LineStatus = gen.NewOptString(f.LineStatus)
	}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params
}

// fillReport fills the report's page from the API's.
func fillReport(rp *ui.ReconciliationPage, rec gen.BankReconciliation, id uuid.UUID, q url.Values) {
	sm := &ui.ReconSummary{Statements: rec.Summary.Statements, Counts: countsOf(rec.Summary.Lines)}
	if o, ok := rec.Summary.OpeningBalance.Get(); ok {
		m := moneyOf(o)
		sm.Opening = &m
	}
	if d, ok := rec.Summary.OpeningDate.Get(); ok {
		sm.OpeningDay = &d
	}
	if f, ok := rec.Summary.Figures.Get(); ok {
		fig := figuresOf(f)
		sm.Figures = &fig
	}
	rp.Summary = sm
	for _, st := range rec.Items {
		rs := ui.ReconStatement{Path: statementPath(st.BankStatementID), Ref: st.StatementRef, Sequence: st.Sequence,
			Status: labelOf(statementStatuses, st.Status), OpeningDay: st.OpeningDate, Opening: moneyOf(st.OpeningBalance),
			Figures: figuresOf(st.Figures), Counts: countsOf(st.LineCounts), Anchor: "statement-" + st.BankStatementID.String()}
		for _, l := range st.Lines {
			rs.Lines = append(rs.Lines, reconLine(l))
		}
		rp.Statements = append(rp.Statements, rs)
	}
	if next := rec.NextPageToken.Or(""); next != "" {
		rp.More = reportPath(id) + "?" + with(q, "page", next).Encode()
	}
}

// figuresOf are a statement's figures as the page shows them.
func figuresOf(f gen.BankReconciliationFigures) ui.Figures {
	out := ui.Figures{AsOf: f.AsOf, Closing: moneyOf(f.ClosingBalance), Excluded: moneyOf(f.Excluded), InTransit: moneyOf(f.InTransit),
		PostedLater: moneyOf(f.PostedLater)}
	if l, ok := f.LedgerBalance.Get(); ok {
		m := moneyOf(l)
		out.Ledger = &m
	}
	if u, ok := f.Unexplained.Get(); ok {
		m := moneyOf(u)
		out.Unexplained = &m
	}
	return out
}

// countsOf are the lines' counts by status, what is left to do first.
func countsOf(c gen.BankReconciliationLineCounts) []ui.Count {
	n := map[string]int32{"unmatched": c.Unmatched, "proposed": c.Proposed, "matched": c.Matched, "pending_approval": c.PendingApproval,
		"posted": c.Posted, "excluded": c.Excluded}
	out := make([]ui.Count, 0, len(lineStatusOrder))
	for _, st := range lineStatusOrder {
		out = append(out, ui.Count{Status: labelOf(lineStatuses, st), N: n[st]})
	}
	return out
}

// reconLine is a line of the report.
func reconLine(l gen.BankReconciliationLine) ui.ReconLine {
	out := ui.ReconLine{No: l.LineNo, BookingDay: l.BookingDate, Amount: moneyOf(l.Amount), Reversal: l.IsReversal, Code: l.TransactionCode,
		Status: labelOf(lineStatuses, l.Status)}
	if k, ok := l.MatchKind.Get(); ok {
		lbl := labelOf(matchKinds, k)
		out.Match = &lbl
	}
	if rule, ok := l.MatchRule.Get(); ok {
		lbl := labelOf(matchRules, rule)
		out.Rule = &lbl
	}
	if d, ok := l.PostingDate.Get(); ok && l.JournalEntryID.Set {
		out.PostingDay = &d
	}
	if req, ok := l.ApprovalRequestID.Get(); ok && !l.JournalEntryID.Set {
		out.Approval = ui.ApprovalsPath + "/" + req.String()
	}
	if o, ok := l.ProposalOutcome.Get(); ok {
		lbl := labelOf(outcomes, o)
		out.Outcome = &lbl
	}
	return out
}
