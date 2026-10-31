package procurement_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestContractExpiryTimeZone: contracts expire, and are alerted, on their
// company's date in its time zone, not UTC's (T72n6). At 22:00 on 17
// April in Toronto (02:00 UTC on the 18th) a contract whose last day is
// the 17th is still active, and one ending on the 27th has 10 days left,
// alerted on the 17th; at 01:00 on 18 April in Dubai (21:00 UTC on the
// 17th) the first has expired on the 18th, and one ending on the 28th has
// 10 days left, alerted on the 18th. The UTC date would decide each the
// other way. The round is the cell's (RunOnce), so the tenant is found by
// the worker's superset of UTC's date.
func TestContractExpiryTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone, today, ending string
		at                  time.Time
		expired             bool
	}{
		{"America/Toronto", "2026-04-17", "2026-04-27", time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC), false},
		{"Asia/Dubai", "2026-04-18", "2026-04-28", time.Date(2026, 4, 17, 21, 0, 0, 0, time.UTC), true},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, perms...)
			e := env{Env: f, office: f.Account(t, "2000", "520000").String()}
			// The contract ending on the 17th was alerted before, so only its
			// expiry brings the tenant into the cell's round; the other is
			// released after the expiry rounds, for the alert rounds.
			last, lastPath := e.activeContract(t)
			pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = '2026-01-01', valid_to = '2026-04-17', expiry_alerted_on = '2026-03-18',
				version = version + 1 WHERE id = $1`, uuidOf(last))
			soon := e.contract(t, e.Supplier.String(), "")
			soonPath := "/v1/contracts/" + id(soon)
			pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = '2026-01-01', valid_to = $2, version = version + 1 WHERE id = $1`, uuidOf(soon), c.ending)
			x := &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(&pgtest.SyncBuffer{}, nil)),
				Settings: configuration.Settings{}, LegalEntities: tenancy.Directory{}, Partners: partner.Payees{}, Now: func() time.Time { return c.at }}
			rounds := func() {
				t.Helper()
				for round := 1; round <= 2; round++ { // run twice: nothing happens twice
					if _, err := x.RunOnce(context.Background()); err != nil {
						t.Fatalf("round %d: %v", round, err)
					}
				}
			}
			rounds()
			status := "active"
			if c.expired {
				status = "expired"
			}
			if got, _ := e.Must(200, "GET", lastPath, "", ""); got["status"] != status {
				t.Errorf("contract ending 2026-04-17: got status %v, want %s", got["status"], status)
			}
			expired := events(t, e, "contract_expired")
			if !c.expired && len(expired) != 0 {
				t.Errorf("ContractExpired: got %v, want none", expired)
			}
			if c.expired {
				if len(expired) != 1 {
					t.Fatalf("ContractExpired: got %d, want 1: %v", len(expired), expired)
				}
				want(t, "ContractExpired", expired[0], map[string]any{"contract_id": id(last), "valid_to": "2026-04-17", "expired_on": c.today, "time_zone": c.zone})
			}
			e.act(t, soonPath, "release", "")
			rounds()
			if got, _ := e.Must(200, "GET", soonPath, "", ""); got["expiry_alerted_on"] != c.today {
				t.Errorf("alerted contract: got expiry_alerted_on %v, want %s", got["expiry_alerted_on"], c.today)
			}
			alerts := events(t, e, "contract_expiring")
			byID := map[any]map[string]any{}
			for _, a := range alerts {
				byID[a["contract_id"]] = a
			}
			if byID[soon["id"]] == nil {
				t.Fatalf("ContractExpiring: got %v, want one for %v", alerts, soon["id"])
			}
			want(t, "ContractExpiring", byID[soon["id"]], map[string]any{"days_left": float64(10), "alerted_on": c.today, "time_zone": c.zone})
			var detail map[string]any
			pgtest.Scan(t, `SELECT detail FROM audit.audit_event WHERE tenant_id = $1 AND action = 'procurement.contract.alert_expiry' AND object_id = $2`,
				[]any{e.Tenant.ID, uuidOf(soon)}, &detail)
			want(t, "alert audit", detail, map[string]any{"alerted_on": c.today, "time_zone": c.zone})
		})
	}
}

// absentEntities knows no legal entity.
type absentEntities struct{}

func (absentEntities) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, nil
}

func (absentEntities) OrgUnit(context.Context, uuid.UUID) (tenancy.Unit, bool, error) {
	return tenancy.Unit{}, false, nil
}

// TestContractExpiryUnknownZone: a round whose company's date cannot be
// told fails rather than judging the contracts in UTC: a stored zone this
// build cannot load (validation refuses one, so only a zone database that
// lost it), or a contract's legal entity the directory does not know.
// Nothing is expired or alerted.
func TestContractExpiryUnknownZone(t *testing.T) {
	e := setup(t)
	c, path := e.ending(t, -1) // ended the day before the expirer's
	lost := fixture.Document(t)
	lost.Company("2000").TimeZone = "America/Nowhere"
	for name, tc := range map[string]struct {
		settings procurement.Settings
		entities procurement.LegalEntities
		want     string
	}{
		"a zone that does not load":    {settingsOf{doc: lost}, tenancy.Directory{}, "procurement: company 2000"},
		"a legal entity without entry": {configuration.Settings{}, absentEntities{}, "of active contracts not found"},
	} {
		t.Run(name, func(t *testing.T) {
			x := expirer(t, &pgtest.SyncBuffer{}, tc.settings)
			x.LegalEntities = tc.entities
			if _, err := x.Expire(context.Background(), e.Tenant.ID); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("round: got %v, want an error saying %q", err, tc.want)
			}
		})
	}
	got, _ := e.Must(200, "GET", path, "", "")
	if got["status"] != "active" {
		t.Errorf("contract %s after the failed rounds: got status %v, want active", id(c), got["status"])
	}
	if got["expiry_alerted_on"] != nil {
		t.Errorf("contract %s after the failed rounds: got alerted on %v, want no alert", id(c), got["expiry_alerted_on"])
	}
}

// tenantSettings is the stored configuration, but the lost document for
// one tenant.
type tenantSettings struct {
	lost   uuid.UUID
	doc    *config.Config
	stored configuration.Settings
}

func (s tenantSettings) Document(ctx context.Context) (*config.Config, int32, error) {
	if p, ok := reqctx.PrincipalFrom(ctx); ok && p.TenantID == s.lost {
		return s.doc, 1, nil
	}
	return s.stored.Document(ctx)
}

// TestContractExpiryOtherTenants: a tenant whose round fails on a zone
// that does not load does not hold up the tenants after it in the cell's
// round: their contracts still expire, and the failure is returned
// naming the tenant.
func TestContractExpiryOtherTenants(t *testing.T) {
	failing := setup(t)
	failing.ending(t, -1)
	other := setup(t)
	_, otherPath := other.ending(t, -1)
	if other.Tenant.ID.String() < failing.Tenant.ID.String() {
		t.Fatalf("tenants: got %s before %s, want the failing tenant's round first", other.Tenant.ID, failing.Tenant.ID)
	}
	lost := fixture.Document(t)
	lost.Company("2000").TimeZone = "America/Nowhere"
	x := expirer(t, &pgtest.SyncBuffer{}, tenantSettings{lost: failing.Tenant.ID, doc: lost})
	_, err := x.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), failing.Tenant.ID.String()) {
		t.Errorf("round: got %v, want the failing tenant's error", err)
	}
	if got, _ := other.Must(200, "GET", otherPath, "", ""); got["status"] != "expired" {
		t.Errorf("other tenant's contract: got status %v, want expired", got["status"])
	}
}
