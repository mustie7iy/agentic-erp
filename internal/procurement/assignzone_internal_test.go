package procurement

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// zoneDirectory knows legal entity 2000 unless absent, or fails with err.
type zoneDirectory struct {
	absent bool
	err    error
}

func (d zoneDirectory) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{ID: id, Code: "2000"}, !d.absent, d.err
}

func (zoneDirectory) OrgUnit(context.Context, uuid.UUID) (tenancy.Unit, bool, error) {
	return tenancy.Unit{}, false, nil
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

// TestCheckSubmittedFailures: a submission whose company's date cannot be
// told fails rather than checking the assignments in UTC.
func TestCheckSubmittedFailures(t *testing.T) {
	down := errors.New("down")
	for name, tc := range map[string]struct {
		e    Endpoints
		want string
	}{
		"the directory fails":       {Endpoints{LegalEntities: zoneDirectory{err: down}}, "down"},
		"an unknown legal entity":   {Endpoints{LegalEntities: zoneDirectory{absent: true}}, "request.reference_not_found"},
		"the configuration fails":   {Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{err: down}}, "procurement: time zone of company 2000: down"},
		"a zone that does not load": {Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{zone: "America/Nowhere"}}, "procurement: company 2000"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.e.checkSubmitted(context.Background(), Requisition{ID: uuid.New(), LegalEntityID: uuid.New()}); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
		})
	}
}
