package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// An entry's lines as a person types them name accounts by number,
// centers and segments by code and partners by number, as in SAP; the
// API takes ids. Each code becomes its id here, before the call: an
// account from the company's chart (the page the form suggests from, then
// one call a number when the chart is longer), each center, segment and
// partner by the API's own filter, one call per code the lines name.
// These lookups prepare the write: they are not a page's reads (ADR-0052
// §10's six), and a code the API does not know is said on its line. A
// lookup that fails is said on its line too, logged: the line cannot be
// sent, the page still answers.

// The shapes of what a line takes, as JournalLineInput's patterns and
// lengths say, checked before the API is called so a typed value stays on
// the page: a center's or segment's code, a tax code, and texts.
var (
	centerCode   = regexp.MustCompile(`^[0-9A-Z_-]{1,10}$`)
	taxCode      = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)
	jurisdiction = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)
	unitCode     = regexp.MustCompile(`^[A-Z0-9]{1,6}$`)
)

const (
	maxLineText   = 200 // JournalLineInput.line_text
	maxHeaderText = 200 // JournalEntryCreate.header_text, JournalEntryReverse.header_text
	maxReference  = 40  // JournalEntryCreate.reference
)

// entryLine is one line as typed in the form or read from a file: its
// number there (the form's line, the file's row), its fields, what is
// wrong with each by its field's name, what the API refused of it, and,
// checked and looked up, what is sent.
type entryLine struct {
	no                             int
	account, amount, costCenter    string
	profitCenter, segment, partner string
	taxCode, taxArea, taxBase      string
	quantity, unit, text           string
	errs                           map[string]i18n.Key
	refused                        []refusedField
	focus                          bool
	sent                           gen.JournalLineInput
}

// refusedField is a line's field the API refused (a FieldError naming
// it, lines[i].field) and why.
type refusedField struct{ api, reason string }

// blank reports whether the line holds nothing: it is left out.
func (l entryLine) blank() bool {
	return strings.TrimSpace(l.account+l.amount+l.costCenter+l.profitCenter+l.segment+l.partner+l.taxCode+l.taxArea+l.taxBase+l.quantity+
		l.unit+l.text) == ""
}

// fail records what is wrong with the line's field.
func (l *entryLine) fail(field string, k i18n.Key) {
	if l.errs == nil {
		l.errs = map[string]i18n.Key{}
	}
	l.errs[field] = k
}

// check checks the line's shapes in the person's locale and the
// currency: its account and amount required, the amount a decimal that is
// not zero, the codes the API's shapes, its tax and quantity (checkTax),
// the text at most 200 characters. The codes are taken in capitals, as
// the API keeps them.
func (l *entryLine) check(loc i18n.Locale, currency string) {
	l.account, l.partner = strings.TrimSpace(l.account), strings.TrimSpace(l.partner)
	for _, code := range []*string{&l.costCenter, &l.profitCenter, &l.segment, &l.taxCode, &l.taxArea, &l.unit} {
		*code = strings.ToUpper(strings.TrimSpace(*code))
	}
	if l.account == "" {
		l.fail(ui.FieldLineAccount, i18n.EntryAccountNeeded)
	}
	amount, ok := loc.Amount(l.amount)
	switch {
	case strings.TrimSpace(l.amount) == "":
		l.fail(ui.FieldLineAmount, i18n.EntryAmountNeeded)
	case !ok:
		l.fail(ui.FieldLineAmount, i18n.EntryAmountInvalid)
	case strings.Trim(amount, "-0.") == "":
		l.fail(ui.FieldLineAmount, i18n.EntryAmountZero)
	}
	l.sent = gen.JournalLineInput{Amount: gen.Money{Amount: amount, Currency: currency}}
	for field, code := range map[string]string{ui.FieldLineCostCenter: l.costCenter, ui.FieldLineProfit: l.profitCenter, ui.FieldLineSegment: l.segment} {
		if code != "" && !centerCode.MatchString(code) {
			l.fail(field, i18n.EntryCodeInvalid)
		}
	}
	if l.taxCode != "" {
		if !taxCode.MatchString(l.taxCode) {
			l.fail(ui.FieldLineTaxCode, i18n.EntryTaxCodeInvalid)
		}
		l.sent.TaxCode = gen.NewOptString(l.taxCode)
	}
	if l.text != "" {
		l.sent.LineText = gen.NewOptString(l.text)
		if e := textError(l.text, maxLineText); e != "" {
			l.fail(ui.FieldLineText, e)
		}
	}
	l.checkTax(loc, currency)
}

// checkTax checks the line's tax jurisdiction (ISO 3166-2), its declared
// tax base (an amount in the currency, as the line's is typed) and its
// quantity, a decimal of at most six places with its unit, each both or
// neither; the API says whether the line's tax code takes them.
func (l *entryLine) checkTax(loc i18n.Locale, currency string) {
	if l.taxArea != "" {
		if !jurisdiction.MatchString(l.taxArea) {
			l.fail(ui.FieldLineTaxArea, i18n.EntryTaxAreaInvalid)
		}
		l.sent.TaxJurisdiction = gen.NewOptString(l.taxArea)
	}
	if strings.TrimSpace(l.taxBase) != "" {
		base, ok := loc.Amount(l.taxBase)
		if !ok {
			l.fail(ui.FieldLineTaxBase, i18n.EntryAmountInvalid)
		}
		l.sent.TaxBase = gen.NewOptMoney(gen.Money{Amount: base, Currency: currency})
	}
	value, ok := loc.Quantity(l.quantity)
	typed := strings.TrimSpace(l.quantity) != ""
	switch {
	case !typed && l.unit == "":
		return
	case !typed:
		l.fail(ui.FieldLineQuantity, i18n.EntryQuantityNeeded)
	case !ok:
		l.fail(ui.FieldLineQuantity, i18n.EntryQuantityInvalid)
	}
	switch {
	case l.unit == "":
		l.fail(ui.FieldLineUnit, i18n.EntryUnitNeeded)
	case !unitCode.MatchString(l.unit):
		l.fail(ui.FieldLineUnit, i18n.EntryUnitInvalid)
	}
	l.sent.Quantity = gen.NewOptQuantity(gen.Quantity{Value: value, Unit: l.unit})
}

// textError is what is wrong with a text of at most max characters.
func textError(text string, max int) i18n.Key {
	if utf8.RuneCountInString(text) > max {
		return i18n.EntryTextTooLong
	}
	return ""
}

// found is what a lookup says of a code: its id, or why it has none (not
// known, the lookup refused or failed, too many codes to look up).
type found struct {
	id  uuid.UUID
	err i18n.Key
}

// maxCodeLookups is the most codes one send looks up one by one: the
// partners, and the accounts, centers and segments a page of their list
// does not hold. With a page's six reads and the three pages of codes a
// send reads, it stays well within the API's 50 calls a second for each
// principal (04 §4.11); a send naming more is refused on the lines past
// the limit, before any call, rather than by the API's rate limit.
const maxCodeLookups = 20

// finder looks one code up: known false when the API knows none, err
// what went wrong when the call did (refused, or failed: logged).
type finder func(code string) (id uuid.UUID, known bool, err i18n.Key)

// book is one kind's codes as a page of its list holds them, and whether
// there may be more (a longer list, or a page that could not be read):
// then a code the page does not hold is looked up by itself.
type book struct {
	ids  map[string]uuid.UUID
	more bool
}

// lookups are the codes one send may still look up one by one.
type lookups struct{ left int }

// lookUp says what each distinct code is: its id from the book, the id
// itself when a code is one (a parked line names its partner, and an
// account its chart's page does not hold, by id), else found by itself
// while the book may hold more and the send may still look codes up.
func (lk *lookups) lookUp(codes []string, known book, unknown i18n.Key, find finder) map[string]found {
	out := map[string]found{}
	for _, code := range codes {
		if _, done := out[code]; done || code == "" {
			continue
		}
		if id, ok := known.ids[code]; ok {
			out[code] = found{id: id}
			continue
		}
		if id, err := uuid.Parse(code); err == nil {
			out[code] = found{id: id}
			continue
		}
		switch {
		case !known.more:
			out[code] = found{err: unknown}
			continue
		case lk.left == 0:
			out[code] = found{err: i18n.EntryTooManyCodes}
			continue
		}
		lk.left--
		id, isKnown, err := find(code)
		switch {
		case err != "":
			out[code] = found{err: err}
		case !isKnown:
			out[code] = found{err: unknown}
		default:
			out[code] = found{id: id}
		}
	}
	return out
}

// lookupError is what a lookup's or a page's answer that is no page
// says: refused (a permission the person lacks), else failed, logged.
func (s *Server) lookupError(ctx context.Context, what string, res any, err error) i18n.Key {
	if prob, ok := problemOf(res); ok && prob.Status == http.StatusForbidden {
		return i18n.EntryLookupRefused
	}
	s.Log.WarnContext(ctx, "web: a line's code could not be looked up", "what", what, "answer", res, "error", err)
	return i18n.EntryLookupFailed
}

// finderOf is a finder by one of the API's lists filtered by the code
// (list), items reading its page's items and id an item's id.
func finderOf[P any, I any](s *Server, ctx context.Context, what string, list func(code string) (any, error), items func(P) ([]I, bool),
	id func(I) uuid.UUID) finder {
	return func(code string) (uuid.UUID, bool, i18n.Key) {
		res, err := list(code)
		page, ok := res.(P)
		if !ok {
			return uuid.Nil, false, s.lookupError(ctx, what, res, err)
		}
		if all, _ := items(page); len(all) > 0 {
			return id(all[0]), true, ""
		}
		return uuid.Nil, false, ""
	}
}

// codesOf are a kind's book, from the first page of its list (list with
// no code), and its finder by code; a page that could not be read leaves
// every code to be looked up by itself.
func codesOf[P any, I any](s *Server, ctx context.Context, what string, list func(code string) (any, error), items func(P) ([]I, bool),
	code func(I) string, id func(I) uuid.UUID) (book, finder) {
	find := finderOf(s, ctx, what, list, items, id)
	res, err := list("")
	page, ok := res.(P)
	if !ok {
		s.lookupError(ctx, what, res, err)
		return book{more: true}, find
	}
	all, more := items(page)
	b := book{ids: make(map[string]uuid.UUID, len(all)), more: more}
	for _, it := range all {
		b.ids[code(it)] = id(it)
	}
	return b, find
}

// The codes a line names: an account by number (its book the chart), a
// cost or profit center of the company and a segment by code, a partner
// by number (no book: one call a partner). Each list with no code is its
// first page, else filtered by the code.
func (s *Server) accountFinder(ctx context.Context, company uuid.UUID) finder {
	return finderOf(s, ctx, "account", func(number string) (any, error) {
		return s.API.ListAccounts(ctx, gen.ListAccountsParams{LegalEntityID: gen.NewOptUUID(company), Number: gen.NewOptString(number)})
	}, func(p *gen.AccountPage) ([]gen.Account, bool) { return p.Items, false }, func(a gen.Account) uuid.UUID { return a.ID })
}

// filtered is a list's page size and filter: the first page with no
// code, else the code's.
func filtered(code string) (gen.OptInt32, gen.OptString) {
	if code == "" {
		return gen.NewOptInt32(maxCenters), gen.OptString{}
	}
	return gen.OptInt32{}, gen.NewOptString(code)
}

func (s *Server) costCenters(ctx context.Context, company uuid.UUID) (book, finder) {
	list := func(code string) (any, error) {
		size, c := filtered(code)
		return s.API.ListCostCenters(ctx, gen.ListCostCentersParams{LegalEntityID: gen.NewOptUUID(company), PageSize: size, Code: c})
	}
	return codesOf(s, ctx, "cost center", list, costCenterItems, func(c gen.CostCenter) string { return c.Code },
		func(c gen.CostCenter) uuid.UUID { return c.ID })
}

func costCenterItems(p *gen.CostCenterPage) ([]gen.CostCenter, bool) {
	return p.Items, p.NextPageToken.Set
}

func (s *Server) profitCenters(ctx context.Context, company uuid.UUID) (book, finder) {
	list := func(code string) (any, error) {
		size, c := filtered(code)
		return s.API.ListProfitCenters(ctx, gen.ListProfitCentersParams{LegalEntityID: gen.NewOptUUID(company), PageSize: size, Code: c})
	}
	return codesOf(s, ctx, "profit center", list, profitCenterItems, profitCenterCode, profitCenterID)
}

func profitCenterItems(p *gen.ProfitCenterPage) ([]gen.ProfitCenter, bool) {
	return p.Items, p.NextPageToken.Set
}

func profitCenterCode(c gen.ProfitCenter) string { return c.Code }

func profitCenterID(c gen.ProfitCenter) uuid.UUID { return c.ID }

func (s *Server) segments(ctx context.Context) (book, finder) {
	return codesOf(s, ctx, "segment", func(code string) (any, error) {
		size, c := filtered(code)
		return s.API.ListSegments(ctx, gen.ListSegmentsParams{PageSize: size, Code: c})
	}, func(p *gen.SegmentPage) ([]gen.Segment, bool) {
		return p.Items, p.NextPageToken.Set
	},
		func(c gen.Segment) string { return c.Code }, func(c gen.Segment) uuid.UUID { return c.ID })
}

func (s *Server) partnerFinder(ctx context.Context) finder {
	return finderOf(s, ctx, "partner", func(number string) (any, error) {
		return s.API.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{Number: gen.NewOptString(number)})
	}, func(p *gen.BusinessPartnerPage) ([]gen.BusinessPartner, bool) { return p.Items, false },
		func(b gen.BusinessPartner) uuid.UUID { return b.ID })
}

// lineKind is one kind of code a line names: its field, how the lines'
// codes of it are found, the line's code of it and where its id goes.
type lineKind struct {
	field string
	find  func([]string) map[string]found
	code  func(entryLine) string
	set   func(*gen.JournalLineInput, uuid.UUID)
}

// named are the codes of the kind the lines name.
func named(lines []entryLine, code func(entryLine) string) []string {
	var out []string
	for _, l := range lines {
		if c := code(l); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// resolve looks up the codes the lines name, in the company and its
// chart (a kind's page read only when a line names one), and sets each
// line's ids, or says on it what is not known. It reports whether every
// line can be sent.
func (s *Server) resolve(ctx context.Context, company uuid.UUID, ch chart, lines []entryLine) bool {
	lk := &lookups{left: maxCodeLookups}
	paged := func(read func() (book, finder), unknown i18n.Key) func([]string) map[string]found {
		return func(codes []string) map[string]found {
			if len(codes) == 0 {
				return nil
			}
			b, f := read()
			return lk.lookUp(codes, b, unknown, f)
		}
	}
	kinds := []lineKind{
		{ui.FieldLineAccount, func(codes []string) map[string]found {
			return lk.lookUp(codes, book{ids: ch.byNumber, more: ch.more}, i18n.EntryAccountUnknown, s.accountFinder(ctx, company))
		}, func(l entryLine) string { return l.account }, func(in *gen.JournalLineInput, id uuid.UUID) { in.AccountID = id }},
		{ui.FieldLineCostCenter, paged(func() (book, finder) { return s.costCenters(ctx, company) }, i18n.EntryCostCenterUnknown),
			func(l entryLine) string { return l.costCenter }, func(in *gen.JournalLineInput, id uuid.UUID) { in.CostCenterID = gen.NewOptUUID(id) }},
		{ui.FieldLineProfit, paged(func() (book, finder) { return s.profitCenters(ctx, company) }, i18n.EntryProfitCenterUnknown),
			func(l entryLine) string { return l.profitCenter }, func(in *gen.JournalLineInput, id uuid.UUID) { in.ProfitCenterID = gen.NewOptUUID(id) }},
		{ui.FieldLineSegment, paged(func() (book, finder) { return s.segments(ctx) }, i18n.EntrySegmentUnknown),
			func(l entryLine) string { return l.segment }, func(in *gen.JournalLineInput, id uuid.UUID) { in.SegmentID = gen.NewOptUUID(id) }},
		{ui.FieldLinePartner, func(codes []string) map[string]found {
			return lk.lookUp(codes, book{more: true}, i18n.EntryPartnerUnknown, s.partnerFinder(ctx))
		}, func(l entryLine) string { return l.partner }, func(in *gen.JournalLineInput, id uuid.UUID) { in.PartnerID = gen.NewOptUUID(id) }},
	}
	ok := true
	for _, k := range kinds {
		got := k.find(named(lines, k.code))
		for i := range lines {
			if code := k.code(lines[i]); code != "" {
				if f := got[code]; f.err != "" {
					lines[i].fail(k.field, f.err)
				} else {
					k.set(&lines[i].sent, f.id)
				}
			}
		}
	}
	for _, l := range lines {
		ok = ok && len(l.errs) == 0
	}
	return ok
}

// chart is a company's chart of accounts as a page of listAccounts holds
// it: its accounts by id and by number, and whether it holds more.
type chart struct {
	byID     map[uuid.UUID]gen.Account
	byNumber map[string]uuid.UUID
	more     bool
}

// chartOf reads the company's accounts (listAccounts), a page of the
// API's largest; none without the permission to list them. A call that
// fails leaves them unread, logged: an account's number is no reason to
// refuse a page, and a line's is then looked up by its number.
func (s *Server) chartOf(ctx context.Context, m me, company uuid.UUID) chart {
	c := chart{byID: map[uuid.UUID]gen.Account{}, byNumber: map[string]uuid.UUID{}}
	if !m.may(permReadAccounts) {
		return c
	}
	res, err := s.API.ListAccounts(ctx, gen.ListAccountsParams{LegalEntityID: gen.NewOptUUID(company), PageSize: gen.NewOptInt32(maxAccounts)})
	page, ok := res.(*gen.AccountPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the accounts could not be read", "answer", res, "error", err)
		c.more = true
		return c
	}
	for _, a := range page.Items {
		c.byID[a.ID], c.byNumber[a.Number] = a, a.ID
	}
	c.more = page.NextPageToken.Set
	return c
}

// names are the chart's accounts by number and name, by id.
func (c chart) names() map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(c.byID))
	for id, a := range c.byID {
		out[id] = a.Number + " " + a.Name
	}
	return out
}

// options are the chart's accounts as the form suggests them: by
// number, with their names.
func (c chart) options() []ui.Option {
	out := make([]ui.Option, 0, len(c.byID))
	for _, a := range c.byID {
		out = append(out, ui.Option{Value: a.Number, Text: a.Number + " " + a.Name})
	}
	sortOptions(out)
	return out
}
