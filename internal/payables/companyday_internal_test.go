package payables

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// zoneDirectory knows every legal entity as company 2000 unless absent,
// or fails with err.
type zoneDirectory struct {
	absent bool
	err    error
}

func (d zoneDirectory) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{ID: id, Code: "2000"}, !d.absent && d.err == nil, d.err
}

// zoneSettings is an active configuration with company 2000 in the zone,
// or failing with err.
type zoneSettings struct {
	zone string
	err  error
}

func (s zoneSettings) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: s.zone}}}, 1, s.err
}

// TestCompanyDayFailures: a posting date the caller left out is never
// read in UTC when the company's date cannot be told: the directory or the
// configuration failing, or a stored zone the binary cannot load
// (validation refuses one, so only a zone database that lost it), fails
// the post and the reversal.
func TestCompanyDayFailures(t *testing.T) {
	down := errors.New("down")
	at := func() time.Time { return time.Date(2026, 4, 1, 2, 0, 0, 0, time.UTC) }
	for name, tc := range map[string]struct {
		e    Endpoints
		want string
	}{
		"the directory fails":       {Endpoints{LegalEntities: zoneDirectory{err: down}, Now: at}, "payables: legal entity"},
		"an unknown legal entity":   {Endpoints{LegalEntities: zoneDirectory{absent: true}, Now: at}, "payables: legal entity"},
		"the configuration fails":   {Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{err: down}, Now: at}, "payables: time zone of company 2000: down"},
		"a zone that does not load": {Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{zone: "America/Nowhere"}, Now: at}, "payables: company 2000"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tc.e.companyDay(context.Background(), uuid.New()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
		})
	}
	e := Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{zone: "America/Toronto"}, Now: at}
	if got, err := e.companyDay(context.Background(), uuid.New()); err != nil || got.Format(time.DateOnly) != "2026-03-31" {
		t.Errorf("Toronto at 02:00 UTC on 1 April: got %v, %v, want 2026-03-31", got, err)
	}
}
