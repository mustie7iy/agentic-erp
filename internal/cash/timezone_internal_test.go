package cash

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestReadDateUnknownZone: a stored document whose company names a zone
// this build cannot load (validation refuses one, so only a zone database
// that lost it) fails the read rather than reading the date in UTC.
func TestReadDateUnknownZone(t *testing.T) {
	e := &Endpoints{Now: func() time.Time { return time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) }}
	doc := &config.Config{Companies: []config.Company{{Code: "3000", TimeZone: "America/Nowhere"}}}
	if _, _, err := e.readDate(doc, "3000"); err == nil || !strings.Contains(err.Error(), "cash: company 3000") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}

// zoneEntities knows legal entity known as company 3000, or fails with
// err.
type zoneEntities struct {
	LegalEntities // the rest, never called
	known         uuid.UUID
	err           error
}

func (z zoneEntities) Company(_ context.Context, id uuid.UUID) (Company, bool, error) {
	return Company{Code: "3000"}, id == z.known, z.err
}

func (z zoneEntities) ByCode(context.Context, string) (uuid.UUID, bool, error) {
	return z.known, true, z.err
}

// zoneSettings is a configuration with company 3000 in Toronto, or failing
// with err.
type zoneSettings struct{ err error }

func (s zoneSettings) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "3000", TimeZone: "America/Toronto"}}}, 1, s.err
}

// TestRecordDay: a memo record read alone is judged on its legal entity's
// date in its company's zone; a stored record's legal entity the directory
// does not know is refused, as every company date refuses it
// (config.EntityDay, review:batch27), no longer read on UTC's date.
func TestRecordDay(t *testing.T) {
	le, down := uuid.New(), errors.New("down")
	at := func() time.Time { return time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) }
	e := &Endpoints{LegalEntities: zoneEntities{known: le}, Settings: zoneSettings{}, Now: at}
	d, err := e.recordDay(context.Background(), le)
	if err != nil {
		t.Fatalf("Toronto: got %v, want no error", err)
	}
	if got := d.Date.Format(time.DateOnly) + " " + d.Zone; got != "2026-04-17 America/Toronto" {
		t.Errorf("Toronto at 02:00 UTC on the 18th: got %s, want 2026-04-17 America/Toronto", got)
	}
	for name, tc := range map[string]struct {
		e    *Endpoints
		want string
	}{
		"an unknown legal entity": {&Endpoints{LegalEntities: zoneEntities{}, Settings: zoneSettings{}, Now: at}, "request.reference_not_found"},
		"the directory fails":     {&Endpoints{LegalEntities: zoneEntities{known: le, err: down}, Settings: zoneSettings{}, Now: at}, "cash: legal entity"},
		"the configuration fails": {&Endpoints{LegalEntities: zoneEntities{known: le}, Settings: zoneSettings{err: down}, Now: at}, "cash: configuration: down"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tc.e.recordDay(context.Background(), le); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// TestReadDaysOfUnknownFilter: a list filtered by a legal entity the
// directory does not know reads no date for it, and answers as for one
// the caller cannot access (it matches no record); a directory failing
// fails the list.
func TestReadDaysOfUnknownFilter(t *testing.T) {
	le := uuid.New()
	at := func() time.Time { return time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) }
	e := &Endpoints{LegalEntities: zoneEntities{known: uuid.New()}, Settings: zoneSettings{}, Now: at}
	r, err := e.readDaysOf(context.Background(), &le)
	if err != nil {
		t.Fatalf("got %v, want no error", err)
	}
	if len(r.byEntity) != 0 {
		t.Errorf("dates read: got %v, want none", r.byEntity)
	}
	e.LegalEntities = zoneEntities{known: le, err: errors.New("down")}
	if _, err := e.readDaysOf(context.Background(), &le); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("directory failing: got %v, want its error", err)
	}
}
