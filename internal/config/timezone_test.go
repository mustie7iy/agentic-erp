package config

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// TestCompanyTimeZone: a company's time zone is an IANA name, the pilots'
// zones and UTC among them (T72n1). A change to it is posting-relevant: it
// moves which day is today for the company.
func TestCompanyTimeZone(t *testing.T) {
	for zone, want := range map[string]string{"UTC": "UTC", "America/Toronto": "America/Toronto", "America/Vancouver": "America/Vancouver",
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
	t.Run("by company code", func(t *testing.T) {
		c := fixture(t)
		c.Companies[0].TimeZone = "Asia/Dubai"
		for code, want := range map[string]string{c.Companies[0].Code: "Asia/Dubai", "none": "UTC"} {
			loc, err := c.CompanyLocation(code)
			if err != nil {
				t.Fatalf("company %s: got %v, want no error", code, err)
			}
			if got := loc.String(); got != want {
				t.Errorf("company %s: got %s, want %s", code, got, want)
			}
		}
		c.Companies[0].TimeZone = "Local"
		if _, err := c.CompanyLocation(c.Companies[0].Code); err == nil {
			t.Error("company in zone Local: got nil error, want the zone refused")
		}
	})
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

// TestCompanyTimeZoneRequired: validation refuses a company without a time
// zone, naming the company and the key (product owner 2026-10-31); a
// document activated before then is read with none, its dates in UTC.
func TestCompanyTimeZoneRequired(t *testing.T) {
	c := fixture(t)
	c.Companies[0].TimeZone = ""
	var errs []Finding
	for _, f := range Validate(c, currency.Seeded()) {
		if f.Severity == SevError {
			errs = append(errs, f)
		}
	}
	if len(errs) != 1 || errs[0].Path != "companies[1000].time_zone" {
		t.Fatalf("validate without a time zone: got %v, want one error on companies[1000].time_zone", errs)
	}
	if want := "time zone is required"; !strings.HasPrefix(errs[0].Message, want) {
		t.Errorf("message: got %q, want it to start %q", errs[0].Message, want)
	}
	doc, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	active, unknown, err := ParseActive(doc)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("read the activated document: got %v, %v, want it read whole", unknown, err)
	}
	loc, err := active.CompanyLocation("1000")
	if err != nil {
		t.Fatalf("location: got %v, want none", err)
	}
	if loc != time.UTC {
		t.Errorf("location without a time zone: got %v, want UTC", loc)
	}
}
