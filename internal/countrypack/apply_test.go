package countrypack

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// item is a keyed list item for the merge table.
type item struct{ K, V string }

func TestMerge3(t *testing.T) {
	tests := []struct {
		name               string
		base, tenant, next []item
		want               []item
		note               string // a note on [k] containing this, or none when empty
	}{
		{"first adoption adds", nil, nil, []item{{"k", "p"}}, []item{{"k", "p"}}, ""},
		{"first adoption, same as here", nil, []item{{"k", "p"}}, []item{{"k", "p"}}, []item{{"k", "p"}}, ""},
		{"first adoption, different here", nil, []item{{"k", "t"}}, []item{{"k", "p"}}, []item{{"k", "t"}}, "already here"},
		{"pack-only change taken", []item{{"k", "b"}}, []item{{"k", "b"}}, []item{{"k", "p"}}, []item{{"k", "p"}}, ""},
		{"tenant-only change kept", []item{{"k", "b"}}, []item{{"k", "t"}}, []item{{"k", "b"}}, []item{{"k", "t"}}, ""},
		{"both changed alike", []item{{"k", "b"}}, []item{{"k", "x"}}, []item{{"k", "x"}}, []item{{"k", "x"}}, ""},
		{"both changed, conflict", []item{{"k", "b"}}, []item{{"k", "t"}}, []item{{"k", "p"}}, []item{{"k", "t"}}, "changed both"},
		{"pack removed, kept", []item{{"k", "b"}}, []item{{"k", "b"}}, nil, []item{{"k", "b"}}, "no longer has"},
		{"pack removed what tenant changed, kept", []item{{"k", "b"}}, []item{{"k", "t"}}, nil, []item{{"k", "t"}}, "no longer has"},
		{"both removed", []item{{"k", "b"}}, nil, nil, nil, ""},
		{"tenant removed, pack unchanged", []item{{"k", "b"}}, nil, []item{{"k", "b"}}, nil, ""},
		{"tenant removed, pack changed", []item{{"k", "b"}}, nil, []item{{"k", "p"}}, nil, "removed here"},
		{"not the pack's", nil, []item{{"k", "t"}}, nil, []item{{"k", "t"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &merger{}
			got := merge3(m, "x", tt.base, tt.tenant, tt.next, func(i item) string { return i.K })
			if !slices.Equal(got, tt.want) {
				t.Errorf("merge: got %v, want %v", got, tt.want)
			}
			switch {
			case tt.note == "" && len(m.notes) != 0:
				t.Errorf("notes: got %v, want none", m.notes)
			case tt.note != "" && (len(m.notes) != 1 || m.notes[0].Path != "x[k]" || !strings.Contains(m.notes[0].Message, tt.note)):
				t.Errorf("notes: got %v, want one on x[k] containing %q", m.notes, tt.note)
			}
		})
	}
}

// The merge keeps the document's order, then adds the pack's new items in
// its order.
func TestMerge3Order(t *testing.T) {
	got := merge3(&merger{}, "x", nil, []item{{"t", "1"}, {"a", "1"}}, []item{{"z", "1"}, {"a", "1"}, {"b", "1"}}, func(i item) string { return i.K })
	if want := []item{{"t", "1"}, {"a", "1"}, {"z", "1"}, {"b", "1"}}; !slices.Equal(got, want) {
		t.Errorf("merge: got %v, want %v", got, want)
	}
}

func testLibrary(t *testing.T) *Library {
	t.Helper()
	lib, err := LoadAll(testFS(t))
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return lib
}

func testDoc() *config.Config {
	return &config.Config{Version: 1,
		Companies:    []config.Company{{Code: "C1", Name: "Test Co", Country: "ZZ", Currency: "EUR"}, {Code: "C2", Country: "DE"}},
		PaymentTerms: []config.PaymentTerm{{Key: "NT14", Description: "14 days net", NetDays: 14}},
	}
}

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // literal dates in tests
	return d
}

func hasNote(notes []config.Note, path, text string) bool {
	return slices.ContainsFunc(notes, func(n config.Note) bool { return n.Path == path && strings.Contains(n.Message, text) })
}

func TestFirstAdoption(t *testing.T) {
	lib := testLibrary(t)
	cur := testDoc()
	got, notes, err := Apply(cur, lib.Get("ZZ", "2026.1"), lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	co := got.Company("C1")
	if n := len(co.ChartOfAccounts); n != 5 {
		t.Errorf("accounts: got %d, want 5", n)
	}
	if ad := co.AccountDetermination; ad.Bank != "1000" || ad.APReconciliation != "2000" || ad.InputTax != "1400" {
		t.Errorf("account determination: got %+v, want bank 1000, AP 2000, input tax 1400", ad)
	}
	// Every rate is written, the future one too (T61a).
	if want := lib.Get("ZZ", "2026.1").Tax.TaxCodes; !reflect.DeepEqual(co.TaxCodes, want) {
		t.Errorf("tax codes: got %+v, want the pack's %+v", co.TaxCodes, want)
	}
	if rates := co.TaxCodes[0].Components[0].Rates; len(rates) != 2 || rates[1].RatePercent != "17.5" {
		t.Errorf("V1's rates: got %+v, want 15%% and 17.5%% from 2027-01-01", rates)
	}
	if n := len(got.NumberRanges); n != 2 || got.NumberRanges[0].Key() != "C1/supplier_invoice/2026" {
		t.Errorf("number ranges: got %+v, want C1's two of 2026", got.NumberRanges)
	}
	if keys := []string{got.PaymentTerms[0].Key, got.PaymentTerms[1].Key}; len(got.PaymentTerms) != 2 || keys[1] != "NET30" {
		t.Errorf("payment terms: got %+v, want NT14 and NET30", got.PaymentTerms)
	}
	if want := []config.Calendar{{Company: "C1", Holidays: []config.Holiday{{Date: "2026-01-01", Name: "New Year's Day"},
		{Date: "2026-12-25", Name: "Christmas Day"}}}}; !reflect.DeepEqual(got.Calendars, want) {
		t.Errorf("calendars: got %+v, want %+v", got.Calendars, want)
	}
	if a := got.Pack("C1"); a == nil || *a != (config.PackAdoption{Company: "C1", Country: "ZZ", Version: "2026.1", AppliedOn: "2026-10-20"}) {
		t.Errorf("provenance: got %+v, want ZZ 2026.1 applied on 2026-10-20", a)
	}
	if !hasNote(notes, "packs[C1]", "first adoption") {
		t.Errorf("notes: got %v, want the adoption", notes)
	}
	if len(cur.Companies[0].ChartOfAccounts) != 0 || cur.Packs != nil || cur.Calendars != nil || len(cur.PaymentTerms) != 1 {
		t.Errorf("the current document changed: %+v", cur)
	}
}

// An upgrade takes what only the pack changed, keeps what only the
// document changed, and notes conflicts and what the pack dropped.
func TestUpgrade(t *testing.T) {
	lib := testLibrary(t)
	doc, _, err := Apply(testDoc(), lib.Get("ZZ", "2026.1"), lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply 2026.1: %v", err)
	}
	co := doc.Company("C1")
	co.ChartOfAccounts[0].Name = "Main bank" // tenant only
	co.ChartOfAccounts[4].Name = "Office"    // the pack renames it too
	got, notes, err := Apply(doc, lib.Get("ZZ", "2026.2"), lib, Options{Company: "C1", On: day("2027-02-01"), FiscalYear: 2027})
	if err != nil {
		t.Fatalf("Apply 2026.2: %v", err)
	}
	co = got.Company("C1")
	checks := []struct {
		name      string
		got, want any
	}{
		{"tenant's rename kept", co.Account("1000").Name, "Main bank"},
		{"conflict kept as the tenant has it", co.Account("6000").Name, "Office"},
		{"new account", co.Account("6100") != nil, true},
		{"new determination field", co.AccountDetermination.CashDiscount, "6000"},
		{"determination kept", co.AccountDetermination.Bank, "1000"},
		{"the pack's new rate taken", co.TaxCodes[0].Components[0].Rates[2].RatePercent, "18"},
		{"dropped tax code kept", co.TaxCodes[1].Code, "V0"},
		{"new tax code", co.TaxCodes[2].Code, "V2"},
		{"ranges of the new year", len(got.NumberRanges), 4},
		{"new year's range from the new version", got.NumberRanges[2].Last, int64(9999999)},
		{"provenance", *got.Pack("C1"), config.PackAdoption{Company: "C1", Country: "ZZ", Version: "2026.2", AppliedOn: "2027-02-01"}},
		{"conflict noted", hasNote(notes, "companies[C1].chart_of_accounts[6000]", "conflict"), true},
		{"drop noted", hasNote(notes, "companies[C1].tax_codes[V0]", "no longer has"), true},
		{"base named", hasNote(notes, "packs[C1]", "upgrading from 2026.1"), true},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}

func TestApplyNotes(t *testing.T) {
	lib := testLibrary(t)
	p := lib.Get("ZZ", "2026.1")
	unsigned := *p
	unsigned.Manifest.SignOff = nil
	_, notes, err := Apply(testDoc(), &unsigned, lib, Options{Company: "C1", On: day("2019-06-01"), FiscalYear: 2019})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasNote(notes, "packs[C1]", "no tax specialist") {
		t.Errorf("notes: got %v, want the missing sign-off", notes)
	}
}

func TestApplyRefuses(t *testing.T) {
	lib := testLibrary(t)
	adopted := func(a config.PackAdoption) *config.Config {
		c := testDoc()
		c.Packs = []config.PackAdoption{a}
		return c
	}
	tests := []struct {
		name          string
		doc           *config.Config
		company       string
		want          string
		notApplicable bool // the error is ErrNotApplicable
	}{
		{"unknown company", testDoc(), "C9", "not in the document", true},
		{"company of another country", testDoc(), "C2", `is in "DE"`, true},
		{"another country's pack adopted", adopted(config.PackAdoption{Company: "C1", Country: "CA", Version: "2026.1", AppliedOn: "2026-01-01"}), "C1", "adopted the CA pack", true},
		{"downgrade", adopted(config.PackAdoption{Company: "C1", Country: "ZZ", Version: "2026.3", AppliedOn: "2026-01-01"}), "C1", ErrDowngrade.Error(), false},
		{"adopted version not in the image", adopted(config.PackAdoption{Company: "C1", Country: "ZZ", Version: "2025.1", AppliedOn: "2026-01-01"}), "C1", "not in the image", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Apply(tt.doc, lib.Get("ZZ", "2026.2"), lib, Options{Company: tt.company, On: day("2026-10-20"), FiscalYear: 2026})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Apply: got %v, want an error containing %q", err, tt.want)
			}
			if got := errors.Is(err, ErrNotApplicable); got != tt.notApplicable {
				t.Errorf("errors.Is(err, ErrNotApplicable): got %v, want %v", got, tt.notApplicable)
			}
		})
	}
	if _, _, err := Apply(testDoc(), lib.Get("ZZ", "2026.2"), lib, Options{Company: "C1"}); err == nil {
		t.Error("Apply without a date and fiscal year: got nil, want an error")
	}
	_, _, err := Apply(adopted(config.PackAdoption{Company: "C1", Country: "ZZ", Version: "2026.3", AppliedOn: "2026-01-01"}), lib.Get("ZZ", "2026.2"), lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if !errors.Is(err, ErrDowngrade) {
		t.Errorf("downgrade: got %v, want ErrDowngrade", err)
	}
}

// An upgrade merges the holidays by date into the company's calendar, in
// date order: the pack's new ones taken, the document's own kept, and what
// the pack dropped or both renamed kept with a note. The current
// document's calendar is not changed.
func TestUpgradeCalendar(t *testing.T) {
	lib := testLibrary(t)
	doc, _, err := Apply(testDoc(), lib.Get("ZZ", "2026.1"), lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply 2026.1: %v", err)
	}
	cal := doc.Calendar("C1")
	cal.Holidays[1].Name = "Xmas"                                                                  // the pack renames it too
	cal.Holidays = append(cal.Holidays, config.Holiday{Date: "2026-06-15", Name: "Founders' Day"}) // the company's own
	before := slices.Clone(cal.Holidays)
	got, notes, err := Apply(doc, lib.Get("ZZ", "2026.2"), lib, Options{Company: "C1", On: day("2027-02-01"), FiscalYear: 2027})
	if err != nil {
		t.Fatalf("Apply 2026.2: %v", err)
	}
	want := []config.Holiday{{Date: "2026-01-01", Name: "New Year's Day"}, {Date: "2026-06-15", Name: "Founders' Day"},
		{Date: "2026-12-25", Name: "Xmas"}, {Date: "2027-01-01", Name: "New Year's Day"}}
	if h := got.Calendar("C1").Holidays; !slices.Equal(h, want) {
		t.Errorf("holidays: got %+v, want %+v", h, want)
	}
	for path, text := range map[string]string{
		"calendars[C1].holidays[2026-01-01]": "no longer has",
		"calendars[C1].holidays[2026-12-25]": "conflict",
	} {
		if !hasNote(notes, path, text) {
			t.Errorf("notes: got %v, want one on %s containing %q", notes, path, text)
		}
	}
	if !slices.Equal(doc.Calendar("C1").Holidays, before) {
		t.Errorf("the current document's calendar changed: got %+v, want %+v", doc.Calendar("C1").Holidays, before)
	}
}

// A pack without holidays gives a company without a calendar none.
func TestApplyWithoutHolidays(t *testing.T) {
	lib := testLibrary(t)
	p := *lib.Get("ZZ", "2026.1")
	p.Calendar = Calendar{}
	got, _, err := Apply(testDoc(), &p, lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Calendars != nil {
		t.Errorf("calendars: got %+v, want none", got.Calendars)
	}
}

// Within the adopted fiscal year a range only the pack changed is taken.
func TestUpgradeSameYear(t *testing.T) {
	lib := testLibrary(t)
	o := Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026}
	doc, _, err := Apply(testDoc(), lib.Get("ZZ", "2026.1"), lib, o)
	if err != nil {
		t.Fatalf("Apply 2026.1: %v", err)
	}
	got, _, err := Apply(doc, lib.Get("ZZ", "2026.2"), lib, o)
	if err != nil {
		t.Fatalf("Apply 2026.2: %v", err)
	}
	if n := len(got.NumberRanges); n != 2 {
		t.Errorf("number ranges: got %d, want 2", n)
	}
	if last := got.NumberRanges[0].Last; last != 9999999 {
		t.Errorf("supplier invoice range: got last %d, want 9999999", last)
	}
}

// A range the document has keeps everything but a raised upper limit.
func TestIssuedRangesStay(t *testing.T) {
	cur := []config.NumberRange{{Company: "C1", DocumentType: "a", FiscalYear: 2026, First: 1, Last: 10}}
	tests := []struct {
		name   string
		merged config.NumberRange
		want   int64 // the resulting Last
		noted  bool
	}{
		{"raised", config.NumberRange{Company: "C1", DocumentType: "a", FiscalYear: 2026, First: 1, Last: 20}, 20, false},
		{"lowered", config.NumberRange{Company: "C1", DocumentType: "a", FiscalYear: 2026, First: 1, Last: 5}, 10, true},
		{"prefix changed", config.NumberRange{Company: "C1", DocumentType: "a", FiscalYear: 2026, Prefix: "X", First: 1, Last: 20}, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &merger{}
			got := issuedStay(m, cur, []config.NumberRange{tt.merged, {Company: "C1", DocumentType: "b", FiscalYear: 2026, First: 1, Last: 9}})
			if got[0].Last != tt.want {
				t.Errorf("last: got %d, want %d", got[0].Last, tt.want)
			}
			if noted := len(m.notes) == 1; noted != tt.noted {
				t.Errorf("noted: got %v, want %v", noted, tt.noted)
			}
		})
	}
}
