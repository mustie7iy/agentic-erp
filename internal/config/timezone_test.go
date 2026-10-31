package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// TestCompanyTimeZone: a company's time zone is an IANA name, the pilots'
// zones and UTC among them; none is UTC (T72n1). A change to it is
// posting-relevant: it moves which day is today for the company.
func TestCompanyTimeZone(t *testing.T) {
	for zone, want := range map[string]string{"": "UTC", "UTC": "UTC", "America/Toronto": "America/Toronto", "America/Vancouver": "America/Vancouver",
		"Asia/Dubai": "Asia/Dubai", "Etc/GMT+5": "Etc/GMT+5", "America/Argentina/Buenos_Aires": "America/Argentina/Buenos_Aires"} {
		t.Run("zone "+want, func(t *testing.T) {
			c := fixture(t)
			c.Companies[0].TimeZone = zone
			if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
				t.Fatalf("validate: got %v, want no errors", fs)
			}
			loc, err := c.Companies[0].Location()
			if err != nil {
				t.Fatalf("location: got %v, want none", err)
			}
			if got := loc.String(); got != want {
				t.Errorf("location: got %s, want %s", got, want)
			}
		})
	}
	changed := fixture(t)
	changed.Companies[0].TimeZone = "America/Toronto"
	changes, err := Diff(fixture(t), changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "companies[1000].time_zone" {
		t.Fatalf("changes: got %v, want companies[1000].time_zone", changes)
	}
	if changes[0].Tier != TierPostingRelated {
		t.Errorf("tier: got %v, want %v", changes[0].Tier, TierPostingRelated)
	}
}

// TestCompanyLocation: the zone of a company by its code is the company's
// own, UTC when the document has no such company (T72n5), and a zone that
// does not load is an error, not UTC.
func TestCompanyLocation(t *testing.T) {
	c := fixture(t)
	c.Companies[0].TimeZone = "Asia/Dubai"
	for code, want := range map[string]string{c.Companies[0].Code: "Asia/Dubai", "9999": "UTC"} {
		t.Run(code, func(t *testing.T) {
			loc, err := c.CompanyLocation(code)
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if loc.String() != want {
				t.Errorf("got %s, want %s", loc, want)
			}
		})
	}
	c.Companies[0].TimeZone = "America/Nowhere"
	if _, err := c.CompanyLocation(c.Companies[0].Code); err == nil {
		t.Error("got no error, want the zone refused")
	}
}
