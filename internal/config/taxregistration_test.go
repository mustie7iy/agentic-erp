package config

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// registration is a valid quarterly registration of company 1000.
func registration() TaxRegistration {
	return TaxRegistration{Code: "GST", Authority: "CRA", Number: "123456789 RT 0001", ReturnType: "GST34", Frequency: TaxQuarterly,
		FirstPeriodStart: "2026-01-01"}
}

// withRegistrations is the fixture with company 1000's registrations rs.
func withRegistrations(t *testing.T, rs ...TaxRegistration) *Config {
	t.Helper()
	c := fixture(t)
	c.Companies[0].TaxRegistrations = rs
	return c
}

// errorPaths are the paths of the error findings.
func errorPaths(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == SevError {
			out = append(out, f.Path)
		}
	}
	return out
}

// TestTaxRegistrations: a company's registrations validate (ADR-0049 §10,
// T61g); a tax group's lists its other members; a registration ending on a
// period's last day hands over to one of a new code.
func TestTaxRegistrations(t *testing.T) {
	group := registration()
	group.Code, group.Authority, group.Number, group.ReturnType, group.FirstPeriodStart = "VAT", "FTA", "100000000000003", "VAT201", "2026-02-01"
	group.GroupMembers = []string{"2000"}
	ended := registration()
	ended.LastPeriodEnd = "2026-06-30"
	monthly := registration()
	monthly.Code, monthly.Frequency, monthly.FirstPeriodStart = "GSTM", TaxMonthly, "2026-07-01"
	for name, rs := range map[string][]TaxRegistration{
		"one":                           {registration()},
		"a tax group and another":       {registration(), group},
		"a frequency changed by a code": {ended, monthly},
	} {
		t.Run(name, func(t *testing.T) {
			if fs := Validate(withRegistrations(t, rs...), currency.Seeded()); HasErrors(fs) {
				t.Fatalf("validate: got %v, want no errors", fs)
			}
		})
	}
}

// TestTaxRegistrationRules: each mutation is refused at its path.
func TestTaxRegistrationRules(t *testing.T) {
	const p = "companies[1000].tax_registrations[GST]"
	mutated := func(f func(r *TaxRegistration)) []TaxRegistration {
		r := registration()
		f(&r)
		return []TaxRegistration{r}
	}
	overlapping := registration()
	overlapping.Code, overlapping.FirstPeriodStart = "GST2", "2026-04-01"
	other := registration()
	other.Code, other.Authority = "GST2", "RQ"
	cases := []struct {
		name string
		rs   []TaxRegistration
		path string
	}{
		{"code", mutated(func(r *TaxRegistration) { r.Code = "gst" }), "companies[1000].tax_registrations[0].code"},
		{"authority", mutated(func(r *TaxRegistration) { r.Authority = "1CRA" }), p + ".authority"},
		{"number missing", mutated(func(r *TaxRegistration) { r.Number = "  " }), p + ".number"},
		{"number too long", mutated(func(r *TaxRegistration) { r.Number = "12345678901234567890123456789012345678901" }), p + ".number"},
		{"number not printable", mutated(func(r *TaxRegistration) { r.Number = "123\u0000456" }), p + ".number"},
		{"return type", mutated(func(r *TaxRegistration) { r.ReturnType = "gst 34" }), p + ".return_type"},
		{"frequency", mutated(func(r *TaxRegistration) { r.Frequency = "weekly" }), p + ".frequency"},
		{"frequency with an end", mutated(func(r *TaxRegistration) { r.Frequency, r.LastPeriodEnd = "weekly", "2026-03-31" }), p + ".frequency"},
		{"first period start", mutated(func(r *TaxRegistration) { r.FirstPeriodStart = "2026-1-1" }), p + ".first_period_start"},
		{"first period start after the 28th", mutated(func(r *TaxRegistration) { r.FirstPeriodStart = "2026-01-29" }), p + ".first_period_start"},
		{"last period end", mutated(func(r *TaxRegistration) { r.LastPeriodEnd = "June" }), p + ".last_period_end"},
		{"last period end inside a period", mutated(func(r *TaxRegistration) { r.LastPeriodEnd = "2026-05-31" }), p + ".last_period_end"},
		{"last period end before the first", mutated(func(r *TaxRegistration) { r.LastPeriodEnd = "2025-12-31" }), p + ".last_period_end"},
		{"member the registrant", mutated(func(r *TaxRegistration) { r.GroupMembers = []string{"1000"} }), p + ".group_members"},
		{"member unknown", mutated(func(r *TaxRegistration) { r.GroupMembers = []string{"9999"} }), p + ".group_members"},
		{"member twice", mutated(func(r *TaxRegistration) { r.GroupMembers = []string{"2000", "2000"} }), p + ".group_members"},
		{"code twice", []TaxRegistration{registration(), other, func() TaxRegistration { r := other; r.Authority = "PST"; return r }()},
			"companies[1000].tax_registrations[GST2].code"},
		{"one authority twice", []TaxRegistration{registration(), overlapping}, "companies[1000].tax_registrations[GST2]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := Validate(withRegistrations(t, tc.rs...), currency.Seeded())
			if got := errorPaths(fs); !slices.Contains(got, tc.path) {
				t.Errorf("errors: got %v, want one at %s", got, tc.path)
			}
		})
	}
}

// TestTaxRegistrationAcrossCompanies: a code names one registration in the
// tenant; a company in a tax group files no other return of the authority
// for the group's periods; one number is one registration at a time,
// whatever the authority's code is written as.
func TestTaxRegistrationAcrossCompanies(t *testing.T) {
	group := registration()
	group.Code, group.Authority, group.GroupMembers = "VAT", "FTA", []string{"2000"}
	member := registration()
	member.Code, member.Authority, member.Number = "VAT2", "FTA", "999"
	sameNumber := registration()
	sameNumber.Code = "OTHER"
	later := registration()
	later.Code, later.Authority, later.FirstPeriodStart = "VAT2", "FTA", "2027-01-01"
	endedGroup := group
	endedGroup.LastPeriodEnd = "2026-12-31"
	cases := []struct {
		name       string
		first, sec []TaxRegistration
		want       []string
	}{
		{"code in two companies", []TaxRegistration{registration()}, []TaxRegistration{registration()}, []string{"companies[2000].tax_registrations[GST].code"}},
		{"member's own registration", []TaxRegistration{group}, []TaxRegistration{member}, []string{"companies[2000].tax_registrations[VAT2]"}},
		{"member's in a group of another authority", []TaxRegistration{group}, []TaxRegistration{func() TaxRegistration { r := member; r.Authority = "CRA"; return r }()}, nil},
		{"one number in two companies", []TaxRegistration{registration()}, []TaxRegistration{sameNumber},
			[]string{"companies[2000].tax_registrations[OTHER].number"}},
		{"one number, written otherwise", []TaxRegistration{registration()}, []TaxRegistration{func() TaxRegistration { r := sameNumber; r.Number = "123456789rt0001"; return r }()},
			[]string{"companies[2000].tax_registrations[OTHER].number"}},
		{"the member's after the group ended", []TaxRegistration{endedGroup}, []TaxRegistration{later}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withRegistrations(t, tc.first...)
			c.Companies[1].TaxRegistrations = tc.sec
			if got := errorPaths(Validate(c, currency.Seeded())); !slices.Equal(got, tc.want) {
				t.Errorf("errors: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaxPeriods: periods follow from the first period's start by the
// frequency, up to the one starting by through, and none after the
// registration's end.
func TestTaxPeriods(t *testing.T) {
	d := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ended := d("2026-06-27")
	cases := []struct {
		name      string
		first     string
		frequency string
		last      *time.Time
		through   string
		want      []string
	}{
		{"quarterly", "2026-01-01", TaxQuarterly, nil, "2026-10-31", []string{"2026-01-01..2026-03-31", "2026-04-01..2026-06-30",
			"2026-07-01..2026-09-30", "2026-10-01..2026-12-31"}},
		{"monthly from the 28th", "2026-01-28", TaxMonthly, nil, "2026-03-28", []string{"2026-01-28..2026-02-27", "2026-02-28..2026-03-27",
			"2026-03-28..2026-04-27"}},
		{"annual", "2025-02-01", TaxAnnual, nil, "2026-01-31", []string{"2025-02-01..2026-01-31"}},
		{"ended", "2026-03-28", TaxQuarterly, &ended, "2027-12-31", []string{"2026-03-28..2026-06-27"}},
		{"not begun", "2027-01-01", TaxQuarterly, nil, "2026-10-31", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, p := range TaxPeriods(d(tc.first), tc.frequency, tc.last, d(tc.through)) {
				got = append(got, p.Start.Format(time.DateOnly)+".."+p.End.Format(time.DateOnly))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("periods: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaxRegistrationTier: a registration change is tier 3, and a
// registration is compared by its code.
func TestTaxRegistrationTier(t *testing.T) {
	ended := registration()
	ended.LastPeriodEnd = "2026-12-31"
	changes, err := Diff(withRegistrations(t, registration()), withRegistrations(t, ended))
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "companies[1000].tax_registrations[GST].last_period_end" {
		t.Fatalf("changes: got %v, want companies[1000].tax_registrations[GST].last_period_end", changes)
	}
	if changes[0].Tier != TierPostingRelated {
		t.Errorf("tier: got %v, want %v", changes[0].Tier, TierPostingRelated)
	}
}
