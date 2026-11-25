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

// countingEntities is a directory of the entities given that counts its
// lookups, or fails with err.
type countingEntities struct {
	entities       []Entity
	err            error
	byCode, listed *int
}

func (c countingEntities) Company(context.Context, uuid.UUID) (Company, bool, error) {
	return Company{}, false, errors.New("countingEntities: Company not expected")
}

func (c countingEntities) ByCode(_ context.Context, code string) (uuid.UUID, bool, error) {
	*c.byCode++
	for _, e := range c.entities {
		if e.Code == code {
			return e.ID, true, c.err
		}
	}
	return uuid.Nil, false, c.err
}

func (c countingEntities) Entities(context.Context) ([]Entity, error) {
	*c.listed++
	return c.entities, c.err
}

func (c countingEntities) GroupCurrency(context.Context) (string, error) {
	return "", errors.New("countingEntities: GroupCurrency not expected")
}

// manyZones is a configuration of four companies, three with a zone, one
// of those (5000) with no legal entity yet.
type manyZones struct{}

func (manyZones) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: "Europe/Berlin"}, {Code: "3000", TimeZone: "America/Toronto"},
		{Code: "4000"}, {Code: "5000", TimeZone: "Asia/Dubai"}}}, 1, nil
}

// TestReadDaysOfOneLookup (review:batch26): an unfiltered memo record list
// finds the legal entities of every zoned company in one directory
// lookup, not one per company; each is dated in its zone, a company
// without a zone or without a legal entity reads none, and the directory
// failing fails the list.
func TestReadDaysOfOneLookup(t *testing.T) {
	berlin, toronto, other := uuid.New(), uuid.New(), uuid.New()
	entities := []Entity{{ID: berlin, Company: Company{Code: "2000"}}, {ID: toronto, Company: Company{Code: "3000"}}, {ID: other, Company: Company{Code: "4000"}}}
	var byCode, listed int
	at := func() time.Time { return time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) }
	e := &Endpoints{LegalEntities: countingEntities{entities: entities, byCode: &byCode, listed: &listed}, Settings: manyZones{}, Now: at}
	r, err := e.readDaysOf(context.Background(), nil)
	if err != nil {
		t.Fatalf("got %v, want no error", err)
	}
	if byCode != 0 || listed != 1 {
		t.Errorf("directory lookups: got %d by code and %d lists, want 0 and 1", byCode, listed)
	}
	got := map[uuid.UUID]string{}
	for id, d := range r.byEntity {
		got[id] = d.Date.Format(time.DateOnly) + " " + d.Zone
	}
	want := map[uuid.UUID]string{berlin: "2026-04-18 Europe/Berlin", toronto: "2026-04-17 America/Toronto"}
	if len(got) != len(want) || got[berlin] != want[berlin] || got[toronto] != want[toronto] {
		t.Errorf("dates read: got %v, want %v", got, want)
	}
	e.LegalEntities = countingEntities{err: errors.New("down"), byCode: &byCode, listed: &listed}
	if _, err := e.readDaysOf(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "cash: legal entities of the zoned companies: down") {
		t.Errorf("directory failing: got %v, want its error", err)
	}
}
