package config

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// directory is an EntityCode knowing legal entity known as company 2000,
// or failing with err.
func directory(known uuid.UUID, err error) EntityCode {
	return func(_ context.Context, le uuid.UUID) (string, bool, error) {
		if le != known {
			return "", false, err
		}
		return "2000", true, err
	}
}

// documents is the active configuration, company 2000 in the zone, or
// failing with err; read counts the reads.
type documents struct {
	zone string
	err  error
	read *int
}

func (d documents) Document(context.Context) (*Config, int32, error) {
	if d.read != nil {
		*d.read++
	}
	return &Config{Companies: []Company{{Code: "2000", TimeZone: d.zone}}}, 1, d.err
}

// TestEntityDay: a legal entity's date is its company's in its zone, UTC
// when the company sets none (review:batch27: one helper for every
// context). The same instant, 02:00 UTC on 18 April, is the 17th in
// Toronto and the 18th in Dubai and UTC.
func TestEntityDay(t *testing.T) {
	le := uuid.New()
	at := time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC)
	for zone, want := range map[string]struct{ date, zone string }{
		"America/Toronto": {"2026-04-17", "America/Toronto"}, "Asia/Dubai": {"2026-04-18", "Asia/Dubai"}, "": {"2026-04-18", "UTC"},
	} {
		t.Run("zone "+want.zone, func(t *testing.T) {
			d, err := EntityDay(context.Background(), documents{zone: zone}, directory(le, nil), le, at)
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if got := d.Date.Format(time.DateOnly); got != want.date {
				t.Errorf("date: got %s, want %s", got, want.date)
			}
			if got := d.Zone.String(); got != want.zone {
				t.Errorf("zone: got %s, want %s", got, want.zone)
			}
		})
	}
	t.Run("a document in hand", func(t *testing.T) {
		doc := &Config{Companies: []Company{{Code: "2000", TimeZone: "America/Toronto"}}}
		d, err := doc.EntityDay(context.Background(), directory(le, nil), le, at)
		if err != nil || d.Date.Format(time.DateOnly) != "2026-04-17" {
			t.Errorf("got %v, %v, want 2026-04-17", d.Date, err)
		}
	})
}

// TestEntityDayRefusals: a legal entity the directory does not know is
// refused with 422 request.reference_not_found naming it, ErrNoLegalEntity
// its cause, and the configuration is not read; a directory or
// configuration failing, or a zone the binary cannot load, fails rather
// than giving UTC's date.
func TestEntityDayRefusals(t *testing.T) {
	le, down := uuid.New(), errors.New("down")
	at := time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC)
	t.Run("an unknown legal entity", func(t *testing.T) {
		read := 0
		_, err := EntityDay(context.Background(), documents{read: &read}, directory(le, nil), uuid.Nil, at)
		if !errors.Is(err, ErrNoLegalEntity) {
			t.Fatalf("got %v, want ErrNoLegalEntity", err)
		}
		p := problem.From(err)
		if p.Status != http.StatusUnprocessableEntity || p.Code != "request.reference_not_found" {
			t.Errorf("problem: got %d %s, want 422 request.reference_not_found", p.Status, p.Code)
		}
		if want := "Legal entity " + uuid.Nil.String() + " does not exist"; !strings.HasPrefix(p.Detail, want) {
			t.Errorf("detail: got %q, want it to start %q", p.Detail, want)
		}
		if read != 0 {
			t.Errorf("configuration reads: got %d, want 0", read)
		}
	})
	for name, tc := range map[string]struct {
		docs   Documents
		codeOf EntityCode
		want   string
	}{
		"the directory fails":       {documents{}, directory(le, down), "legal entity " + le.String() + ": down"},
		"the configuration fails":   {documents{err: down}, directory(le, nil), "time zone of company 2000: down"},
		"a zone that does not load": {documents{zone: "America/Nowhere"}, directory(le, nil), `company 2000: time zone "America/Nowhere"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := EntityDay(context.Background(), tc.docs, tc.codeOf, le, at)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
			if errors.Is(err, ErrNoLegalEntity) {
				t.Errorf("got %v, want it not taken for an unknown legal entity", err)
			}
		})
	}
}
