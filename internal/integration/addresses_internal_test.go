package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestCentralAddress: of a vendor's central address (ADRC by LFA1-ADRNR)
// the load takes the international row valid on the cutover date, and
// leaves out with a reason an address it cannot keep (po:2026-11-09, A1).
func TestCentralAddress(t *testing.T) {
	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	valid := sap.ADRC{ADDRNUMBER: "42", STREET: "Hauptstrasse", HOUSE_NUM1: "5", CITY1: "Berlin"}
	with := func(change func(r *sap.ADRC)) sap.ADRC {
		r := valid
		change(&r)
		return r
	}
	for _, c := range []struct {
		name      string
		adrnr     string
		rows      []sap.ADRC
		wantLine1 string // "" when none is taken
		wantWhy   string
	}{
		{"no address number", "", []sap.ADRC{valid}, "", ""},
		{"the one valid row", "42", []sap.ADRC{valid}, "Hauptstrasse 5", ""},
		{"initial dates open-ended", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.DATE_FROM, r.DATE_TO = "00000000", "00000000" })}, "Hauptstrasse 5", ""},
		{"valid from the day and to the day", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.DATE_FROM, r.DATE_TO = "20261109", "20261109" })}, "Hauptstrasse 5", ""},
		{"another script, an expired and a future row left out", "42", []sap.ADRC{
			with(func(r *sap.ADRC) { r.NATION, r.STREET = "I", "Other script" }),
			with(func(r *sap.ADRC) { r.DATE_TO, r.STREET = "20261108", "Old street" }),
			with(func(r *sap.ADRC) { r.DATE_FROM, r.STREET = "20261110", "New street" }),
			valid}, "Hauptstrasse 5", ""},
		{"another address's row", "43", []sap.ADRC{valid}, "", "no central address (ADRC 43) valid on 2026-11-09"},
		{"two valid rows", "42", []sap.ADRC{valid, valid}, "", "2 central addresses (ADRC 42) valid on 2026-11-09"},
		{"neither street nor city", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.STREET, r.HOUSE_NUM1, r.CITY1 = "", "", "" })}, "",
			"a central address without a street or city"},
		{"a house number without a street", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.STREET = "" })}, "", "a city but no street (ADRC STREET)"},
		{"a city without a street", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.STREET, r.HOUSE_NUM1 = "", "" })}, "", "a city but no street (ADRC STREET)"},
		{"a street without a city", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.CITY1 = "" })}, "", "a street but no city (ADRC CITY1)"},
		{"a street too long", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.STREET = strings.Repeat("s", 199) })}, "",
			"the street (ADRC STREET, HOUSE_NUM1, HOUSE_NUM2) longer than an address holds"},
		{"further lines too long", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.STR_SUPPL1, r.LOCATION = strings.Repeat("s", 100), strings.Repeat("l", 99) })}, "",
			"the street's further lines (ADRC STR_SUPPL1-3, LOCATION) longer than an address holds"},
		{"a postal code too long", "42", []sap.ADRC{with(func(r *sap.ADRC) { r.POST_CODE1 = strings.Repeat("9", 21) })}, "", "ADRC POST_CODE1 longer than an address holds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := &sap.Extract{ADRC: c.rows}
			a, ok, why := vendorAddress(sap.LFA1{LIFNR: "1", LAND1: "DE", ADRNR: c.adrnr}, addressesOf(x), []time.Time{day}, day)
			if ok != (c.wantLine1 != "") {
				t.Fatalf("address taken: got %v, want %v (why %q)", ok, c.wantLine1 != "", why)
			}
			if a.Line1 != c.wantLine1 {
				t.Errorf("line 1: got %q, want %q", a.Line1, c.wantLine1)
			}
			if why != c.wantWhy {
				t.Errorf("why: got %q, want %q", why, c.wantWhy)
			}
		})
	}
}
