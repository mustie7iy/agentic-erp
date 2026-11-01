package web

import (
	"bufio"
	"cmp"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Beyond the line's decisions (T73g2, ADR-0047): a line's search for a
// payment the rules did not find, the company's matching rules run again
// on the statement, and the bank's file it was imported from. The search
// is a page of its own, three calls (getCurrentPrincipal,
// getBankStatement, listBankStatementPayments), whose filter htmx sends
// for the filter and results alone; the payment chosen is sent as the line's match
// by hand (matchBankStatementLine, statement_actions.go), with the
// statement's ETag and the form's idempotency key, so the API checks it
// again and a refusal comes back on the statement's page with the choice
// kept.

// rulesVerb is the route of the rules' run on a statement; "match" is the
// line's match by hand.
const rulesVerb = "rules"

// searchPageSize is how many payments the search shows at a time.
const searchPageSize = 25

// linePaymentsPath is a line's search, under its statement's path.
func linePaymentsPath(statement string, line int32) string {
	return statement + "/lines/" + strconv.Itoa(int(line)) + "/payments"
}

// linePayments is a line's search for the payments it may be matched to.
func (s *Server) linePayments(w http.ResponseWriter, r *http.Request, p *person) {
	w.Header().Add("Vary", "HX-Request") // a page, or its results alone
	id, err := uuid.Parse(r.PathValue("id"))
	line, lerr := strconv.ParseInt(r.PathValue("line"), 10, 32)
	if err != nil || lerr != nil || line < 1 {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	res, err := s.API.GetBankStatement(r.Context(), gen.GetBankStatementParams{ID: id})
	got, ok := res.(*gen.BankStatementHeaders)
	if !ok {
		s.statementRefused(w, r, p, m, "getBankStatement", res, err)
		return
	}
	st := got.Response
	l, found := lineOf(st.Lines, int32(line))
	if !found {
		s.notFound(w, r)
		return
	}
	path := linePaymentsPath(statementPath(id), l.LineNo)
	q := r.URL.Query()
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	params, f := searchParams(loc, q, id, decimals(l.Amount.Amount))
	f.Action, f.Currency = path, l.Amount.Currency
	v := ui.PaymentSearch{Back: statementPath(id), Ref: st.StatementRef, Filter: f,
		Line: ui.LineView{No: l.LineNo, Amount: moneyOf(l.Amount), Reversal: l.IsReversal, BookingDay: l.BookingDate, ValueDay: l.ValueDate,
			Code: l.TransactionCode, BankRef: l.BankReference, OwnerRef: l.OwnerReference, Text: l.Text, Status: labelOf(lineStatuses, l.Status)}}
	status := http.StatusOK
	if v.Results.Invalid = cmp.Or(f.MinError, f.MaxError, f.FromError, f.ToError); v.Results.Invalid != "" {
		status = http.StatusBadRequest
	} else if !s.searchPayments(w, r, p, m, params, &v.Results, &status) {
		return
	}
	if len(v.Results.Found) > 0 {
		if v.Results.Why = searchWhy(st, l); v.Results.Why == "" {
			v.Results.Match = &ui.MatchForm{LineForm: ui.LineForm{Verb: ui.LineMatch, Line: l.LineNo, ID: "search-match",
				Action: statementPath(id) + "/" + ui.LineMatch, CSRF: p.CSRF, ETag: got.ETag.Or(etag.Format(st.Version)), Key: uuid.NewString(), Open: true},
				Choices: v.Results.Found}
		}
	}
	if v.Results.More != "" {
		v.Results.More = path + "?" + with(q, "page", v.Results.More).Encode()
	}
	if r.Header.Get("HX-Request") == "true" {
		// htmx swaps a 2xx answer alone: a filter refused, or a search the
		// API refused, is said in what it replaces.
		s.render(w, r, http.StatusOK, ui.PaymentSearchBody(v))
		return
	}
	s.render(w, r, status, ui.LinePayments(s.page(i18n.SearchTitle, ui.BankPath, p, m), v))
}

// searchPayments reads a page of the search into res, the next page's
// token in More; a refusal is shown in the results' place. ok is false
// when the answer is already written.
func (s *Server) searchPayments(w http.ResponseWriter, r *http.Request, p *person, m me, params gen.ListBankStatementPaymentsParams,
	res *ui.PaymentResults, status *int) bool {
	answer, err := s.API.ListBankStatementPayments(r.Context(), params)
	page, ok := answer.(*gen.PaymentPage)
	if !ok {
		refused, shown := s.inPlace(w, r, p, m, i18n.BankProblems, "listBankStatementPayments", answer, err, i18n.StatementRefusedTitle, "search-refusal")
		if shown {
			res.Refusal = refused
			*status = refusalStatus(answer)
		}
		return shown
	}
	for _, pay := range page.Items {
		res.Found = append(res.Found, ui.Choice{Value: ui.ChoicePayments + ":" + pay.ID.String(), Kind: ui.ChoiceFound, Payments: []string{pay.ID.String()},
			Found: &ui.FoundPayment{EndToEnd: pay.EndToEndID, Supplier: pay.SupplierName, Account: pay.BankAccountLast4, Amount: moneyOf(pay.Amount),
				Entry: pay.JournalEntryNumber.Or("")}})
	}
	res.More = page.NextPageToken.Or("")
	return true
}

// refusalStatus is the status of the API's refusal the page shows.
func refusalStatus(answer any) int {
	prob, _ := problemOf(answer) // inPlace showed it: a problem
	return int(prob.Status)
}

// searchWhy is why the line takes no match by hand now, "" when it does:
// as lineForms offers it, by the line's status while the statement is not
// reconciled.
func searchWhy(st gen.BankStatement, l gen.BankStatementLine) i18n.Key {
	switch l.Status {
	case "unmatched", "proposed", "matched", "excluded":
		if st.Status != "reconciled" {
			return ""
		}
	}
	return i18n.SearchNotOpen
}

// lineOf is the statement's line numbered no.
func lineOf(lines []gen.BankStatementLine, no int32) (gen.BankStatementLine, bool) {
	for _, l := range lines {
		if l.LineNo == no {
			return l, true
		}
	}
	return gen.BankStatementLine{}, false
}

// decimals are the minor-unit digits of an API amount: the statement's
// currency's, as every amount of it carries them (ADR-0005).
func decimals(amount string) int {
	_, frac, _ := strings.Cut(amount, ".")
	return len(frac)
}

// searchParams are the search's API parameters from the query and the
// filter as typed with its errors: the amounts read in the person's
// locale (no sign, at most the currency's digits, padded to them), the
// dates as dates.
func searchParams(loc i18n.Locale, q url.Values, id uuid.UUID, digits int) (gen.ListBankStatementPaymentsParams, ui.PaymentFilter) {
	f := ui.PaymentFilter{Min: q.Get(ui.FieldAmountMin), Max: q.Get(ui.FieldAmountMax), From: q.Get(ui.FieldRunFrom), To: q.Get(ui.FieldRunTo)}
	params := gen.ListBankStatementPaymentsParams{ID: id, PageSize: gen.NewOptInt32(searchPageSize)}
	params.AmountMin, f.MinError = amountBound(loc, f.Min, digits)
	params.AmountMax, f.MaxError = amountBound(loc, f.Max, digits)
	params.RunDateFrom, f.FromError = dateParam(f.From)
	params.RunDateTo, f.ToError = dateParam(f.To)
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params, f
}

// amountBound is an amount the person typed as the API's bound, with
// exactly the currency's digits; unset when empty, an error when it is
// no amount, has a sign or more digits than the currency.
func amountBound(loc i18n.Locale, typed string, digits int) (gen.OptString, i18n.Key) {
	if strings.TrimSpace(typed) == "" {
		return gen.OptString{}, ""
	}
	amount, ok := loc.Amount(typed)
	whole, frac, _ := strings.Cut(amount, ".")
	if !ok || strings.HasPrefix(amount, "-") || len(frac) > digits {
		return gen.OptString{}, i18n.SearchAmountInvalid
	}
	if digits == 0 {
		return gen.NewOptString(whole), ""
	}
	return gen.NewOptString(whole + "." + frac + strings.Repeat("0", digits-len(frac))), ""
}

// rerunRules runs the company's matching rules on the statement again
// (matchBankStatement): done, its page says so; refused, it is rendered
// again with the refusal.
func (s *Server) rerunRules(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, statementPath(id), i18n.StatementBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	res, err := s.API.MatchBankStatement(r.Context(), gen.MatchBankStatementParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	// ogen names the response matchBankStatement shares after acceptBankStatementGap.
	if _, done := res.(*gen.AcceptBankStatementGapOKHeaders); done {
		http.Redirect(w, r, statementPath(id)+"?done="+rulesVerb, http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "matchBankStatement", res, err); show {
		s.showStatement(w, r, p, m, id, int(prob.Status), decision{}, explainIn(i18n.BankProblems, i18n.StatementRefusedTitle, prob, m))
	}
}

// statementFile relays the bank's file a statement was imported from
// (getBankStatementFile): untrusted bytes, always a download (ADR-0052
// §7: inline only for PDFs and images, and a statement's file is XML or
// MT940 text), never cached, under a sandbox. The API answers
// octet-stream; the name says XML when the bytes are, text otherwise.
func (s *Server) statementFile(w http.ResponseWriter, r *http.Request, p *person) {
	h := w.Header()
	h.Set("Content-Security-Policy", documentCSP)
	h.Set("Cache-Control", "no-store")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	res, err := s.API.GetBankStatementFile(r.Context(), gen.GetBankStatementFileParams{ID: id})
	file, ok := res.(*gen.GetBankStatementFileOK)
	if !ok {
		prob, show := s.refusal(w, r, p, "getBankStatementFile", res, err)
		switch {
		case !show:
		case prob.Status == http.StatusNotFound:
			s.notFound(w, r)
		default:
			s.render(w, r, int(prob.Status), ui.RefusalPage(s.shell(i18n.StatementTitle, ""), i18n.BankRefusedTitle, statementPath(id),
				i18n.StatementBack, *explainIn(i18n.BankProblems, i18n.BankRefusedTitle, prob, me{})))
		}
		return
	}
	body := bufio.NewReaderSize(file.Data, 512)
	head, _ := body.Peek(512) // fewer bytes for a shorter file; the client holds it in memory, so reading cannot fail
	ext := ".txt"
	if strings.Contains(http.DetectContentType(head), "xml") {
		ext = ".xml"
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="statement-`+id.String()+ext+`"`)
	_, _ = io.Copy(w, body) // the headers are sent: a failed write is the browser gone, with no one left to tell
}
