package config

import (
	"strings"
	"testing"
)

func TestNumberRanges(t *testing.T) {
	c := fixture(t)
	good := NumberRange{Company: "1000", DocumentType: "supplier_invoice", FiscalYear: 2026, Prefix: "SI-", First: 1, Last: 999999, Gapless: true}
	c.NumberRanges = []NumberRange{good}
	if fs := Validate(c); HasErrors(fs) {
		t.Fatalf("valid range refused: %v", fs)
	}
	bad := []NumberRange{
		good, // duplicate
		{Company: " ", DocumentType: "Supplier Invoice", FiscalYear: 1800, Prefix: "si", First: 0, Last: -1},
	}
	c.NumberRanges = append(c.NumberRanges, bad...)
	var paths []string
	for _, f := range Validate(c) {
		if strings.HasPrefix(f.Path, "number_ranges") {
			paths = append(paths, f.Path)
		}
	}
	want := "number_ranges[1000/supplier_invoice/2026] number_ranges[ /Supplier Invoice/1800].company number_ranges[ /Supplier Invoice/1800].document_type " +
		"number_ranges[ /Supplier Invoice/1800].fiscal_year number_ranges[ /Supplier Invoice/1800].prefix number_ranges[ /Supplier Invoice/1800].first " +
		"number_ranges[ /Supplier Invoice/1800].last"
	if strings.Join(paths, " ") != want {
		t.Fatalf("findings:\n%s\nwant\n%s", strings.Join(paths, " "), want)
	}

	// Ranges are compared by company, type and year; a new one and a raised
	// limit need no approval.
	base := fixture(t)
	base.NumberRanges = []NumberRange{good}
	next := fixture(t)
	raised := good
	raised.Last = 1999999
	next.NumberRanges = []NumberRange{{Company: "1000", DocumentType: "payment", FiscalYear: 2026, First: 1, Last: 99}, raised}
	changes, _ := Diff(base, next)
	if len(changes) != 2 || changes[0].Path != "number_ranges[1000/payment/2026]" || changes[0].New != "(added)" ||
		changes[1].Path != "number_ranges[1000/supplier_invoice/2026].last" || MaxTier(changes) != TierCosmetic {
		t.Fatalf("diff: %v", changes)
	}
	if got := good.Key(); got != "1000/supplier_invoice/2026" {
		t.Fatal(got)
	}
}
