package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// maple is a valid postal address.
func maple() *PostalAddress {
	return &PostalAddress{Line1: "150 Bay Street", Line2: "Suite 400", City: "Toronto", PostalCode: "M5J 2R8", Region: "ON", CountryCode: "CA"}
}

// TestPostalAddressValidation: an address is optional; one given has a
// first line and a city, texts within a partner address's bounds and a
// two-letter country (ADR-0054 As built).
func TestPostalAddressValidation(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(a *PostalAddress) *PostalAddress
		want   []string // the error paths below companies[1000].postal_address
	}{
		{"none", func(*PostalAddress) *PostalAddress { return nil }, nil},
		{"complete", func(a *PostalAddress) *PostalAddress { return a }, nil},
		{"first line and city only", func(a *PostalAddress) *PostalAddress {
			return &PostalAddress{Line1: a.Line1, City: a.City, CountryCode: a.CountryCode}
		}, nil},
		{"at the bounds", func(a *PostalAddress) *PostalAddress {
			a.Line1, a.Line2, a.City = strings.Repeat("é", 200), strings.Repeat("x", 200), strings.Repeat("ق", 100)
			a.PostalCode, a.Region = strings.Repeat("9", 20), strings.Repeat("r", 100)
			return a
		}, nil},
		{"blank first line", func(a *PostalAddress) *PostalAddress { a.Line1 = " "; return a }, []string{"line1"}},
		{"no city", func(a *PostalAddress) *PostalAddress { a.City = ""; return a }, []string{"city"}},
		{"too long", func(a *PostalAddress) *PostalAddress {
			a.Line1, a.Line2, a.City = strings.Repeat("é", 201), strings.Repeat("x", 201), strings.Repeat("c", 101)
			a.PostalCode, a.Region = strings.Repeat("9", 21), strings.Repeat("r", 101)
			return a
		}, []string{"line1", "line2", "city", "postal_code", "region"}},
		{"no country", func(a *PostalAddress) *PostalAddress { a.CountryCode = ""; return a }, []string{"country_code"}},
		{"country in lower case", func(a *PostalAddress) *PostalAddress { a.CountryCode = "ca"; return a }, []string{"country_code"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := fixture(t)
			doc.Companies[0].PostalAddress = c.mutate(maple())
			var want []string
			for _, k := range c.want {
				want = append(want, "companies[1000].postal_address."+k)
			}
			if got := errorPaths(Validate(doc, currency.Seeded())); !slices.Equal(got, want) {
				t.Errorf("errors: got %v, want %v", got, want)
			}
		})
	}
}

// TestPostalAddressTier: adding, changing or removing a company's postal
// address is operational: it posts nothing and routes deliveries.
func TestPostalAddressTier(t *testing.T) {
	without := fixture(t)
	with := fixture(t)
	with.Companies[0].PostalAddress = maple()
	moved := fixture(t)
	moved.Companies[0].PostalAddress = maple()
	moved.Companies[0].PostalAddress.Line1 = "1 Front Street"
	var every []string // the diff names each field, sorted
	for _, k := range []string{"city", "country_code", "line1", "line2", "postal_code", "region"} {
		every = append(every, "companies[1000].postal_address."+k)
	}
	for _, c := range []struct {
		name     string
		old, new *Config
		paths    []string
	}{
		{"added", without, with, every},
		{"changed", with, moved, []string{"companies[1000].postal_address.line1"}},
		{"removed", with, without, every},
	} {
		t.Run(c.name, func(t *testing.T) {
			changes, err := Diff(c.old, c.new)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			var paths []string
			for _, ch := range changes {
				paths = append(paths, ch.Path)
				if ch.Tier != TierOperational {
					t.Errorf("%s: tier %v, want %v", ch.Path, ch.Tier, TierOperational)
				}
			}
			if !slices.Equal(paths, c.paths) {
				t.Errorf("changes: got %v, want %v", paths, c.paths)
			}
		})
	}
}
