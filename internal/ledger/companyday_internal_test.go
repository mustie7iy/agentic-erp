package ledger

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

// entities is a directory with legal entity 2000, or failing with err.
type entities struct{ err error }

func (e entities) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{ID: id, Code: "2000"}, id != uuid.Nil, e.err
}

func (entities) EntityByCode(context.Context, string) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, nil
}

func (entities) GroupCurrency(context.Context) (string, error) { return "EUR", nil }

// settings is an active configuration with company 2000 in the zone, or
// failing with err.
type settings struct {
	zone string
	err  error
}

func (s settings) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: s.zone}}}, 1, s.err
}

// TestCompanyDays: the company's date in its zone, and each way of not
// knowing it failing rather than falling back to UTC.
func TestCompanyDays(t *testing.T) {
	at := func() time.Time { return time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC) }
	le := uuid.New()
	down := errors.New("down")
	got, err := companyDays{now: at, entities: entities{}, settings: settings{zone: "America/Toronto"}}.of(context.Background(), le)
	if err != nil {
		t.Fatalf("Toronto: %v", err)
	}
	if got.text() != "2026-04-16 (the company's date in America/Toronto)" {
		t.Errorf("Toronto at 02:00 UTC on the 17th: got %q, want 2026-04-16 in America/Toronto", got.text())
	}
	for name, tc := range map[string]struct {
		d    companyDays
		id   uuid.UUID
		want string
	}{
		"no configuration":        {companyDays{now: at, entities: entities{}}, le, "no legal entities or configuration"},
		"no directory":            {companyDays{now: at, settings: settings{}}, le, "no legal entities or configuration"},
		"the directory fails":     {companyDays{now: at, entities: entities{err: down}, settings: settings{}}, le, "down"},
		"an unknown entity":       {companyDays{now: at, entities: entities{}, settings: settings{}}, uuid.Nil, "not found"},
		"the configuration fails": {companyDays{now: at, entities: entities{}, settings: settings{err: down}}, le, "time zone of company 2000: down"},
		"a zone that does not load": {companyDays{now: at, entities: entities{}, settings: settings{zone: "America/Nowhere"}}, le,
			"company 2000: time zone \"America/Nowhere\""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tc.d.of(context.Background(), tc.id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// TestVisibleOutsideARequest: the caller's access is read in the request,
// and without one the date is not given at all.
func TestVisibleOutsideARequest(t *testing.T) {
	d := companyDays{now: time.Now, entities: entities{}, settings: settings{zone: "Asia/Dubai"}}
	if _, err := d.visible(context.Background(), uuid.New()); err == nil || !strings.Contains(err.Error(), "company access") {
		t.Errorf("got %v, want the access read's failure", err)
	}
	if _, err := (companyDays{}).visible(context.Background(), uuid.New()); err == nil {
		t.Error("without a configuration: got nil, want an error")
	}
}
