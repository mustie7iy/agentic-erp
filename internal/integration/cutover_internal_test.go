package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func day(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }

// TestCompanyDays: a run's day is each of its companies' date in its time
// zone at the run's instant (review:batch63): at 02:00 UTC on 11 March it
// is still the 10th in Toronto and already the 11th in Dubai, as in UTC
// for a company without a zone; a company outside the run has no day, and
// a zone that does not load fails rather than giving UTC's date.
func TestCompanyDays(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000"}, {Code: "2000", TimeZone: "Asia/Dubai"}, {Code: "3000", TimeZone: "America/Toronto"},
		{Code: "4000", TimeZone: "America/Nowhere"}}}
	entities := map[string]tenancy.Entity{"1000": {}, "2000": {}, "3000": {}}
	at := time.Date(2026, 3, 11, 2, 0, 0, 0, time.UTC)
	got, err := companyDays(doc, entities, at)
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]time.Time{"1000": day(11), "2000": day(11), "3000": day(10)} {
		if !got[code].Equal(want) {
			t.Errorf("day of company %s at %s: got %s, want %s", code, at, got[code].Format(time.DateOnly), want.Format(time.DateOnly))
		}
	}
	if len(got) != 3 {
		t.Errorf("days: got %d companies, want the run's 3", len(got))
	}
	entities["4000"] = tenancy.Entity{}
	if _, err := companyDays(doc, entities, at); err == nil || !strings.Contains(err.Error(), `the run's day: company 4000: time zone "America/Nowhere"`) {
		t.Errorf("a zone that does not load: got %v, want the company's zone refused", err)
	}
}

// TestCutoverDays: a vendor is judged on the days of the companies it is
// set up in, each once and in order, and one set up in none on every day
// of the run.
func TestCutoverDays(t *testing.T) {
	tg := target{days: map[string]time.Time{"1000": day(11), "2000": day(11), "3000": day(10)}}
	for name, c := range map[string]struct {
		companies []string
		want      string
	}{
		"one company":                     {[]string{"3000"}, "2026-03-10"},
		"companies on one day":            {[]string{"1000", "2000"}, "2026-03-11"},
		"companies whose days differ":     {[]string{"2000", "3000", "1000"}, "2026-03-10 or 2026-03-11"},
		"a vendor set up in no company":   {nil, "2026-03-10 or 2026-03-11"},
		"a company named twice by a file": {[]string{"3000", "3000"}, "2026-03-10"},
	} {
		t.Run(name, func(t *testing.T) {
			var in []sap.LFB1
			for _, code := range c.companies {
				in = append(in, sap.LFB1{LIFNR: "1", BUKRS: code})
			}
			if got := daysText(tg.cutoverDays(in)); got != c.want {
				t.Errorf("days: got %s, want %s", got, c.want)
			}
		})
	}
}

// TestCentralAddressAcrossDays: where the days of a vendor's companies
// differ, an address valid on both is taken, and one SAP changes between
// them is left out with both days named, since no one day decides; the
// address starts on the platform on the day given, not on a cutover day.
func TestCentralAddressAcrossDays(t *testing.T) {
	row := func(from, to, street string) sap.ADRC {
		return sap.ADRC{ADDRNUMBER: "42", DATE_FROM: from, DATE_TO: to, STREET: street, CITY1: "Bonn"}
	}
	days, validFrom := []time.Time{day(10), day(11)}, day(12)
	for name, c := range map[string]struct {
		rows      []sap.ADRC
		wantLine1 string
		wantWhy   string
	}{
		"valid on both days":      {[]sap.ADRC{row("20260101", "99991231", "Hauptstrasse")}, "Hauptstrasse", ""},
		"changed between them":    {[]sap.ADRC{row("00010101", "20260310", "Old street"), row("20260311", "99991231", "New street")}, "", "2 central addresses (ADRC 42) valid on 2026-03-10 or 2026-03-11"},
		"valid on the later only": {[]sap.ADRC{row("20260311", "99991231", "New street")}, "New street", ""},
		"valid on neither":        {[]sap.ADRC{row("20260312", "99991231", "Later street")}, "", "no central address (ADRC 42) valid on 2026-03-10 or 2026-03-11"},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, why := vendorAddress(sap.LFA1{LIFNR: "1", LAND1: "DE", ADRNR: "42"}, addressesOf(&sap.Extract{ADRC: c.rows}), days, validFrom)
			if a.Line1 != c.wantLine1 {
				t.Errorf("line 1: got %q, want %q", a.Line1, c.wantLine1)
			}
			if why != c.wantWhy {
				t.Errorf("why: got %q, want %q", why, c.wantWhy)
			}
			if c.wantLine1 != "" && !a.ValidFrom.Equal(validFrom) {
				t.Errorf("valid_from: got %s, want %s", a.ValidFrom.Format(time.DateOnly), validFrom.Format(time.DateOnly))
			}
		})
	}
}
