package web

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The entry form (ADR-0052 §7, T73h): a new entry's, parked
// (parkJournalEntry), or a parked entry's change (updateParkedJournalEntry,
// its lines replaced as a whole), each with a fresh idempotency key, a
// change with the entry's ETag as If-Match. Lines are added by the form's
// button and left out by a line's box: without htmx the form is sent and
// rendered again, with htmx the lines' section alone is swapped, the line
// added (or the one now in a removed line's place) focused. A line left
// empty is left out when the entry is sent. What is wrong is said on its
// field before the API is called, and what the API refuses of a line (its
// field errors, lines[i].field) on its line. The form renders with
// getCurrentPrincipal, listLegalEntities (a new entry's company and
// currency) or getParkedJournalEntry (a change's entry), and the
// company's accounts, which it suggests by number: three or four calls;
// sending it looks its codes up besides (entry_lookup.go).

// docType is a document type as JournalEntryCreate takes it.
var docType = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// blankLines is how many empty lines a new entry starts with: an entry
// has at least two.
const blankLines = 2

// The fields of a line in the form, in the order they are read.
var lineNames = []string{ui.FieldLineAccount, ui.FieldLineAmount, ui.FieldLineCostCenter, ui.FieldLineProfit, ui.FieldLineSegment,
	ui.FieldLinePartner, ui.FieldLineTaxCode, ui.FieldLineText}

// entryTyped is what the person typed in the entry form, what is wrong
// with it, and the API's refusal of it.
type entryTyped struct {
	company, docType, docDate, postingDate, taxDate string
	currency, reference, header, ifMatch            string
	errs                                            map[string]i18n.Key
	linesErr                                        i18n.Key
	lines                                           []entryLine
	refusal                                         *ui.Refusal
}

// fail records what is wrong with a header's field.
func (t *entryTyped) fail(field string, k i18n.Key) {
	if t.errs == nil {
		t.errs = map[string]i18n.Key{}
	}
	t.errs[field] = k
}

// readEntryForm reads the posted form: its header, its lines in their
// order (those left out dropped, the rest numbered again, at most
// ui.MaxFormLines), whether its button adds a line, and the place the
// first line left out had (0 for none).
func readEntryForm(r *http.Request) (entryTyped, bool, int) {
	f := r.PostForm
	t := entryTyped{company: f.Get(ui.FieldCompany), docType: f.Get(ui.FieldDocType), docDate: f.Get(ui.FieldDocDate), postingDate: f.Get(ui.FieldPostingDate),
		taxDate: f.Get(ui.FieldTaxDate), currency: f.Get(ui.FieldCurrency), reference: f.Get(ui.FieldReference), header: f.Get(ui.FieldHeaderText),
		ifMatch: f.Get(ui.FieldIfMatch)}
	left := map[string]bool{}
	for _, v := range f[ui.FieldLineRemove] {
		left[v] = true
	}
	removed := 0
	for i := range min(len(f[ui.FieldLineAccount]), ui.MaxFormLines) {
		if left[strconv.Itoa(i+1)] {
			if removed == 0 {
				removed = len(t.lines) + 1
			}
			continue
		}
		l := entryLine{no: len(t.lines) + 1}
		for _, name := range lineNames {
			if v := f[name]; i < len(v) {
				setField(&l, name, v[i])
			}
		}
		t.lines = append(t.lines, l)
	}
	return t, f.Get(ui.FieldOp) == ui.OpAdd, removed
}

// edited is the form after its lines were changed but not sent: a line
// added, focused, or the line now in the first removed line's place
// focused; never fewer than two lines.
func (t *entryTyped) edited(add bool, removed int) {
	if add && len(t.lines) < ui.MaxFormLines {
		t.lines = append(t.lines, entryLine{no: len(t.lines) + 1, focus: true})
	}
	if removed > 0 && len(t.lines) > 0 {
		t.lines[min(removed, len(t.lines))-1].focus = true
	}
	t.pad()
}

// pad gives the form at least its two lines.
func (t *entryTyped) pad() {
	for len(t.lines) < blankLines {
		t.lines = append(t.lines, entryLine{no: len(t.lines) + 1})
	}
}

// entrySent is what an entry form sends: its header and its lines.
type entrySent struct {
	docType              string
	docDate, postingDate time.Time
	taxDate              gen.OptDate
	lines                []gen.JournalLineInput
}

// check checks the form in the person's locale before the API is called,
// so what was typed stays on the page, and looks its codes up in the
// company and its chart: the header's fields, at least two lines that
// are not empty, and each line. ok is false when anything is wrong; the
// lines left are then those not empty.
func (s *Server) check(ctx context.Context, t *entryTyped, company uuid.UUID, ch chart, lookup bool) (entrySent, bool) {
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	var sent entrySent
	t.docType, t.currency = strings.ToUpper(strings.TrimSpace(t.docType)), strings.ToUpper(strings.TrimSpace(t.currency))
	sent.docType = t.docType
	if !docType.MatchString(t.docType) {
		t.fail(ui.FieldDocType, i18n.EntryDocTypeInvalid)
	}
	var err i18n.Key
	if sent.docDate, err = requiredDate(strings.TrimSpace(t.docDate)); err != "" {
		t.fail(ui.FieldDocDate, err)
	}
	if sent.postingDate, err = requiredDate(strings.TrimSpace(t.postingDate)); err != "" {
		t.fail(ui.FieldPostingDate, err)
	}
	if sent.taxDate, err = dateParam(strings.TrimSpace(t.taxDate)); err != "" {
		t.fail(ui.FieldTaxDate, i18n.ActionDateInvalid)
	}
	if !currencyCode.MatchString(t.currency) {
		t.fail(ui.FieldCurrency, i18n.ProposeCurrencyInvalid)
	}
	if e := textError(t.reference, maxReference); e != "" {
		t.fail(ui.FieldReference, i18n.EntryReferenceTooLong)
	}
	if e := textError(t.header, maxHeaderText); e != "" {
		t.fail(ui.FieldHeaderText, e)
	}
	lines := t.lines[:0]
	for _, l := range t.lines {
		if !l.blank() {
			l.no = len(lines) + 1
			l.check(loc, t.currency)
			lines = append(lines, l)
		}
	}
	t.lines = lines
	if len(lines) < blankLines {
		t.linesErr = i18n.EntryLinesTooFew
	}
	// The codes are looked up once everything else is right: a send
	// that cannot go makes no call.
	ok := lookup && len(t.errs) == 0 && t.linesErr == "" && shaped(t.lines) && s.resolve(ctx, company, ch, t.lines)
	for _, l := range t.lines {
		sent.lines = append(sent.lines, l.sent)
	}
	return sent, ok
}

// shaped reports whether no line has anything wrong with its shapes.
func shaped(lines []entryLine) bool {
	for _, l := range lines {
		if len(l.errs) > 0 {
			return false
		}
	}
	return true
}

// view is the form as the page shows it, the lines' errors and refusals
// on their lines.
func (t entryTyped) view(action string, p *person, ch chart) ui.EntryForm {
	f := ui.EntryForm{Action: action, CSRF: p.CSRF, Key: uuid.NewString(), ETag: t.ifMatch, DocType: t.docType, DocDate: t.docDate, PostingDate: t.postingDate,
		TaxDate: t.taxDate, Currency: t.currency, Reference: t.reference, Header: t.header, Errors: t.errs, LinesError: t.linesErr, Accounts: ch.options(),
		Refusal: t.refusal}
	for _, l := range t.lines {
		in := ui.LineInput{No: l.no, Account: l.account, Amount: l.amount, CostCenter: l.costCenter, ProfitCenter: l.profitCenter, Segment: l.segment,
			Partner: l.partner, TaxCode: l.taxCode, Text: l.text, Errors: l.errs, Focus: l.focus}
		for _, rf := range l.refused {
			in.Refused = append(in.Refused, ui.FieldRefusal{Field: ui.LineFieldLabel(rf.api), Reason: rf.reason})
		}
		f.Lines = append(f.Lines, in)
	}
	return f
}

// markRefused records, on the lines sent, the fields the API's problem
// names (lines[i] or lines[i].field) and why.
func markRefused(prob gen.Problem, lines []entryLine) {
	for i, rf := range refusedFields(prob, "lines", len(lines)) {
		lines[i].refused = append(lines[i].refused, rf...)
	}
}

// refusedFields are the fields of a list's elements the API's problem
// names (list[i] or list[i].field) and why, by the element's index, of the
// n elements sent.
func refusedFields(prob gen.Problem, list string, n int) map[int][]refusedField {
	out := map[int][]refusedField{}
	for _, fe := range prob.Errors {
		rest, ok := strings.CutPrefix(fe.Field, list+"[")
		index, field, closed := strings.Cut(rest, "]")
		i, err := strconv.Atoi(index)
		if !ok || !closed || err != nil || i < 0 || i >= n {
			continue
		}
		api, _, _ := strings.Cut(strings.TrimPrefix(field, "."), ".")
		out[i] = append(out[i], refusedField{api: api, reason: fe.Code})
	}
	return out
}

// newEntry is the form of a new entry, of the company the query names
// (else the first) in its currency, dated its today when the query names
// it or it is the only one (datedCompany).
func (s *Server) newEntry(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t := entryTyped{company: r.URL.Query().Get(ui.FieldCompany), docType: "SA"}
	t.pad()
	s.showNewEntry(w, r, p, m, http.StatusOK, t, true, nil)
}

// showNewEntry renders the new entry's form with status, the company's
// currency the form's when fresh; with htmx, after its lines changed, its
// lines' section alone.
func (s *Server) showNewEntry(w http.ResponseWriter, r *http.Request, p *person, m me, status int, t entryTyped, fresh bool, ch *chart) {
	ctx := r.Context()
	entities := s.entities(ctx, m)
	names := map[uuid.UUID]string{}
	for id, e := range entities {
		names[id] = e.Name
	}
	if entities == nil {
		names = nil // no choice offered: the companies could not be listed
	}
	options := companyOptions(names, t.company)
	dated := datedCompany(t.company, options)
	t.company = askedOrFirst(t.company, options)
	company, _ := uuid.Parse(t.company) // uuid.Nil when none: its chart is then empty
	if e, ok := entities[company]; ok && fresh {
		t.currency = e.LocalCurrency
	}
	if fresh {
		today := s.today(ctx, m, dated)
		t.docDate, t.postingDate = today, today
	}
	if ch == nil {
		read := s.chartOf(ctx, m, company)
		ch = &read
	}
	f := t.view(ui.NewEntryPath, p, *ch)
	f.Companies, f.Back = options, ui.JournalPath
	if t.errs[ui.FieldCompany] != "" {
		f.CompanyError = t.errs[ui.FieldCompany]
	}
	s.renderForm(w, r, p, m, status, f)
}

// renderForm renders an entry form's page, or with htmx (its lines
// changed) its lines' section alone.
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, p *person, m me, status int, f ui.EntryForm) {
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, status, ui.EntryLines(f))
		return
	}
	title := i18n.NewEntryTitle
	if f.Edit {
		title = i18n.EditEntryTitle
	}
	s.render(w, r, status, ui.EntryFormPage(s.page(title, ui.JournalPath, p, m), f))
}

// formKey is a posted form's idempotency key; ok is false, the answer
// written (with the way back to the page), when the form is not the
// page's.
func (s *Server) formKey(w http.ResponseWriter, r *http.Request, back string, backText i18n.Key) (string, bool) {
	// csrf parsed the form unless htmx sent the token as a header; a body
	// that does not parse leaves the fields empty, refused below.
	_ = r.ParseForm()
	key := r.PostForm.Get(ui.FieldIdempotency)
	if _, err := uuid.Parse(key); err != nil {
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody, back, backText))
		return "", false
	}
	return key, true
}

// parkEntry parks the new entry (parkJournalEntry), or, when the form's
// lines were changed instead, renders it again: parked, the person goes
// to the entry's page; refused, the form says why and keeps what was
// typed.
func (s *Server) parkEntry(w http.ResponseWriter, r *http.Request, p *person) {
	key, ok := s.formKey(w, r, ui.NewEntryPath, i18n.ParkedBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t, add, removed := readEntryForm(r)
	if add || r.Header.Get("HX-Request") == "true" {
		t.edited(add, removed)
		s.showNewEntry(w, r, p, m, http.StatusOK, t, false, nil)
		return
	}
	ctx := r.Context()
	company, err := uuid.Parse(t.company)
	ch := chart{}
	if err != nil {
		t.fail(ui.FieldCompany, i18n.ProposeCompanyNeeded)
	} else {
		ch = s.chartOf(ctx, m, company)
	}
	sent, valid := s.check(ctx, &t, company, ch, err == nil)
	if !valid {
		t.pad()
		s.showNewEntry(w, r, p, m, http.StatusUnprocessableEntity, t, false, &ch)
		return
	}
	body := gen.JournalEntryCreate{LegalEntityID: company, DocumentType: gen.NewOptString(sent.docType), DocumentDate: sent.docDate,
		PostingDate: sent.postingDate, TaxDate: sent.taxDate, Currency: t.currency, Lines: sent.lines}
	if t.reference != "" {
		body.Reference = gen.NewOptString(t.reference)
	}
	if t.header != "" {
		body.HeaderText = gen.NewOptString(t.header)
	}
	res, err := s.API.ParkJournalEntry(ctx, &body, gen.ParkJournalEntryParams{IdempotencyKey: key})
	if pe, ok := res.(*gen.ParkedJournalEntryHeaders); ok {
		http.Redirect(w, r, parkedPath(pe.Response.ID)+"?done=parked", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "parkJournalEntry", res, err); show {
		t.refusal = explainIn(i18n.EntryProblems, i18n.EntryRefusedTitle, prob, m)
		markRefused(prob, t.lines)
		s.showNewEntry(w, r, p, m, int(prob.Status), t, false, &ch)
	}
}

// editEntry is the form of a parked entry's change, as the entry is.
func (s *Server) editEntry(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	pe, tag, ok := s.readParked(w, r, p, m, id)
	if !ok {
		return
	}
	if pe.Status != "parked" {
		// Only a parked entry changes: its page says what it is now.
		http.Redirect(w, r, parkedPath(id), http.StatusSeeOther)
		return
	}
	ch := s.chartOf(r.Context(), m, pe.LegalEntityID)
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	t := entryTyped{docType: pe.DocumentType, docDate: pe.DocumentDate.Format(time.DateOnly), postingDate: pe.PostingDate.Format(time.DateOnly),
		currency: pe.Currency, reference: pe.Reference.Or(""), header: pe.HeaderText.Or(""), ifMatch: tag}
	if d, ok := pe.TaxDate.Get(); ok {
		t.taxDate = d.Format(time.DateOnly)
	}
	for i, l := range pe.Lines {
		account := l.AccountID.String() // an account the chart's page does not hold is sent by its id
		if a, ok := ch.byID[l.AccountID]; ok {
			account = a.Number
		}
		t.lines = append(t.lines, entryLine{no: i + 1, account: account, amount: loc.AmountInput(l.Amount.Amount), costCenter: l.CostCenterCode.Or(""),
			profitCenter: l.ProfitCenterCode.Or(""), segment: l.SegmentCode.Or(""), partner: idText(l.PartnerID), taxCode: l.TaxCode.Or(""),
			text: l.LineText.Or("")})
	}
	t.pad()
	s.showEdit(w, r, p, m, pe, http.StatusOK, t, ch)
}

// showEdit renders a parked entry's change with status.
func (s *Server) showEdit(w http.ResponseWriter, r *http.Request, p *person, m me, pe gen.ParkedJournalEntry, status int, t entryTyped, ch chart) {
	f := t.view(parkedPath(pe.ID)+"/edit", p, ch)
	f.Company, f.Back = s.companies(r.Context(), m)[pe.LegalEntityID], parkedPath(pe.ID)
	f.Edit = true
	s.renderForm(w, r, p, m, status, f)
}

// saveEntry sends a parked entry's change (updateParkedJournalEntry, at
// the ETag the form was opened with), or, when the form's lines were
// changed instead, renders it again: done, the person goes to the entry's
// page; refused (a change meanwhile, an entry no longer parked, another
// person's), the form says why, keeps what was typed and takes the
// entry's version now.
func (s *Server) saveEntry(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	key, ok := s.formKey(w, r, parkedPath(id), i18n.ParkedBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	pe, tag, ok := s.readParked(w, r, p, m, id)
	if !ok {
		return
	}
	ctx := r.Context()
	ch := s.chartOf(ctx, m, pe.LegalEntityID)
	t, add, removed := readEntryForm(r)
	if add || r.Header.Get("HX-Request") == "true" {
		t.edited(add, removed)
		s.showEdit(w, r, p, m, pe, http.StatusOK, t, ch)
		return
	}
	sent, valid := s.check(ctx, &t, pe.LegalEntityID, ch, true)
	if !valid {
		t.pad()
		s.showEdit(w, r, p, m, pe, http.StatusUnprocessableEntity, t, ch)
		return
	}
	res, err := s.API.UpdateParkedJournalEntry(ctx, changeOf(t, sent), gen.UpdateParkedJournalEntryParams{ID: id, IfMatch: t.ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.UpdateParkedJournalEntryOKHeaders); ok {
		http.Redirect(w, r, parkedPath(id)+"?done=changed", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "updateParkedJournalEntry", res, err); show {
		t.refusal, t.ifMatch = explainIn(i18n.EntryProblems, i18n.EntryRefusedTitle, prob, m), tag
		markRefused(prob, t.lines)
		s.showEdit(w, r, p, m, pe, int(prob.Status), t, ch)
	}
}

// changeOf is a parked entry's change as a merge patch: its header as
// typed (an empty tax date, reference or header text cleared) and its
// lines replaced as a whole.
func changeOf(t entryTyped, sent entrySent) *gen.ParkedJournalEntryUpdate {
	body := &gen.ParkedJournalEntryUpdate{DocumentType: gen.NewOptString(sent.docType), DocumentDate: gen.NewOptDate(sent.docDate),
		PostingDate: gen.NewOptDate(sent.postingDate), Currency: gen.NewOptString(t.currency), Lines: sent.lines,
		TaxDate: gen.OptNilDate{Set: true, Null: true}, Reference: gen.OptNilString{Set: true, Null: true}, HeaderText: gen.OptNilString{Set: true, Null: true}}
	if d, ok := sent.taxDate.Get(); ok {
		body.TaxDate = gen.NewOptNilDate(d)
	}
	if t.reference != "" {
		body.Reference = gen.NewOptNilString(t.reference)
	}
	if t.header != "" {
		body.HeaderText = gen.NewOptNilString(t.header)
	}
	return body
}
