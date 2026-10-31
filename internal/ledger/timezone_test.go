package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// zoned is an active configuration whose company 2000 is in the zone.
type zoned string

func (z zoned) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: string(z)}}}, 1, nil
}

// zoneCase is a company's zone and an instant whose date there differs
// from UTC's: 22:00 on 16 April in Toronto is 02:00 on the 17th in UTC,
// 01:00 on 17 April in Dubai is 21:00 on the 16th in UTC.
type zoneCase struct {
	zone  zoned
	at    time.Time
	today string // the company's date at the instant
}

var (
	toronto = zoneCase{"America/Toronto", time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC), "2026-04-16"}
	dubai   = zoneCase{"Asia/Dubai", time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), "2026-04-17"}
)

func (z zoneCase) now() time.Time { return z.at }

// TestCenterChangesOnTheCompanysDate: a change takes effect from the
// company's date in its time zone, not UTC's (product owner 2026-10-31,
// T72n4). A lock of a default center up to 17 April (exclusive) leaves
// it locked on the 16th: refused in Toronto at 22:00 on the 16th, which
// is already the 17th in UTC, and accepted in Dubai at 01:00 on the 17th,
// still the 16th in UTC; at the request and again at the approval. The
// refusal names the date and the zone.
func TestCenterChangesOnTheCompanysDate(t *testing.T) {
	a := setupAssigned(t)
	until := gen.NewOptDate(date("2026-04-17"))
	for name, lock := range map[string]func(ctx context.Context, h *ledger.Endpoints) (map[string]any, error){
		"a default cost center": func(ctx context.Context, h *ledger.Endpoints) (map[string]any, error) {
			res, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-04-01"), ValidTo: until, LockActualCost: gen.NewOptBool(true), Reason: "r"},
				gen.LockCostCenterParams{ID: a.cc1000, IfMatch: "*"})
			ch, _ := res.(*gen.LockCostCenterOKHeaders)
			return changeOf(ch), err
		},
		"a default profit center": func(ctx context.Context, h *ledger.Endpoints) (map[string]any, error) {
			res, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-04-01"), ValidTo: until, Locked: true, Reason: "r"},
				gen.LockProfitCenterParams{ID: a.dummy, IfMatch: "*"})
			ch, _ := res.(*gen.LockCostCenterOKHeaders)
			return changeOf(ch), err
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := func(z zoneCase) *ledger.Endpoints {
				h := endpoints()
				h.Settings, h.Now = z.zone, z.now
				return h
			}
			var change map[string]any
			err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				_, err := lock(ctx, in(toronto))
				wantDefaultInUse(t, err, toronto)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
				change, err = lock(ctx, in(dubai))
				return err
			}); err != nil {
				t.Fatalf("requested in Dubai at 01:00 on the 17th: got %v, want it accepted", err)
			}
			approve := func(z zoneCase) error {
				return apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
					h := ledger.CenterChanges{LegalEntities: tenancy.Directory{}, Settings: z.zone, Now: z.now}
					return h.Approved(ctx, uuid.MustParse(change["id"].(string)), uuid.MustParse(change["workflow_request_id"].(string)))
				})
			}
			wantDefaultInUse(t, approve(toronto), toronto)
			if err := approve(dubai); err != nil {
				t.Errorf("approved in Dubai at 01:00 on the 17th: got %v, want it applied", err)
			}
		})
	}
}

// changeOf is the center change a request opened, as the API returns it.
func changeOf(h *gen.LockCostCenterOKHeaders) map[string]any {
	if h == nil {
		return nil
	}
	return map[string]any{"id": h.Response.ID.String(), "workflow_request_id": h.Response.WorkflowRequestID.Value.String()}
}

// wantDefaultInUse checks the refusal of a change leaving a default
// center locked on the company's date, which its detail names.
func wantDefaultInUse(t *testing.T, err error, z zoneCase) {
	t.Helper()
	p := problem.From(err)
	if p.Code != "ledger.default_center_in_use" {
		t.Fatalf("in %s on %s: got %v, want ledger.default_center_in_use", z.zone, z.today, err)
	}
	if want := "is locked from " + z.today + " (the company's date in " + string(z.zone) + ")"; !strings.Contains(p.Detail, want) {
		t.Errorf("detail: got %q, want it to say %q", p.Detail, want)
	}
}

// TestCenterChangesWithoutZones: a handler wired without the
// configuration cannot tell the company's date, so a change it would
// judge on it fails rather than judging it in UTC.
func TestCenterChangesWithoutZones(t *testing.T) {
	a := setupAssigned(t)
	h := endpoints()
	h.Settings = nil
	err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		_, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-04-01"), LockActualCost: gen.NewOptBool(true), Reason: "r"},
			gen.LockCostCenterParams{ID: a.cc1000, IfMatch: "*"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Errorf("cost center: got %v, want the missing time zones named", err)
	}
	err = apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		_, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-04-01"), Locked: true, Reason: "r"},
			gen.LockProfitCenterParams{ID: a.dummy, IfMatch: "*"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Errorf("profit center: got %v, want the missing time zones named", err)
	}
}

// TestControllingDefaultsOnTheCompanysDate: the setup judges a default
// center's validity on the company's date in the zone the document gives
// it (T72n4). LATER is valid from 1 June: at 22:00 on 31 May in Toronto
// (1 June in UTC) it is not valid yet, at 01:00 on 1 June in Dubai (31 May
// in UTC) it is. A zone the binary cannot load leaves the defaults
// unchecked (validation reports the zone) and fails the application.
func TestControllingDefaultsOnTheCompanysDate(t *testing.T) {
	c := setupCenters(t)
	c.profitCenter(t, "DUMMY", "", c.de)
	later, _ := c.Must(201, "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "LATER", "name": "L",
		"responsible": "J", "category": "ADMIN", "valid_from": "2026-06-01", "reason": "r"}`, c.de)), "")
	c.decide(t, later["pending_change_id"], "approve")
	doc := func(zone string) *config.Config {
		return controlling(t, func(_ *config.Config, co *config.Company) {
			co.DefaultAssignments[0].CostCenter, co.TimeZone = "LATER", zone
		})
	}
	check := func(zone string, at time.Time) ([]config.Finding, error) {
		var out []config.Finding
		err := apitest.Committed(t, c.ten, c.ten.Human, func(ctx context.Context) (err error) {
			out, err = ledger.Setup{LegalEntities: tenancy.Directory{}, Now: func() time.Time { return at }}.Check(ctx, nil, doc(zone))
			return err
		})
		return out, err
	}
	const path = "companies[2000].default_assignments[520000].cost_center"
	fs, err := check("America/Toronto", time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, fs, path, "LATER is not valid from 2026-05-31 (the company's date in America/Toronto)")
	fs, err = check("Asia/Dubai", time.Date(2026, 5, 31, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(fs, func(f config.Finding) bool { return f.Path == path }); i >= 0 {
		t.Errorf("in Dubai on 1 June: got %v, want LATER usable", fs[i])
	}
	fs, err = check("America/Nowhere", time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Errorf("checking an unknown zone: got %v, want findings", err)
	}
	if i := slices.IndexFunc(fs, func(f config.Finding) bool { return f.Path == path }); i >= 0 {
		t.Errorf("an unknown zone: got %v, want the defaults left unchecked", fs[i])
	}
	err = apitest.Committed(t, c.ten, c.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc("America/Nowhere"))
	})
	if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("applying an unknown zone: got %v, want the zone named", err)
	}
}

// TestDefaultInUseHidesOtherCompanies: a refusal names the company's date
// and zone only to a caller who can access the company: a requester
// locking a profit center that an inaccessible company defaults to, and
// an approver deciding a change of a cost center in one, are told it is
// locked on the date of a company they cannot access.
func TestDefaultInUseHidesOtherCompanies(t *testing.T) {
	a := setupAssigned(t)
	h := endpoints()
	h.Settings, h.Now = toronto.zone, toronto.now
	var change map[string]any
	if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		dubaiH := endpoints()
		dubaiH.Settings, dubaiH.Now = dubai.zone, dubai.now
		res, err := dubaiH.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-04-01"), ValidTo: gen.NewOptDate(date("2026-04-17")),
			LockActualCost: gen.NewOptBool(true), Reason: "r"}, gen.LockCostCenterParams{ID: a.cc1000, IfMatch: "*"})
		ch, _ := res.(*gen.LockCostCenterOKHeaders)
		change = changeOf(ch)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE tenant_id = $1 AND id = $2`, a.ten.ID, a.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, a.ten.ID, a.ten.Human.ID, a.us)
	const hidden = "the date of a company you cannot access"
	wantHidden := func(what string, err error) {
		t.Helper()
		p := problem.From(err)
		if p.Code != "ledger.default_center_in_use" {
			t.Fatalf("%s: got %v, want ledger.default_center_in_use", what, err)
		}
		if !strings.Contains(p.Detail, hidden) {
			t.Errorf("%s: detail %q, want it to say %q", what, p.Detail, hidden)
		}
		if strings.Contains(p.Detail, string(toronto.zone)) || strings.Contains(p.Detail, toronto.today) {
			t.Errorf("%s: detail %q names the company's zone or date", what, p.Detail)
		}
	}
	err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		_, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-04-01"), ValidTo: gen.NewOptDate(date("2026-04-17")), Locked: true, Reason: "r"},
			gen.LockProfitCenterParams{ID: a.dummy, IfMatch: "*"})
		return err
	})
	wantHidden("the profit center's lock", err)
	err = apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		h := ledger.CenterChanges{LegalEntities: tenancy.Directory{}, Settings: toronto.zone, Now: toronto.now}
		return h.Approved(ctx, uuid.MustParse(change["id"].(string)), uuid.MustParse(change["workflow_request_id"].(string)))
	})
	wantHidden("the cost center lock's approval", err)
}
