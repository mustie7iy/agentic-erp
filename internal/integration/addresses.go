package integration

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// vendorAddress is the legal address a vendor master's street, city,
// postal code and region give its supplier (LFA1 STRAS, ORT01, PSTLZ,
// REGIO, pending the SAP specialist; ADR-0054 As built), in its country
// (LAND1), valid from today, the day it is migrated. ok is false when the
// extract holds no address for it, and why is then set when it holds one
// the platform cannot keep: a street without a city or a city without a
// street, which partner.address requires both of, or a text longer than a
// partner address's (the contract's Address). Such a vendor loads without
// an address, and the quality gate warns of it; nothing fails the run.
func vendorAddress(v sap.LFA1, today time.Time) (a partner.AddressInput, ok bool, why string) {
	street, city := strings.TrimSpace(v.STRAS), strings.TrimSpace(v.ORT01)
	postal, region := strings.TrimSpace(v.PSTLZ), strings.TrimSpace(v.REGIO)
	switch {
	case street == "" && city == "":
		if postal != "" || region != "" {
			return a, false, "a postal code or region but no street or city"
		}
		return a, false, ""
	case street == "":
		return a, false, "a city but no street (STRAS)"
	case city == "":
		return a, false, "a street but no city (ORT01)"
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"STRAS", street, 200}, {"ORT01", city, 100}, {"PSTLZ", postal, 20}, {"REGIO", region, 100}} {
		if utf8.RuneCountInString(f.value) > f.max {
			return a, false, f.name + " longer than an address holds"
		}
	}
	a = partner.AddressInput{Purpose: "legal", Line1: street, City: city, CountryCode: v.LAND1, ValidFrom: partner.Date{Time: today}}
	if postal != "" {
		a.PostalCode = &postal
	}
	if region != "" {
		a.Region = &region
	}
	return a, true, ""
}
