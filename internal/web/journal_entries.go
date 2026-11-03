package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The journal of posted entries (ADR-0052 §2): a company's entries in a
// ledger, filtered by the API's own filters, those of the centers among
// them (T56d2), and an entry's page with its lines and its reversal,
// which the API refuses for an entry another context owns (a supplier
// invoice's, a goods receipt's) and explains by its code. The list
// makes, with getCurrentPrincipal, listLegalEntities, listLedgers and
// the company's cost and profit centers, six calls; an entry's page
// reads the entry with the partners its lines name (expand=partners,
// T73h3: no call of their own), the companies, the ledgers, the company's
// accounts and who posted it: six too.

// entryPageSize is how many entries a journal page shows.
const entryPageSize = 25

// maxCenters is the most centers a filter offers, the API's largest
// page.
const maxCenters = 500

// The source contexts the catalog names and the notices of what was just
// done to an entry.
var (
	sourceContexts = map[string]i18n.Key{"ledger": i18n.SourceLedger, "payables": i18n.SourcePayables, "payments": i18n.SourcePayments,
		"procurement": i18n.SourceProcurement, "receivables": i18n.SourceReceivables, "assets": i18n.SourceAssets, "integration": i18n.SourceIntegration}
	entryDone = map[string]i18n.Key{"reversed": i18n.EntryDoneReversed, "posted": i18n.EntryDonePosted}
)

// journalEntries is the journal: the entries of the company, ledger and
// filters the query names, else of the first company and the leading
// ledger.
func (s *Server) journalEntries(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	companies, ledgers := s.companies(ctx, m), s.ledgers(ctx, m)
	f := ui.EntryFilters{Companies: companyOptions(companies, q.Get(ui.FieldCompany)), Ledgers: ledgerOptions(ledgers, q.Get(ui.FieldLedger)),
		Number: q.Get(ui.FieldDocumentNumber), From: q.Get(ui.FieldPostingDateFrom), To: q.Get(ui.FieldPostingDateTo)}
	company, ledger := askedOrFirst(q.Get(ui.FieldCompany), f.Companies), askedOrFirst(q.Get(ui.FieldLedger), f.Ledgers)
	l := ui.EntryList{}
	params, valid := entryParams(q, company, ledger, &f)
	if id, err := uuid.Parse(company); err == nil {
		f.CostCenters, f.ProfitCenters = s.centerOptions(ctx, m, id, q.Get(ui.FieldCostCenter), q.Get(ui.FieldProfitCenter))
	}
	l.Filters = f
	status := http.StatusOK
	switch {
	case !valid:
		l.Invalid, status = true, http.StatusBadRequest
	case f.FromError != "" || f.ToError != "":
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.ListJournalEntries(ctx, params)
		page, isPage := res.(*gen.JournalEntryPage)
		if !isPage {
			prob, show := s.refusal(w, r, p, "listJournalEntries", res, err)
			if !show {
				return
			}
			l.Refusal, status = explainIn(i18n.EntryProblems, i18n.EntriesRefusedTitle, prob, m), int(prob.Status)
			break
		}
		l.Rows = entryRows(page.Items)
		if next := page.NextPageToken.Or(""); next != "" {
			l.More = ui.EntriesFor(with(q, "page", next))
		}
	}
	s.render(w, r, status, ui.Entries(s.page(i18n.EntriesTitle, ui.JournalPath, p, m), l))
}

// entryParams are the API's filters of the journal, and whether the
// company, the ledger and the centers are ids; a date that is none is its
// filter's error.
func entryParams(q url.Values, company, ledger string, f *ui.EntryFilters) (gen.ListJournalEntriesParams, bool) {
	params := gen.ListJournalEntriesParams{PageSize: gen.NewOptInt32(entryPageSize), OrderBy: gen.NewOptString("posting_date desc")}
	valid := true
	for _, v := range []struct {
		value string
		set   *gen.OptUUID
	}{{company, &params.LegalEntityID}, {ledger, &params.LedgerID}, {q.Get(ui.FieldCostCenter), &params.CostCenterID},
		{q.Get(ui.FieldProfitCenter), &params.ProfitCenterID}} {
		if v.value != "" {
			id, err := uuid.Parse(v.value)
			valid = valid && err == nil
			*v.set = gen.NewOptUUID(id)
		}
	}
	if f.Number != "" {
		params.DocumentNumber = gen.NewOptString(f.Number)
	}
	params.PostingDateFrom, f.FromError = dateParam(f.From)
	params.PostingDateTo, f.ToError = dateParam(f.To)
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params, valid
}

// entryRows are a page of the journal.
func entryRows(items []gen.JournalEntry) []ui.EntryRow {
	out := make([]ui.EntryRow, 0, len(items))
	for _, e := range items {
		out = append(out, ui.EntryRow{Path: entryPath(e.ID), Number: e.DocumentNumber, DocType: e.DocumentType, Posting: e.PostingDate,
			Header: e.HeaderText.Or(""), Source: labelOf(sourceContexts, e.SourceContext), Debits: debits(postedAmounts(e.Lines)),
			Reversed: e.ReversedByID.Set, Reversal: e.ReversalOfID.Set})
	}
	return out
}

// postedAmounts are a posted entry's amounts in its currency.
func postedAmounts(lines []gen.JournalLine) []gen.Money {
	out := make([]gen.Money, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Amount)
	}
	return out
}

// centerOptions are the company's cost and profit centers to filter by,
// the chosen ones marked; nil when they cannot be read (logged): the
// centers are no reason to refuse a page.
func (s *Server) centerOptions(ctx context.Context, m me, company uuid.UUID, cost, profit string) ([]ui.Option, []ui.Option) {
	if !m.may(permReadCenters) {
		return nil, nil
	}
	var costs, profits []ui.Option
	res, err := s.API.ListCostCenters(ctx, gen.ListCostCentersParams{LegalEntityID: gen.NewOptUUID(company), PageSize: gen.NewOptInt32(maxCenters),
		OrderBy: gen.NewOptString("code")})
	if page, ok := res.(*gen.CostCenterPage); ok {
		costs = []ui.Option{}
		for _, c := range page.Items {
			costs = append(costs, ui.Option{Value: c.ID.String(), Text: c.Code + " " + c.Name, Selected: c.ID.String() == cost})
		}
	} else {
		s.Log.WarnContext(ctx, "web: the cost centers could not be read", "answer", res, "error", err)
	}
	pres, err := s.API.ListProfitCenters(ctx, gen.ListProfitCentersParams{LegalEntityID: gen.NewOptUUID(company), PageSize: gen.NewOptInt32(maxCenters),
		OrderBy: gen.NewOptString("code")})
	if page, ok := pres.(*gen.ProfitCenterPage); ok {
		profits = []ui.Option{}
		for _, c := range page.Items {
			profits = append(profits, ui.Option{Value: c.ID.String(), Text: c.Code + " " + c.Name, Selected: c.ID.String() == profit})
		}
	} else {
		s.Log.WarnContext(ctx, "web: the profit centers could not be read", "answer", pres, "error", err)
	}
	return costs, profits
}

// journalEntry is a posted entry's page.
func (s *Server) journalEntry(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showEntry(w, r, p, m, id, http.StatusOK, reverseTyped{}, nil)
}

// reverseTyped is what the person sent in the reversal's form that the
// API refused or that was not valid.
type reverseTyped struct {
	date, text       string
	dateErr, textErr i18n.Key
}

// showEntry reads the entry and renders its page with status: the
// reversal's form fresh (the entry's current ETag, a new idempotency key)
// holding what the person typed, and the API's refusal of it.
func (s *Server) showEntry(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, t reverseTyped, refused *ui.Refusal) {
	res, err := s.API.GetJournalEntry(r.Context(), gen.GetJournalEntryParams{ID: id, Expand: m.partnersExpanded()})
	got, ok := res.(*gen.GetJournalEntryOKHeaders)
	if !ok {
		s.pageRefused(w, r, p, m, "getJournalEntry", res, err, i18n.EntryTitle, i18n.EntriesRefusedTitle, ui.EntriesPath, i18n.EntryBack)
		return
	}
	e := got.Response
	ctx := r.Context()
	companies, ledgers, accounts := s.companies(ctx, m), s.ledgers(ctx, m), s.accounts(ctx, m, e.LegalEntityID)
	names := s.names(ctx, p, []uuid.UUID{e.PostedBy})
	ep := ui.EntryPage{Number: e.DocumentNumber, Header: entryHeader(e, companies, ledgers, names), Lines: postedLines(e.Lines, accounts, partnerNames(e.Expanded)),
		Done: entryDone[r.URL.Query().Get("done")], Refusal: refused, Audit: auditLink(m, e.ID),
		Back: ui.EntriesFor(url.Values{ui.FieldCompany: {e.LegalEntityID.String()}, ui.FieldLedger: {e.LedgerID.String()}})}
	switch {
	case e.ReversedByID.Set, e.ReversalOfID.Set, !got.ETag.Set:
		// Reversed, a reversal, or no version to send the reversal at.
	case m.may(permPostEntries):
		ep.Reverse = &ui.ReverseForm{Action: entryPath(id) + "/reverse", CSRF: p.CSRF, ETag: got.ETag.Value, Key: uuid.NewString(),
			Date: t.date, Text: t.text, DateError: t.dateErr, TextError: t.textErr}
	default:
		ep.Why = i18n.EntryNotYours
	}
	s.render(w, r, status, ui.Entry(s.page(i18n.EntryTitle, ui.JournalPath, p, m), ep))
}

// pageRefused answers a page's first read that the API refused: not
// found, or the refusal on a page of its own with the way back.
func (s *Server) pageRefused(w http.ResponseWriter, r *http.Request, p *person, m me, operation string, res any, err error, title, heading i18n.Key,
	back string, backText i18n.Key) {
	prob, show := s.refusal(w, r, p, operation, res, err)
	switch {
	case !show:
	case prob.Status == http.StatusNotFound:
		s.notFound(w, r)
	default:
		s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(title, ui.JournalPath, p, m), heading, back, backText,
			*explainIn(i18n.EntryProblems, heading, prob, m)))
	}
}

// entryHeader is a posted entry's summary: its company and ledger named,
// the parked entry it was posted for and the entries it reverses or that
// reversed it linked.
func entryHeader(e gen.JournalEntry, companies map[uuid.UUID]string, ledgers []gen.Ledger, names map[uuid.UUID]ui.Name) ui.EntryHeader {
	h := ui.EntryHeader{Company: companies[e.LegalEntityID], Ledger: ledgerName(ledgers, e.LedgerID), Year: e.FiscalYear, Period: e.FiscalPeriod,
		DocType: e.DocumentType, DocDate: e.DocumentDate, Posting: e.PostingDate, Currency: e.Currency, Local: e.LocalCurrency, Group: e.GroupCurrency,
		RateLC: string(e.RateTcLc), RateGC: string(e.RateLcGc), Reference: e.Reference.Or(""), Header: e.HeaderText.Or(""),
		Source: labelOf(sourceContexts, e.SourceContext), PostedBy: names[e.PostedBy], PostedAt: e.PostedAt, Debits: debits(postedAmounts(e.Lines))}
	if d, ok := e.TaxDate.Get(); ok {
		h.TaxDate = &d
	}
	if doc, ok := e.SourceDocumentID.Get(); ok && e.SourceContext == "ledger" && !e.ReversalOfID.Set {
		h.Parked = parkedPath(doc)
	}
	if v, ok := e.ReversalOfID.Get(); ok {
		h.ReversalOf = entryPath(v)
	}
	if v, ok := e.ReversedByID.Get(); ok {
		h.ReversedBy = entryPath(v)
	}
	return h
}

// postedLines are a posted entry's lines, their accounts and partners
// named.
func postedLines(lines []gen.JournalLine, accounts, partners map[uuid.UUID]string) []ui.JournalLine {
	out := make([]ui.JournalLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, ui.JournalLine{No: l.LineNo, Account: accountText(accounts, l.AccountID), Amount: moneyOf(l.Amount), AmountLC: new(moneyOf(l.AmountLc)),
			CostCenter: l.CostCenterCode.Or(""), ProfitCenter: l.ProfitCenterCode.Or(""), Segment: l.SegmentCode.Or(""), Partner: partnerText(partners, l.PartnerID),
			TaxCode: l.TaxCode.Or(""), Text: l.LineText.Or("")})
	}
	return out
}

// reverseEntry reverses the entry (reverseJournalEntry, people's alone):
// done, the person goes to the reversal's page; refused (an entry another
// context owns, reversed already, a period not open), the entry's page
// says why and keeps what was typed.
func (s *Server) reverseEntry(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, entryPath(id), i18n.EntryBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t := reverseTyped{date: strings.TrimSpace(r.PostForm.Get(ui.FieldPostingDate)), text: r.PostForm.Get(ui.FieldHeaderText)}
	date, dateErr := dateParam(t.date)
	if dateErr != "" {
		t.dateErr = i18n.ActionDateInvalid
	}
	t.textErr = textError(t.text, maxHeaderText)
	if t.dateErr != "" || t.textErr != "" {
		s.showEntry(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
		return
	}
	body := gen.JournalEntryReverse{PostingDate: date}
	if t.text != "" {
		body.HeaderText = gen.NewOptString(t.text)
	}
	res, err := s.API.ReverseJournalEntry(r.Context(), gen.NewOptJournalEntryReverse(body), gen.ReverseJournalEntryParams{ID: id, IfMatch: ifMatch,
		IdempotencyKey: key})
	if got, ok := res.(*gen.ReverseJournalEntryOKHeaders); ok {
		http.Redirect(w, r, entryPath(got.Response.ID)+"?done=reversed", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "reverseJournalEntry", res, err); show {
		s.showEntry(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.EntryProblems, i18n.ReverseRefusedTitle, prob, m))
	}
}
