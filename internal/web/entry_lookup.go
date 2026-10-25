package web

import (
	"context"
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
	centerCode = regexp.MustCompile(`^[0-9A-Z_-]{1,10}$`)
	taxCode    = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)
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
	taxCode, text                  string
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
	return strings.TrimSpace(l.account+l.amount+l.costCenter+l.profitCenter+l.segment+l.partner+l.taxCode+l.text) == ""
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
// not zero, the codes the API's shapes, the text at most 200 characters.
// The codes are taken in capitals, as the API keeps them.
func (l *entryLine) check(loc i18n.Locale, currency string) {
	l.account, l.partner = strings.TrimSpace(l.account), strings.TrimSpace(l.partner)
	for _, code := range []*string{&l.costCenter, &l.profitCenter, &l.segment, &l.taxCode} {
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
}

// textError is what is wrong with a text of at most max characters.
func textError(text string, max int) i18n.Key {
	if utf8.RuneCountInString(text) > max {
		return i18n.EntryTextTooLong
	}
	return ""
}

// found is what a lookup says of a code: its id, or why it has none (not
// known, or the lookup failed).
type found struct {
	id  uuid.UUID
	err i18n.Key
}

// finder looks one code up: found false when the API knows none, ok false
// when the call failed (logged).
type finder func(code string) (id uuid.UUID, known, ok bool)

// lookUp looks each distinct code up once, those not known already.
func lookUp(codes []string, known map[string]uuid.UUID, unknown i18n.Key, find finder) map[string]found {
	out := map[string]found{}
	for _, code := range codes {
		if _, done := out[code]; done || code == "" {
			continue
		}
		if id, ok := known[code]; ok {
			out[code] = found{id: id}
			continue
		}
		// A line as the API gave it names a partner, or an account its
		// chart's page does not hold, by its id.
		if id, err := uuid.Parse(code); err == nil {
			out[code] = found{id: id}
			continue
		}
		if find == nil {
			out[code] = found{err: unknown}
			continue
		}
		id, isKnown, ok := find(code)
		switch {
		case !ok:
			out[code] = found{err: i18n.EntryLookupFailed}
		case !isKnown:
			out[code] = found{err: unknown}
		default:
			out[code] = found{id: id}
		}
	}
	return out
}

// finderOf is a finder by one of the API's lists filtered by the code:
// call lists, items reads its page's items and id an item's id.
func finderOf[P any, I any](s *Server, ctx context.Context, what string, call func(string) (any, error), items func(P) []I, id func(I) uuid.UUID) finder {
	return func(code string) (uuid.UUID, bool, bool) {
		res, err := call(code)
		page, ok := res.(P)
		if !ok {
			s.Log.WarnContext(ctx, "web: a line's code could not be looked up", "what", what, "answer", res, "error", err)
			return uuid.Nil, false, false
		}
		if all := items(page); len(all) > 0 {
			return id(all[0]), true, true
		}
		return uuid.Nil, false, true
	}
}

// The finders of a company's codes: an account by number, a cost or
// profit center and a segment by code, a partner by number.
func (s *Server) accountFinder(ctx context.Context, company uuid.UUID) finder {
	return finderOf(s, ctx, "account", func(number string) (any, error) {
		return s.API.ListAccounts(ctx, gen.ListAccountsParams{LegalEntityID: gen.NewOptUUID(company), Number: gen.NewOptString(number)})
	}, func(p *gen.AccountPageHeaders) []gen.Account { return p.Response.Items }, func(a gen.Account) uuid.UUID { return a.ID })
}

func (s *Server) costCenterFinder(ctx context.Context, company uuid.UUID) finder {
	return finderOf(s, ctx, "cost center", func(code string) (any, error) {
		return s.API.ListCostCenters(ctx, gen.ListCostCentersParams{LegalEntityID: gen.NewOptUUID(company), Code: gen.NewOptString(code)})
	}, func(p *gen.CostCenterPageHeaders) []gen.CostCenter { return p.Response.Items }, func(c gen.CostCenter) uuid.UUID { return c.ID })
}

func (s *Server) profitCenterFinder(ctx context.Context, company uuid.UUID) finder {
	return finderOf(s, ctx, "profit center", func(code string) (any, error) {
		return s.API.ListProfitCenters(ctx, gen.ListProfitCentersParams{LegalEntityID: gen.NewOptUUID(company), Code: gen.NewOptString(code)})
	}, func(p *gen.ProfitCenterPageHeaders) []gen.ProfitCenter { return p.Response.Items }, func(c gen.ProfitCenter) uuid.UUID { return c.ID })
}

func (s *Server) segmentFinder(ctx context.Context) finder {
	return finderOf(s, ctx, "segment", func(code string) (any, error) {
		return s.API.ListSegments(ctx, gen.ListSegmentsParams{Code: gen.NewOptString(code)})
	}, func(p *gen.SegmentPageHeaders) []gen.Segment { return p.Response.Items }, func(c gen.Segment) uuid.UUID { return c.ID })
}

func (s *Server) partnerFinder(ctx context.Context) finder {
	return finderOf(s, ctx, "partner", func(number string) (any, error) {
		return s.API.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{Number: gen.NewOptString(number)})
	}, func(p *gen.BusinessPartnerPageHeaders) []gen.BusinessPartner { return p.Response.Items }, func(b gen.BusinessPartner) uuid.UUID { return b.ID })
}

// resolve looks up the codes the lines name, in the company and its
// chart, and sets each line's ids, or says on it what is not known. It
// reports whether every line can be sent.
func (s *Server) resolve(ctx context.Context, company uuid.UUID, ch chart, lines []entryLine) bool {
	var accounts, costs, profits, segments, partners []string
	for _, l := range lines {
		accounts, costs, profits = append(accounts, l.account), append(costs, l.costCenter), append(profits, l.profitCenter)
		segments, partners = append(segments, l.segment), append(partners, l.partner)
	}
	var accountFind finder
	if ch.more {
		accountFind = s.accountFinder(ctx, company)
	}
	looked := []struct {
		field string
		found map[string]found
		code  func(entryLine) string
		set   func(*gen.JournalLineInput, uuid.UUID)
	}{
		{ui.FieldLineAccount, lookUp(accounts, ch.byNumber, i18n.EntryAccountUnknown, accountFind), func(l entryLine) string { return l.account },
			func(in *gen.JournalLineInput, id uuid.UUID) { in.AccountID = id }},
		{ui.FieldLineCostCenter, lookUp(costs, nil, i18n.EntryCostCenterUnknown, s.costCenterFinder(ctx, company)),
			func(l entryLine) string { return l.costCenter }, func(in *gen.JournalLineInput, id uuid.UUID) { in.CostCenterID = gen.NewOptUUID(id) }},
		{ui.FieldLineProfit, lookUp(profits, nil, i18n.EntryProfitCenterUnknown, s.profitCenterFinder(ctx, company)),
			func(l entryLine) string { return l.profitCenter }, func(in *gen.JournalLineInput, id uuid.UUID) { in.ProfitCenterID = gen.NewOptUUID(id) }},
		{ui.FieldLineSegment, lookUp(segments, nil, i18n.EntrySegmentUnknown, s.segmentFinder(ctx)),
			func(l entryLine) string { return l.segment }, func(in *gen.JournalLineInput, id uuid.UUID) { in.SegmentID = gen.NewOptUUID(id) }},
		{ui.FieldLinePartner, lookUp(partners, nil, i18n.EntryPartnerUnknown, s.partnerFinder(ctx)),
			func(l entryLine) string { return l.partner }, func(in *gen.JournalLineInput, id uuid.UUID) { in.PartnerID = gen.NewOptUUID(id) }},
	}
	ok := true
	for i := range lines {
		l := &lines[i]
		for _, k := range looked {
			code := k.code(*l)
			if code == "" || l.errs[k.field] != "" {
				continue
			}
			if f := k.found[code]; f.err != "" {
				l.fail(k.field, f.err)
			} else {
				k.set(&l.sent, f.id)
			}
		}
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
	page, ok := res.(*gen.AccountPageHeaders)
	if !ok {
		s.Log.WarnContext(ctx, "web: the accounts could not be read", "answer", res, "error", err)
		c.more = true
		return c
	}
	for _, a := range page.Response.Items {
		c.byID[a.ID], c.byNumber[a.Number] = a, a.ID
	}
	c.more = page.Response.NextPageToken.Set
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
