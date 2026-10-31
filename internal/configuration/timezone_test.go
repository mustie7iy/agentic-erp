package configuration_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// zoneless is document(t) with no company's time zone, as every document
// was before the zone was required.
func zoneless(t *testing.T) *config.Config {
	t.Helper()
	doc := document(t)
	for i := range doc.Companies {
		doc.Companies[i].TimeZone = ""
	}
	return doc
}

// timeZoneFields are the fields of a problem's errors that name a
// company's time zone, under the request's field that carries the
// document (prefix).
func timeZoneFields(prefix string, fields []string) []string {
	var out []string
	for _, f := range fields {
		if f == prefix+".companies[1000].time_zone" || f == prefix+".companies[2000].time_zone" {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// bothZones are the field errors of both companies' time zones under the
// request's field that carries the document.
func bothZones(prefix string) []string {
	return []string{prefix + ".companies[1000].time_zone", prefix + ".companies[2000].time_zone"}
}

// TestTimeZoneRequired: a company's time zone is required of a new or
// changed configuration (product owner 2026-10-31), refused with the
// company and key to set, while a document activated before the rule
// still reads, its companies' dates in UTC (tolerant read, strict write:
// 06 §6.4).
func TestTimeZoneRequired(t *testing.T) {
	e := setup(t, true)
	err := e.bootstrap(t, jsonOf(t, zoneless(t)))
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != "config.validation_failed" {
		t.Fatalf("bootstrap without time zones: got %v, want config.validation_failed", err)
	}
	var fields []string
	for _, f := range p.Errors {
		fields = append(fields, f.Field)
	}
	// The bootstrap's document is the createTenant request's configuration.
	if got := timeZoneFields("configuration", fields); !slices.Equal(got, bothZones("configuration")) {
		t.Errorf("bootstrap without time zones: got errors %v, want one on each company's time_zone", p.Errors)
	}
	if err := e.bootstrap(t, jsonOf(t, document(t))); err != nil {
		t.Fatalf("bootstrap with time zones: %v", err)
	}

	// Version 2 is a document activated before the rule: no zone.
	activateDocument(t, e.ten.ID, `document #- '{companies,0,time_zone}' #- '{companies,1,time_zone}'`)
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		doc, _, err := configuration.Settings{}.Document(ctx)
		if err != nil {
			return err
		}
		if got := doc.Company("1000").TimeZone; got != "" {
			t.Errorf("activated document's time zone: got %q, want none", got)
		}
		loc, err := doc.CompanyLocation("1000")
		if err != nil {
			return err
		}
		if loc != time.UTC {
			t.Errorf("activated document without a time zone: got %v, want UTC", loc)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the activated document: %v", err)
	}

	// A change to it must set every company's zone.
	unzoned := zoneless(t)
	unzoned.Purchasing.ContractExpiryNoticeDays++
	cr := e.propose(t, 2, unzoned)
	refused := e.Problem(422, "config.validation_failed", "POST", "/v1/change-requests/"+apitest.String(t, cr, "id")+":submit", "", `"1"`)
	fields = nil
	for _, f := range refused["errors"].([]any) {
		fields = append(fields, f.(map[string]any)["field"].(string))
	}
	if got := timeZoneFields("proposed", fields); !slices.Equal(got, bothZones("proposed")) {
		t.Errorf("change without time zones: got errors %v, want one on each company's time_zone", refused["errors"])
	}
	zoned := document(t)
	zoned.Purchasing.ContractExpiryNoticeDays++
	cr = e.propose(t, 2, zoned)
	e.Must(200, "POST", "/v1/change-requests/"+apitest.String(t, cr, "id")+":submit", "", `"1"`)
}
