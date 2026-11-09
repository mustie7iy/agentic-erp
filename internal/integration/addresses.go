package integration

import (
	"cmp"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// addresses are an extract's central addresses (ADRC) by ADDRNUMBER.
type addresses map[string][]sap.ADRC

func addressesOf(e *sap.Extract) addresses {
	out := addresses{}
	for _, a := range e.ADRC {
		out[a.ADDRNUMBER] = append(out[a.ADDRNUMBER], a)
	}
	return out
}

// current is the central address a vendor master names (LFA1-ADRNR), in
// its international version (NATION blank) valid on day, the cutover date
// (DATE_FROM to DATE_TO, both included, an initial one open-ended;
// po:2026-11-09, A1). ok is false
// when there is none, and why is then set when the vendor names an
// address the extract holds no such row of, or several.
func (as addresses) current(v sap.LFA1, day time.Time) (a sap.ADRC, ok bool, why string) {
	if v.ADRNR == "" {
		return a, false, ""
	}
	on := day.Format("20060102")
	var found []sap.ADRC
	for _, r := range as[v.ADRNR] {
		if r.NATION == "" && sapDay(r.DATE_FROM, "00010101") <= on && on <= sapDay(r.DATE_TO, "99991231") {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return a, false, fmt.Sprintf("no central address (ADRC %s) valid on %s", v.ADRNR, day.Format(time.DateOnly))
	case 1:
		return found[0], true, ""
	}
	return a, false, fmt.Sprintf("%d central addresses (ADRC %s) valid on %s", len(found), v.ADRNR, day.Format(time.DateOnly))
}

// sapDay is an SAP date as YYYYMMDD, or open when it is initial (blank
// or 00000000, SAP's initial date).
func sapDay(d, open string) string {
	if strings.Trim(d, "0 ") == "" {
		return open
	}
	return d
}

// vendorAddress is the legal address a vendor master's central address
// gives its supplier (ADRC through LFA1-ADRNR, po:2026-11-09, A1; ADR-0054
// As built), valid from day, the day it is migrated. Line 1 is the street
// followed by the house number and its supplement (STREET, HOUSE_NUM1,
// HOUSE_NUM2), line 2 the street's further lines (STR_SUPPL1-3,
// LOCATION); the country is ADRC's, the vendor's (LAND1) when it has
// none. ok is false when the extract holds no address for it, and why is
// then set when it holds one the platform cannot keep: none or several
// valid on the day, a street without a city or a city without a street,
// which partner.address requires both of, or a text longer than a partner
// address's (the contract's Address). Such a vendor loads without an
// address, and the quality gate warns of it; nothing fails the run. A
// blank house number is normal: many systems keep it in the street.
func vendorAddress(v sap.LFA1, as addresses, day time.Time) (a partner.AddressInput, ok bool, why string) {
	r, ok, why := as.current(v, day)
	if !ok {
		return a, false, why
	}
	street := joined(" ", r.STREET, r.HOUSE_NUM1, r.HOUSE_NUM2)
	more := joined(", ", r.STR_SUPPL1, r.STR_SUPPL2, r.STR_SUPPL3, r.LOCATION)
	city, postal, region := strings.TrimSpace(r.CITY1), strings.TrimSpace(r.POST_CODE1), strings.TrimSpace(r.REGION)
	switch named := strings.TrimSpace(r.STREET) != ""; {
	case !named && city == "":
		return a, false, "a central address without a street or city"
	case !named:
		return a, false, "a city but no street (ADRC STREET)"
	case city == "":
		return a, false, "a street but no city (ADRC CITY1)"
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"the street (ADRC STREET, HOUSE_NUM1, HOUSE_NUM2)", street, 200}, {"the street's further lines (ADRC STR_SUPPL1-3, LOCATION)", more, 200},
		{"ADRC CITY1", city, 100}, {"ADRC POST_CODE1", postal, 20}, {"ADRC REGION", region, 100}} {
		if utf8.RuneCountInString(f.value) > f.max {
			return a, false, f.name + " longer than an address holds"
		}
	}
	a = partner.AddressInput{Purpose: "legal", Line1: street, City: city, CountryCode: cmp.Or(strings.TrimSpace(r.COUNTRY), v.LAND1),
		ValidFrom: partner.Date{Time: day}}
	a.Line2, a.PostalCode, a.Region = optional(more), optional(postal), optional(region)
	return a, true, ""
}

// optional is s, or nil when it is empty.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// joined is the parts that are not blank, trimmed, joined by sep.
func joined(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
