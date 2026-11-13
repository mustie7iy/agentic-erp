package records_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/records"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// years is n as a rule's years.
func years(n int) *int { return &n }

// setup is a configured tenant whose company 2000 keeps journal entries
// for the years given (none: no rule) and has no rule for personal data;
// its person reads the retention status.
func setup(t *testing.T, journal *int) fixture.Env {
	t.Helper()
	return fixture.NewWith(t, nil, func(c *config.Config) {
		if journal != nil {
			c.Company("2000").Retention = []config.RetentionRule{{Kind: config.RetentionJournalEntry, Years: journal}}
		}
	}, "records.retention.read")
}

// at is the context's endpoints as the API wires them, on the day.
func at(day string) *records.Endpoints {
	now, _ := time.Parse(time.DateOnly, day) // the tests' days parse
	return app.Records(app.Config{Now: func() time.Time { return now.Add(12 * time.Hour) }})
}

// status reads the retention status on the day as the tenant's person.
func status(t *testing.T, e fixture.Env, day string, le uuid.UUID, kind string) (*gen.RetentionStatus, error) {
	t.Helper()
	var out *gen.RetentionStatus
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		res, err := at(day).GetRetentionStatus(ctx, gen.GetRetentionStatusParams{LegalEntityID: le, DocumentKind: kind})
		if err == nil {
			out = res.(*gen.RetentionStatus)
		}
		return err
	})
	return out, err
}

// yearsOf are the status's years as "year end retention_ends past".
func yearsOf(s *gen.RetentionStatus) string {
	out := make([]string, len(s.Years))
	for i, y := range s.Years {
		ends := "-"
		if v, ok := y.RetentionEnds.Get(); ok {
			ends = v.Format(time.DateOnly)
		}
		out[i] = fmt.Sprint(y.FiscalYear, " ", y.EndDate.Format(time.DateOnly), " ", ends, " ", y.PastRetention)
	}
	return strings.Join(out, ", ")
}

// TestRetentionStatus: a fiscal year is past retention from the day
// after its last day plus the rule's years; a rule of zero years puts an
// ended year past at once, one of ten keeps it (T99a acceptance).
func TestRetentionStatus(t *testing.T) {
	for _, c := range []struct {
		name  string
		rule  *int
		day   string
		years string
		past  int32
	}{
		{"zero years, the year ended", years(0), "2027-01-01", "2026 2026-12-31 2026-12-31 true", 1},
		{"zero years, on the year's last day", years(0), "2026-12-31", "2026 2026-12-31 2026-12-31 false", 0},
		{"ten years", years(10), "2027-01-01", "2026 2026-12-31 2036-12-31 false", 0},
		{"ten years, the day after they end", years(10), "2037-01-01", "2026 2026-12-31 2036-12-31 true", 1},
		{"no rule", nil, "2040-01-01", "2026 2026-12-31 - false", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, c.rule)
			s, err := status(t, e, c.day, e.DE, config.RetentionJournalEntry)
			if err != nil {
				t.Fatal(err)
			}
			if got := yearsOf(s); !strings.HasPrefix(got, c.years) {
				t.Errorf("years: got %s, want %s first", got, c.years)
			}
			if s.PastRetentionCount != c.past || s.YearCount != int32(len(s.Years)) {
				t.Errorf("totals: got %d past of %d, want %d past of %d", s.PastRetentionCount, s.YearCount, c.past, len(s.Years))
			}
			if s.HasRule != (c.rule != nil) || s.RetentionYears.Set != (c.rule != nil) {
				t.Errorf("rule: got has_rule %v, years %v, want the rule %v", s.HasRule, s.RetentionYears, c.rule)
			}
			if got := fmt.Sprint(s.CompanyCode, " ", s.CompanyName, " ", s.DocumentKind, " ", s.AsOf.Format(time.DateOnly), " ", s.ConfigurationVersion > 0); got !=
				"2000 Acme GmbH journal_entry "+c.day+" true" {
				t.Errorf("header: got %s", got)
			}
		})
	}
}

// TestRetentionStatusKinds: a kind without a rule has nothing past
// retention, whatever another kind's rule says; an unknown kind is
// refused naming the field.
func TestRetentionStatusKinds(t *testing.T) {
	e := setup(t, years(0))
	s, err := status(t, e, "2030-01-01", e.DE, config.RetentionPersonalData)
	if err != nil {
		t.Fatal(err)
	}
	if s.HasRule || s.PastRetentionCount != 0 {
		t.Errorf("personal data without a rule: got has_rule %v and %d past, want none", s.HasRule, s.PastRetentionCount)
	}
	_, err = status(t, e, "2030-01-01", e.DE, "invoice")
	if p := problem.From(err); p.Code != "request.validation_failed" || len(p.Errors) != 1 || p.Errors[0].Field != "document_kind" {
		t.Errorf("an unknown kind: got %v, want request.validation_failed naming document_kind", err)
	}
}

// TestRetentionStatusCompanies: a legal entity the caller cannot act in
// is not found, as one that does not exist; one whose company the
// configuration does not have is dated in UTC and has no rule.
func TestRetentionStatusCompanies(t *testing.T) {
	e := setup(t, years(0))
	if _, err := status(t, e, "2030-01-01", uuid.Must(uuid.NewV7()), config.RetentionJournalEntry); problem.From(err).Status != 404 {
		t.Errorf("an unknown legal entity: got %v, want 404", err)
	}
	reader := e.Reader(t, "Rita Reader", e.US)
	err := apitest.InRequest(t, e.Tenant, reader, func(ctx context.Context) error {
		_, err := at("2030-01-01").GetRetentionStatus(ctx, gen.GetRetentionStatusParams{LegalEntityID: e.DE, DocumentKind: config.RetentionJournalEntry})
		return err
	})
	if problem.From(err).Status != 404 {
		t.Errorf("another company's: got %v, want 404", err)
	}
	stray := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '9000', 'Stray Ltd', 'GB', 'GBP', gen_random_uuid(), 'active', $3, $3)`, e.Tenant.ID, stray, e.Tenant.Human.ID)
	s, err := status(t, e, "2030-01-01", stray, config.RetentionJournalEntry)
	if err != nil || s.HasRule || len(s.Years) != 0 || s.AsOf.Format(time.DateOnly) != "2030-01-01" {
		t.Errorf("a company the configuration does not have: got %+v, %v, want no rule, no year, dated in UTC", s, err)
	}
}

// TestRetentionStatusThroughTheAPI: the read is the API's.
func TestRetentionStatusThroughTheAPI(t *testing.T) {
	e := setup(t, years(0))
	got, _ := e.Must(200, "GET", "/v1/retention-status?document_kind=journal_entry&legal_entity_id="+e.DE.String(), "", "")
	if got["has_rule"] != true || got["retention_years"] != float64(0) || got["company_code"] != "2000" {
		t.Errorf("got %v, want company 2000's rule of zero years", got)
	}
}

// badZone is a configuration whose company 2000 names a time zone that
// does not exist, as Settings.
type badZone struct{}

func (badZone) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: "Nowhere/Atlantis"}}}, 1, nil
}

// TestDatabaseFailures calls the read outside a unit of work and with
// each of its database statements failing in turn, and with a company
// whose date cannot be told.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t, years(0))
	read := func(ctx context.Context) error {
		_, err := at("2030-01-01").GetRetentionStatus(ctx, gen.GetRetentionStatusParams{LegalEntityID: e.DE, DocumentKind: config.RetentionJournalEntry})
		return err
	}
	if err := read(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "retention status", read)
	h := at("2030-01-01")
	h.Settings = badZone{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.StatusOf(ctx, e.DE, config.RetentionJournalEntry)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "date of company 2000") {
		t.Errorf("a time zone that does not exist: got %v, want the company's date refused", err)
	}
}
