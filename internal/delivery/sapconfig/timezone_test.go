package sapconfig

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// zoned is the default options with a time zone for each company of the
// mock extract, as erp config generate -time-zone gives them.
func zoned() Options {
	o := DefaultOptions()
	o.TimeZones = map[string]string{"1000": "America/New_York", "2000": "Europe/Berlin", "3000": "America/Toronto"}
	return o
}

// TestTimeZoneIsAnInput: the extract has no time zone for a company code,
// so the draft takes each company's from the options, noted; a company
// given none is drafted without one, noted, and the draft does not
// validate until a reviewer sets it (product owner 2026-10-31).
func TestTimeZoneIsAnInput(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	if got := c.Company("3000").TimeZone; got != "America/Toronto" {
		t.Errorf("company 3000 time zone: got %q, want America/Toronto", got)
	}
	if all := notesText(notes); !strings.Contains(all, "companies[3000].time_zone: America/Toronto, as given") {
		t.Errorf("got notes without the given zone of 3000, want one:\n%s", all)
	}

	opts := zoned()
	delete(opts.TimeZones, "3000")
	c, notes = Generate(sap.GenerateMock(42, "2026"), opts)
	if got := c.Company("3000").TimeZone; got != "" {
		t.Errorf("company 3000 time zone not given: got %q, want none", got)
	}
	if all := notesText(notes); !strings.Contains(all, "companies[3000].time_zone: not in the SAP extract and not given (erp config generate -time-zone 3000=ZONE)") {
		t.Errorf("got notes without the missing zone of 3000, want one:\n%s", all)
	}
	var paths []string
	for _, f := range config.Validate(reviewed(c), currency.Seeded()) {
		if f.Severity == config.SevError {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) != 1 || paths[0] != "companies[3000].time_zone" {
		t.Errorf("errors of the draft without a zone: got %v, want companies[3000].time_zone", paths)
	}
}
