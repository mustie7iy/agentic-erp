package tenancy_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// zoned is a configured tenant whose company 1000 is in America/Toronto
// and 2000 in Asia/Dubai, the pilots' zones (ADR-0051 as built).
func zoned(t *testing.T) fixture.Env {
	t.Helper()
	return fixture.NewWith(t, nil, func(c *config.Config) {
		for i, co := range c.Companies {
			c.Companies[i].TimeZone = map[string]string{"1000": "America/Toronto", "2000": "Asia/Dubai"}[co.Code]
		}
	}, perms...)
}

// TestLegalEntityToday: a legal entity's date today is its company's date
// in its time zone at the API's clock, with the zone named, not UTC's
// date: at 22:00 in Toronto (02:00 UTC the next day) it is still that
// day, and at 01:00 in Dubai (21:00 UTC the day before) it is already
// the next.
func TestLegalEntityToday(t *testing.T) {
	f := zoned(t)
	// A legal entity the configuration names no company of is dated in
	// UTC, as every date the API judges for it is (CompanyLocation).
	unconfigured := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, 'CA02', 'Maple East', 'CA', 'CAD', gen_random_uuid(), 'active', $3, $3)`, f.Tenant.ID, unconfigured, f.Tenant.Human.ID)
	for name, tc := range map[string]struct {
		at     string
		entity func(fixture.Env) uuid.UUID
		date   string
		zone   string
	}{
		"Toronto at 22:00":      {"2026-04-18T02:00:00Z", func(f fixture.Env) uuid.UUID { return f.US }, "2026-04-17", "America/Toronto"},
		"Toronto that morning":  {"2026-04-17T12:00:00Z", func(f fixture.Env) uuid.UUID { return f.US }, "2026-04-17", "America/Toronto"},
		"Dubai at 01:00":        {"2026-04-16T21:00:00Z", func(f fixture.Env) uuid.UUID { return f.DE }, "2026-04-17", "Asia/Dubai"},
		"Dubai the evening":     {"2026-04-17T18:00:00Z", func(f fixture.Env) uuid.UUID { return f.DE }, "2026-04-17", "Asia/Dubai"},
		"Dubai on Toronto's 22": {"2026-04-18T02:00:00Z", func(f fixture.Env) uuid.UUID { return f.DE }, "2026-04-18", "Asia/Dubai"},
		"no company configured": {"2026-04-18T02:00:00Z", func(fixture.Env) uuid.UUID { return unconfigured }, "2026-04-18", "UTC"},
	} {
		t.Run(name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			c := f.In(t)
			c.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return at } })
			id := tc.entity(f)
			got, _ := c.Must(200, "GET", "/v1/legal-entities/"+id.String()+"/today", "", "")
			if got["legal_entity_id"] != id.String() {
				t.Errorf("legal_entity_id: got %v, want %s", got["legal_entity_id"], id)
			}
			if got["date"] != tc.date {
				t.Errorf("date: got %v, want %s", got["date"], tc.date)
			}
			if got["time_zone"] != tc.zone {
				t.Errorf("time_zone: got %v, want %s", got["time_zone"], tc.zone)
			}
		})
	}
}

// TestLegalEntityTodayRefused: a legal entity the caller cannot act in,
// or none, is not found, as every tenancy read answers; a tenant without
// a configuration has no time zone to tell the date in (409
// config.not_bootstrapped, not UTC's date).
func TestLegalEntityTodayRefused(t *testing.T) {
	f := zoned(t)
	outsider := f.In(t)
	outsider.As = f.Tenant.Agent // holds tenancy.org.read, acts in no company
	outsider.Problem(404, "request.not_found", "GET", "/v1/legal-entities/"+f.US.String()+"/today", "", "")
	f.Problem(404, "request.not_found", "GET", "/v1/legal-entities/"+uuid.NewString()+"/today", "", "")

	ten := org(t)
	c := newClient(t, ten, ten.Human)
	le := c.legalEntity("DE01")
	c.Problem(409, "config.not_bootstrapped", "GET", "/v1/legal-entities/"+le["id"].(string)+"/today", "", "")
}

// TestLegalEntityTodayFailures: each of its database statements failing
// in turn, the configuration's read included, fails the read.
func TestLegalEntityTodayFailures(t *testing.T) {
	f := zoned(t)
	e := &tenancy.Endpoints{Days: configuration.Settings{}}
	call := func(ctx context.Context) error {
		_, err := e.GetLegalEntityToday(ctx, gen.GetLegalEntityTodayParams{ID: f.US})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "legal entity today", call)
}
