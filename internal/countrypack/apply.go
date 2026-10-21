package countrypack

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// Options say where and when a pack is applied.
type Options struct {
	// Company is the company code the pack is applied to; it must be in the
	// document already and in the pack's country.
	Company string
	// On is the date of application, recorded in the packs section.
	On time.Time
	// FiscalYear is the year the pack's number ranges are created for.
	FiscalYear int
}

// ErrDowngrade refuses applying a version older than the one adopted.
var ErrDowngrade = errors.New("countrypack: the company adopted a newer version")

// ErrNotApplicable refuses applying a pack to a company it is not for: one
// not in the document, in another country, or that adopted another
// country's pack.
var ErrNotApplicable = errors.New("countrypack: the pack does not apply to the company")

// Apply merges next into a copy of cur for one company and returns the
// draft document, with notes for the reviewers, for an ordinary change
// request (ADR-0048). The merge is three-way: the base is the version the
// document's packs section says the company adopted (from lib), empty on
// first adoption. cur is not changed.
func Apply(cur *config.Config, next *Pack, lib *Library, o Options) (*config.Config, []config.Note, error) {
	if o.On.IsZero() || o.FiscalYear < 1900 {
		return nil, nil, fmt.Errorf("countrypack: the date of application and the fiscal year are required")
	}
	draft := *cur
	draft.Companies = slices.Clone(cur.Companies)
	draft.Packs = slices.Clone(cur.Packs)
	draft.Calendars = slices.Clone(cur.Calendars)
	co := draft.Company(o.Company)
	switch {
	case co == nil:
		return nil, nil, fmt.Errorf("%w: company %q is not in the document", ErrNotApplicable, o.Company)
	case co.Country != next.Manifest.Country:
		return nil, nil, fmt.Errorf("%w: company %s is in %q, the pack is for %s", ErrNotApplicable, o.Company, co.Country, next.Manifest.Country)
	}
	base, err := adopted(cur, next, lib, o.Company)
	if err != nil {
		return nil, nil, err
	}
	m := &merger{}
	p := "companies[" + o.Company + "]"
	co.ChartOfAccounts = merge3(m, p+".chart_of_accounts", base.Accounts.Accounts, co.ChartOfAccounts, next.Accounts.Accounts,
		func(a config.Account) string { return a.Number })
	mergeDetermination(m, p+".account_determination", &base.Accounts.AccountDetermination, &co.AccountDetermination, &next.Accounts.AccountDetermination)
	// A pack's codes carry all their dated rates (T61a), so they are merged
	// as they are, whatever the date of application.
	co.TaxCodes = merge3(m, p+".tax_codes", base.Tax.TaxCodes, co.TaxCodes, next.Tax.TaxCodes, func(t config.TaxCode) string { return t.Code })
	draft.NumberRanges = merge3(m, "number_ranges", adoptedRanges(base, cur, o), cur.NumberRanges, numberRanges(next, o),
		func(n config.NumberRange) string { return n.Key() })
	draft.NumberRanges = issuedStay(m, cur.NumberRanges, draft.NumberRanges)
	draft.PaymentTerms = merge3(m, "payment_terms", base.Payments.PaymentTerms, cur.PaymentTerms, next.Payments.PaymentTerms,
		func(t config.PaymentTerm) string { return t.Key })
	mergeCalendar(m, &draft, base, next, o.Company)
	adopt(&draft, next, o)
	m.note("packs["+o.Company+"]", "applies the %s pack %s (%s) on %s", next.Manifest.Country, next.Manifest.Version, baseText(base), o.On.Format(time.DateOnly))
	if !next.SignedOff() {
		m.note("packs["+o.Company+"]", "no tax specialist has signed this pack off; it may not be used in the pilot")
	}
	return &draft, m.notes, nil
}

// adopted is the base of the merge, the version the company adopted; an
// empty pack on first adoption.
func adopted(cur *config.Config, next *Pack, lib *Library, company string) (*Pack, error) {
	a := cur.Pack(company)
	if a == nil {
		return &Pack{}, nil
	}
	if a.Country != next.Manifest.Country {
		return nil, fmt.Errorf("%w: company %s adopted the %s pack, not %s", ErrNotApplicable, company, a.Country, next.Manifest.Country)
	}
	if CompareVersions(next.Manifest.Version, a.Version) < 0 {
		return nil, fmt.Errorf("%w: %s %s, not %s", ErrDowngrade, a.Country, a.Version, next.Manifest.Version)
	}
	base := lib.Get(a.Country, a.Version)
	if base == nil {
		return nil, fmt.Errorf("countrypack: the adopted pack %s %s is not in the image", a.Country, a.Version)
	}
	return base, nil
}

func baseText(base *Pack) string {
	if base.Manifest.Version == "" {
		return "first adoption"
	}
	return "upgrading from " + base.Manifest.Version
}

// adopt records the adoption in the document's packs section.
func adopt(c *config.Config, next *Pack, o Options) {
	a := config.PackAdoption{Company: o.Company, Country: next.Manifest.Country, Version: next.Manifest.Version, AppliedOn: o.On.Format(time.DateOnly)}
	if prev := c.Pack(o.Company); prev != nil {
		*prev = a
		return
	}
	c.Packs = append(c.Packs, a)
}

// numberRanges are the pack's templates for the company and fiscal year.
func numberRanges(p *Pack, o Options) []config.NumberRange {
	var out []config.NumberRange
	for _, r := range p.Numbering.NumberRanges {
		out = append(out, config.NumberRange{Company: o.Company, DocumentType: r.DocumentType, FiscalYear: o.FiscalYear,
			Prefix: r.Prefix, First: r.First, Last: r.Last, Gapless: r.Gapless})
	}
	return out
}

// adoptedRanges are the base's ranges the document has: number ranges are
// per fiscal year, so a year the document has no ranges for yet is not a
// removal there but a year the adopted version never reached, and the new
// version's ranges are created for it.
func adoptedRanges(base *Pack, cur *config.Config, o Options) []config.NumberRange {
	have := index(cur.NumberRanges, func(n config.NumberRange) string { return n.Key() })
	var out []config.NumberRange
	for _, n := range numberRanges(base, o) {
		if _, ok := have[n.Key()]; ok {
			out = append(out, n)
		}
	}
	return out
}

// issuedStay keeps a range the document already has where the merge
// would change more than raise its last number: numbers issued from it must
// stay valid, so submission refuses any other change (config §3.5).
func issuedStay(m *merger, cur, merged []config.NumberRange) []config.NumberRange {
	had := index(cur, func(n config.NumberRange) string { return n.Key() })
	for i, n := range merged {
		old, ok := had[n.Key()]
		raised := old
		raised.Last = n.Last
		if ok && n != old && (n != raised || n.Last < old.Last) {
			m.note("number_ranges["+n.Key()+"]", "the pack changes more than the upper limit of this range, which issued numbers forbid; kept, the pack has %+v", n)
			merged[i] = old
		}
	}
	return merged
}

// mergeCalendar merges the pack's holidays into the company's calendar by
// date, in date order; a company without a calendar gets one when the
// merge leaves it any holidays.
func mergeCalendar(m *merger, c *config.Config, base, next *Pack, company string) {
	var tenant []config.Holiday
	cal := c.Calendar(company)
	if cal != nil {
		tenant = cal.Holidays
	}
	merged := merge3(m, "calendars["+company+"].holidays", base.Calendar.Holidays, tenant, next.Calendar.Holidays,
		func(h config.Holiday) string { return h.Date })
	slices.SortStableFunc(merged, func(a, b config.Holiday) int { return strings.Compare(a.Date, b.Date) })
	switch {
	case cal != nil:
		cal.Holidays = merged
	case len(merged) > 0:
		c.Calendars = append(c.Calendars, config.Calendar{Company: company, Holidays: merged})
	}
}

// field is one account determination setting, merged like a list item.
type field struct{ Name, Value string }

// mergeDetermination merges the account determination field by field.
func mergeDetermination(m *merger, p string, base, tenant, next *config.AccountDetermination) {
	list := func(ad *config.AccountDetermination) []field {
		var out []field
		fs := determination(ad)
		for _, name := range slices.Sorted(maps.Keys(fs)) {
			if v := *fs[name]; v != "" {
				out = append(out, field{name, v})
			}
		}
		return out
	}
	merged := merge3(m, p, list(base), list(tenant), list(next), func(f field) string { return f.Name })
	*tenant = config.AccountDetermination{}
	fs := determination(tenant)
	for _, f := range merged {
		*fs[f.Name] = f.Value
	}
}

type merger struct{ notes []config.Note }

func (m *merger) note(path, format string, a ...any) {
	m.notes = append(m.notes, config.Note{Path: path, Message: fmt.Sprintf(format, a...)})
}

// merge3 merges a keyed list: base is the adopted pack's, tenant the
// document's, next the new pack's. A change only in the pack is taken, a
// change only in the document kept; where both changed it differently the
// document's is kept with a conflict note, and an item the pack dropped is
// kept with a note. Items the pack never had are left alone. The result
// keeps the document's order, then the pack's new items in its order.
func merge3[T any](m *merger, p string, base, tenant, next []T, key func(T) string) []T {
	bm, nm, tm := index(base, key), index(next, key), index(tenant, key)
	var out []T
	for _, t := range tenant {
		k := key(t)
		b, inBase := bm[k]
		n, inNext := nm[k]
		switch {
		case !inNext:
			if inBase {
				m.note(p+"["+k+"]", "the new pack version no longer has this; kept")
			}
			out = append(out, t)
		case reflect.DeepEqual(t, n):
			out = append(out, t)
		case inBase && reflect.DeepEqual(t, b):
			out = append(out, n) // changed only in the pack
		case inBase && reflect.DeepEqual(b, n):
			out = append(out, t) // changed only here
		case inBase:
			m.note(p+"["+k+"]", "conflict: changed both here and in the pack; kept as here, the pack has %+v", n)
			out = append(out, t)
		default:
			m.note(p+"["+k+"]", "conflict: already here and different in the pack; kept as here, the pack has %+v", n)
			out = append(out, t)
		}
	}
	for _, n := range next {
		k := key(n)
		if _, here := tm[k]; here {
			continue
		}
		switch b, inBase := bm[k]; {
		case !inBase:
			out = append(out, n) // new in the pack
		case !reflect.DeepEqual(b, n):
			m.note(p+"["+k+"]", "conflict: removed here and changed in the pack; left out, the pack has %+v", n)
		}
		// removed here and unchanged in the pack: stays removed
	}
	return out
}

func index[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, it := range items {
		out[key(it)] = it
	}
	return out
}
