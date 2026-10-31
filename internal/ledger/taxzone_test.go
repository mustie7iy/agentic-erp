package ledger_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// zonedRate is the test document with company 2000 in the zone and its
// single-rate code V1 changed from 19% to 16%.
func zonedRate(tb testing.TB, zone string) *config.Config {
	tb.Helper()
	doc := document(tb)
	co := doc.Company("2000")
	co.TimeZone = zone
	for i := range co.TaxCodes {
		if co.TaxCodes[i].Code == "V1" {
			co.TaxCodes[i].RatePercent = "16"
		}
	}
	return doc
}

// datedRates lists company 2000's live rates of the code as "rate from to"
// with the dates as stored, an open end as "open".
func (e env) datedRates(tb testing.TB, code string) string {
	tb.Helper()
	var out string
	pgtest.Scan(tb, `SELECT coalesce(string_agg(trim_scale(rate_pct) || ' ' || valid_from || ' ' || coalesce(valid_to::text, 'open'), ', '
			ORDER BY valid_from), '')
		FROM ledger.tax_rate WHERE legal_entity_id = $1 AND withdrawn_at IS NULL AND code = $2`, []any{e.de, code}, &out)
	return out
}

// setupAt is the ledger's configuration section with its clock at the
// instant.
func setupAt(at time.Time) ledger.Setup {
	return ledger.Setup{LegalEntities: tenancy.Directory{}, Now: func() time.Time { return at }}
}

// TestTaxRateChangeOnTheCompanysDate: the rate in force at a tax setup's
// activation, and the day a changed single-rate code's new rate starts,
// are judged on the company's date in its time zone, not UTC's (product
// owner 2026-10-31, ADR-0049 and ADR-0051 as built). At 22:00 on 16 April
// in Toronto (02:00 UTC on the 17th) V1's 16% starts on the 16th; at 01:00
// on 17 April in Dubai (21:00 UTC on the 16th) on the 17th. In UTC both
// would be the other day. A posting with V1 dated 16 April is re-rated by
// a change from the 16th: in Toronto the submission reports it and the
// activation refuses it, each naming the company's date and zone; in Dubai
// the change starts after it and goes ahead.
func TestTaxRateChangeOnTheCompanysDate(t *testing.T) {
	for _, c := range []struct {
		zone   string
		at     time.Time
		today  string
		posted bool // a posting dated 16 April is re-rated
	}{
		{"America/Toronto", time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC), "2026-04-16", true},
		{"Asia/Dubai", time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), "2026-04-17", false},
	} {
		t.Run(c.zone, func(t *testing.T) {
			t.Run("the change", func(t *testing.T) {
				e := setup(t)
				if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
					return setupAt(c.at).Apply(ctx, zonedRate(t, c.zone))
				}); err != nil {
					t.Fatal(err)
				}
				if got, want := e.datedRates(t, "V1"), "19 2026-01-01 "+c.today+", 16 "+c.today+" open"; got != want {
					t.Errorf("rates: got %s, want %s", got, want)
				}
			})
			t.Run("a posting on 16 April", func(t *testing.T) {
				e := setup(t)
				e.postTaxed(t, "V1", "2026-04-16")
				named := c.today + " (the company's date in " + c.zone + ")"
				var fs []config.Finding
				if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
					fs, err = setupAt(c.at).Check(ctx, document(t), zonedRate(t, c.zone))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				const path = "companies[2000].tax_codes[V1].components[tax].rates"
				err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
					return setupAt(c.at).Apply(ctx, zonedRate(t, c.zone))
				})
				if !c.posted {
					wantNoFinding(t, fs, path)
					if err != nil {
						t.Errorf("activation after the posting's day: got %v, want it applied", err)
					}
					return
				}
				wantFinding(t, fs, path, "it is "+named)
				got := problem.From(err)
				if got.Status != 409 || got.Code != "ledger.tax_rate_in_use" {
					t.Fatalf("activation: got %v, want 409 ledger.tax_rate_in_use", err)
				}
				if want := "The activation dates a changed rate from " + named + "."; !strings.Contains(got.Detail, want) {
					t.Errorf("activation detail: got %q, want it to say %q", got.Detail, want)
				}
			})
		})
	}
}

// wantNoFinding fails when fs has a finding at path.
func wantNoFinding(t *testing.T, fs []config.Finding, path string) {
	t.Helper()
	for _, f := range fs {
		if f.Path == path {
			t.Errorf("findings: got %v at %s, want none", f.Message, path)
		}
	}
}

// TestTaxRateChangeUnknownZone: a stored zone the binary cannot load
// (validation refuses one, so only a zone database that lost it) leaves
// the submission's rate check undone, as validation reports the zone, and
// fails the activation rather than dating the change in UTC.
func TestTaxRateChangeUnknownZone(t *testing.T) {
	e := setup(t)
	// A posting dated UTC's date at the instant would be re-rated by a
	// change dated in UTC: a finding would show the check fell back to it.
	at := time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC)
	e.postTaxed(t, "V1", "2026-04-17")
	var fs []config.Finding
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		fs, err = setupAt(at).Check(ctx, document(t), zonedRate(t, "America/Nowhere"))
		return err
	}); err != nil {
		t.Fatalf("checking an unknown zone: got %v, want findings", err)
	}
	wantNoFinding(t, fs, "companies[2000].tax_codes[V1].components[tax].rates")
	err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return setupAt(at).Apply(ctx, zonedRate(t, "America/Nowhere"))
	})
	if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("applying an unknown zone: got %v, want the zone named", err)
	}
	if got := e.datedRates(t, "V1"); got != "19 2026-01-01 open" {
		t.Errorf("rates after the refusal: got %s, want V1 unchanged", got)
	}
}
