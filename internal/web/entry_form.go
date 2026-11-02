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
// field errors, lines[i].field) on its line. A new entry is also posted
// at once or parked from a file (entry_send.go, T73h2). The new entry's
// form renders with getCurrentPrincipal, listLegalEntities, the
// company's day, its accounts, which it suggests by number, its ledgers
// and the ledger groups: six calls; a change's with getParkedJournalEntry
// in place of the day and ledgers and the lines' partners besides: five.
// Sending a form looks its codes up besides (entry_lookup.go).

// quantityUnit is the API's name of a line's quantity's unit, which its
// field errors name (lines[i].quantity.unit).
const quantityUnit = "quantity.unit"

// docType is a document type as JournalEntryCreate takes it.
var docType = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// blankLines is how many empty lines a new entry starts with: an entry
// has at least two.
const blankLines = 2

// The fields of a line in the form, in the order they are read.
var lineNames = []string{ui.FieldLineAccount, ui.FieldLineAmount, ui.FieldLineCostCenter, ui.FieldLineProfit, ui.FieldLineSegment,
	ui.FieldLinePartner, ui.FieldLineTaxCode, ui.FieldLineTaxArea, ui.FieldLineTaxBase, ui.FieldLineQuantity, ui.FieldLineUnit, ui.FieldLineText}

// entryTyped is what the person typed in the entry form, what the form
// asks (its op), what is wrong with it, and the API's refusal of it. A
// new entry's form also holds the company its currency and dates were
// filled in for (filledFor), the ledgers it posts to, whether the
// company was changed since, and what is wrong with a file of its lines.
type entryTyped struct {
	company, docType, docDate, postingDate, taxDate string
	currency, reference, header, ifMatch            string
	op, filledFor, filledOn, postTo                 string
	errs                                            map[string]i18n.Key
	linesErr                                        i18n.Key
	lines                                           []entryLine
	refusal                                         *ui.Refusal
	changed                                         bool
	file                                            *ui.LinesUpload
}

// fail records what is wrong with a header's field.
func (t *entryTyped) fail(field string, k i18n.Key) {
	if t.errs == nil {
		t.errs = map[string]i18n.Key{}
	}
	t.errs[field] = k
}

// readEntryForm reads the posted form: its header, what it asks, its
// lines in their order (those left out dropped, the rest numbered again,
// at most ui.MaxFormLines), and the place the first line left out had (0
// for none).
func readEntryForm(r *http.Request) (entryTyped, int) {
	f := r.PostForm
	t := entryTyped{company: f.Get(ui.FieldCompany), docType: f.Get(ui.FieldDocType), docDate: f.Get(ui.FieldDocDate), postingDate: f.Get(ui.FieldPostingDate),
		taxDate: f.Get(ui.FieldTaxDate), currency: f.Get(ui.FieldCurrency), reference: f.Get(ui.FieldReference), header: f.Get(ui.FieldHeaderText),
		ifMatch: f.Get(ui.FieldIfMatch), op: f.Get(ui.FieldOp), filledFor: f.Get(ui.FieldFilledFor), filledOn: f.Get(ui.FieldFilledOn),
		postTo: f.Get(ui.FieldPostTo)}
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
	return t, removed
}

// switched reports whether a new entry's company is not the one its
// currency and dates were filled in for: chosen in the browser without
// htmx, the form was not rendered for it, and sending it would send one
// company's currency and day for another. A company that is no id is the
// company field's error instead.
func (t entryTyped) switched() bool {
	_, err := uuid.Parse(t.company)
	return err == nil && t.filledFor != "" && t.filledFor != t.company
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

// entrySent is what an entry form sends: its header, the ledgers a new
// entry posts to (one ledger, a ledger group, or neither: every ledger
// its company keeps), and its lines.
type entrySent struct {
	docType              string
	docDate, postingDate time.Time
	taxDate              gen.OptDate
	ledger               gen.OptUUID
	group                gen.OptString
	lines                []gen.JournalLineInput
}

// ledgerGroupCode is JournalEntryCreate.ledger_group.
var ledgerGroupCode = regexp.MustCompile(`^[0-9A-Z]{1,4}$`)

// checkHeader checks the form's header in the person's locale before the
// API is called, so what was typed stays on the page; the currency and
// document type are taken in capitals.
func (t *entryTyped) checkHeader() entrySent {
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
	t.checkPostTo(&sent)
	return sent
}

// checkPostTo reads the ledgers a new entry posts to, as the form offers
// them (ui.PostToLedger and ui.PostToGroup).
func (t *entryTyped) checkPostTo(sent *entrySent) {
	kind, value, _ := strings.Cut(t.postTo, ":")
	switch kind + ":" {
	case ":":
		return
	case ui.PostToLedger:
		if id, err := uuid.Parse(value); err == nil {
			sent.ledger = gen.NewOptUUID(id)
			return
		}
	case ui.PostToGroup:
		if ledgerGroupCode.MatchString(value) {
			sent.group = gen.NewOptString(value)
			return
		}
	}
	t.fail(ui.FieldPostTo, i18n.EntryPostToInvalid)
}

// check checks the form before the API is called and looks its codes up
// in the company and its chart: the header, at least two lines that are
// not empty, and each line. ok is false when anything is wrong; the lines
// left are then those not empty.
func (s *Server) check(ctx context.Context, t *entryTyped, company uuid.UUID, ch chart, lookup bool) (entrySent, bool) {
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	sent := t.checkHeader()
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
	sent.lines = linesSent(t.lines)
	return sent, ok
}

// linesSent are the checked lines as the API takes them.
func linesSent(lines []entryLine) []gen.JournalLineInput {
	out := make([]gen.JournalLineInput, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.sent)
	}
	return out
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
// on their lines, and its running totals.
func (t entryTyped) view(loc i18n.Locale, action string, p *person, ch chart) ui.EntryForm {
	f := ui.EntryForm{Action: action, CSRF: p.CSRF, Key: uuid.NewString(), ETag: t.ifMatch, DocType: t.docType, DocDate: t.docDate, PostingDate: t.postingDate,
		TaxDate: t.taxDate, Currency: t.currency, Reference: t.reference, Header: t.header, Errors: t.errs, LinesError: t.linesErr, Accounts: ch.options(),
		Refusal: t.refusal, Totals: totalsOf(loc, t.lines, t.currency)}
	for _, l := range t.lines {
		in := ui.LineInput{No: l.no, Account: l.account, Amount: l.amount, CostCenter: l.costCenter, ProfitCenter: l.profitCenter, Segment: l.segment,
			Partner: l.partner, TaxCode: l.taxCode, TaxArea: l.taxArea, TaxBase: l.taxBase, Quantity: l.quantity, Unit: l.unit, Text: l.text,
			Errors: l.errs, Focus: l.focus}
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
		api := strings.TrimPrefix(field, ".")
		if api != quantityUnit { // a quantity's unit is a field of its own on the line
			api, _, _ = strings.Cut(api, ".")
		}
		out[i] = append(out[i], refusedField{api: api, reason: fe.Code})
	}
	return out
}

// newEntry is the form of a new entry, of the company the query names
// (else the first), its currency and dates filled in for the company
// only when the query names it or it is the only one (datedCompany).
func (s *Server) newEntry(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t := entryTyped{company: r.URL.Query().Get(ui.FieldCompany), docType: "SA"}
	t.pad()
	s.showNewEntry(w, r, p, m, http.StatusOK, t, fillDated, nil)
}

// fillFor is for which company showNewEntry fills in a new entry's
// currency and dates: none, the one its address names or the only one
// (datedCompany), or the one the form names, chosen by the person.
type fillFor int

const (
	fillNone fillFor = iota
	fillDated
	fillChosen
)

// showNewEntry renders the new entry's form with status, its currency and
// dates filled in as fill says; with htmx, after its lines changed, its
// lines' section alone, and after its company was chosen, the form.
func (s *Server) showNewEntry(w http.ResponseWriter, r *http.Request, p *person, m me, status int, t entryTyped, fill fillFor, ch *chart) {
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
	target := t.company
	if fill == fillDated {
		target = datedCompany(t.company, options)
	}
	t.company = askedOrFirst(t.company, options)
	if fill != fillNone {
		s.fill(ctx, m, &t, target, entities)
	}
	company, _ := uuid.Parse(t.company) // uuid.Nil when none: its chart is then empty
	if ch == nil {
		read := s.chartOf(ctx, m, company)
		ch = &read
	}
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	f := t.view(loc, ui.NewEntryPath, p, *ch)
	f.Companies, f.Back, f.FilledFor, f.FilledOn, f.Changed, f.CompanyError = options, ui.JournalPath, t.filledFor, t.filledOn, t.changed,
		t.errs[ui.FieldCompany]
	f.Local, f.CanPost, f.File = entities[company].LocalCurrency, m.may(permPostEntries), t.file
	if f.File == nil {
		f.File = &ui.LinesUpload{}
	}
	if !linesAlone(r, t.op) {
		f.PostTo = s.postToOptions(ctx, m, company, t.postTo)
	}
	s.renderForm(w, r, p, m, status, f, t.op)
}

// fill fills in a new entry's currency and dates for the company (an id
// the person may see): its local currency and its day today
// (getLegalEntityToday). A field still holding what was filled in for
// another company (its currency, the day filledOn) is replaced, and so
// is an empty one; what the person typed is kept. When anything filled
// in for another company is replaced, the form says so. For a company the
// person may not see, nothing is filled in, and what the form holds is
// the person's: sent, the API refuses the company.
func (s *Server) fill(ctx context.Context, m me, t *entryTyped, company string, entities map[uuid.UUID]gen.LegalEntity) {
	id, err := uuid.Parse(company)
	if err != nil || t.filledFor == company {
		return
	}
	e, known := entities[id]
	if !known {
		t.filledFor = ""
		return
	}
	before := ""
	if old, err := uuid.Parse(t.filledFor); err == nil {
		before = entities[old].LocalCurrency
	}
	today := s.today(ctx, m, company)
	for _, f := range []struct{ field, was, to *string }{{&t.currency, &before, &e.LocalCurrency}, {&t.docDate, &t.filledOn, &today},
		{&t.postingDate, &t.filledOn, &today}} {
		typed := strings.TrimSpace(*f.field)
		switch {
		case typed == "":
			*f.field = *f.to
		case typed == *f.was && *f.was != *f.to:
			*f.field, t.changed = *f.to, true
		}
	}
	t.filledFor, t.filledOn = company, today
}

// linesAlone reports whether the answer is the form's lines' section
// alone: an htmx request that changed its lines.
func linesAlone(r *http.Request, op string) bool {
	return r.Header.Get("HX-Request") == "true" && op != ui.OpCompany
}

// renderForm renders an entry form's page, or with htmx its lines'
// section alone (its lines changed) or the form (its company chosen).
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, p *person, m me, status int, f ui.EntryForm, op string) {
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		if op == ui.OpCompany {
			s.render(w, r, status, ui.EntryFormView(f))
			return
		}
		s.render(w, r, status, ui.EntryLinesSwap(f))
		return
	}
	title := i18n.NewEntryTitle
	if f.Edit {
		title = i18n.EditEntryTitle
	}
	s.render(w, r, status, ui.EntryFormPage(s.page(title, ui.JournalPath, p, m), f))
}

// renderTotals answers an htmx request for the form's running totals, as
// the form holds its lines: no call to the API.
func (s *Server) renderTotals(w http.ResponseWriter, r *http.Request, t entryTyped) {
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	s.render(w, r, http.StatusOK, ui.EntryTotalsBody(totalsOf(loc, t.lines, strings.ToUpper(strings.TrimSpace(t.currency)))))
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

// parkEntry sends the new entry (sendNew), or, when the form's lines
// were changed or its company chosen instead, renders it again; asked
// for its running totals, they alone. A form whose company was changed
// without the form being rendered for it is rendered for the company,
// its currency and dates the company's, and sent only when sent again.
func (s *Server) parkEntry(w http.ResponseWriter, r *http.Request, p *person) {
	key, ok := s.formKey(w, r, ui.NewEntryPath, i18n.ParkedBack)
	if !ok {
		return
	}
	t, removed := readEntryForm(r)
	if t.op == ui.OpTotals {
		s.renderTotals(w, r, t)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	if t.op == ui.OpAdd || t.op == ui.OpCompany || r.Header.Get("HX-Request") == "true" {
		if t.op == ui.OpCompany {
			t.postTo = "" // another company keeps other ledgers: every ledger until chosen again
		}
		t.edited(t.op == ui.OpAdd, removed)
		fill := fillChosen
		if linesAlone(r, t.op) {
			fill = fillNone // the header is not rendered
		}
		s.showNewEntry(w, r, p, m, http.StatusOK, t, fill, nil)
		return
	}
	if t.switched() {
		t.changed, t.postTo = true, ""
		t.pad()
		s.showNewEntry(w, r, p, m, http.StatusUnprocessableEntity, t, fillChosen, nil)
		return
	}
	ctx := r.Context()
	company, ch, known := s.companyOf(ctx, m, &t)
	sent, valid := s.check(ctx, &t, company, ch, known)
	if !valid {
		t.pad()
		s.showNewEntry(w, r, p, m, http.StatusUnprocessableEntity, t, fillNone, &ch)
		return
	}
	s.sendNew(w, r, p, m, key, t, createOf(company, t, sent), ch)
}

// companyOf is a new entry's company and its chart; known is false, said
// on the company's field, when the form names no company.
func (s *Server) companyOf(ctx context.Context, m me, t *entryTyped) (uuid.UUID, chart, bool) {
	company, err := uuid.Parse(t.company)
	if err != nil {
		t.fail(ui.FieldCompany, i18n.ProposeCompanyNeeded)
		return company, chart{}, false
	}
	return company, s.chartOf(ctx, m, company), true
}

// createOf is a new entry as the API takes it, to park or to post.
func createOf(company uuid.UUID, t entryTyped, sent entrySent) gen.JournalEntryCreate {
	body := gen.JournalEntryCreate{LegalEntityID: company, DocumentType: gen.NewOptString(sent.docType), DocumentDate: sent.docDate,
		PostingDate: sent.postingDate, TaxDate: sent.taxDate, Currency: t.currency, Lines: sent.lines, LedgerID: sent.ledger, LedgerGroup: sent.group}
	if t.reference != "" {
		body.Reference = gen.NewOptString(t.reference)
	}
	if t.header != "" {
		body.HeaderText = gen.NewOptString(t.header)
	}
	return body
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
	ctx := r.Context()
	ch := s.chartOf(ctx, m, pe.LegalEntityID)
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	t := entryTyped{docType: pe.DocumentType, docDate: pe.DocumentDate.Format(time.DateOnly), postingDate: pe.PostingDate.Format(time.DateOnly),
		currency: pe.Currency, reference: pe.Reference.Or(""), header: pe.HeaderText.Or(""), ifMatch: tag}
	if d, ok := pe.TaxDate.Get(); ok {
		t.taxDate = d.Format(time.DateOnly)
	}
	partners := s.partnerNumbers(ctx, m, pe.Lines)
	for i, l := range pe.Lines {
		t.lines = append(t.lines, parkedLineTyped(loc, i+1, l, ch, partners))
	}
	t.pad()
	s.showEdit(w, r, p, m, pe, http.StatusOK, t, ch)
}

// parkedLineTyped is a parked line as the form shows it to be changed: its
// account by number and its partner by number (by id when the chart's
// page or the partners read do not hold them: sent so, they are taken as
// they are), its amounts and quantity in the person's locale.
func parkedLineTyped(loc i18n.Locale, no int, l gen.ParkedJournalLine, ch chart, partners map[uuid.UUID]string) entryLine {
	account := l.AccountID.String()
	if a, ok := ch.byID[l.AccountID]; ok {
		account = a.Number
	}
	partner := idText(l.PartnerID)
	if id, ok := l.PartnerID.Get(); ok && partners[id] != "" {
		partner = partners[id]
	}
	out := entryLine{no: no, account: account, amount: loc.AmountInput(l.Amount.Amount), costCenter: l.CostCenterCode.Or(""),
		profitCenter: l.ProfitCenterCode.Or(""), segment: l.SegmentCode.Or(""), partner: partner, taxCode: l.TaxCode.Or(""),
		taxArea: l.TaxJurisdiction.Or(""), text: l.LineText.Or("")}
	if b, ok := l.TaxBase.Get(); ok {
		out.taxBase = loc.AmountInput(b.Amount)
	}
	if q, ok := l.Quantity.Get(); ok {
		out.quantity, out.unit = loc.AmountInput(q.Value), q.Unit
	}
	return out
}

// showEdit renders a parked entry's change with status.
func (s *Server) showEdit(w http.ResponseWriter, r *http.Request, p *person, m me, pe gen.ParkedJournalEntry, status int, t entryTyped, ch chart) {
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	f := t.view(loc, parkedPath(pe.ID)+"/edit", p, ch)
	f.Company, f.Back = s.companies(r.Context(), m)[pe.LegalEntityID], parkedPath(pe.ID)
	f.Edit = true
	s.renderForm(w, r, p, m, status, f, t.op)
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
	t, removed := readEntryForm(r)
	if t.op == ui.OpTotals {
		s.renderTotals(w, r, t)
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
	if t.op == ui.OpAdd || r.Header.Get("HX-Request") == "true" {
		t.edited(t.op == ui.OpAdd, removed)
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
