package closing_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// separated leaves company 2000's sign_off_separation out, so its closes
// take the default: signed off by someone who completed none of their
// tasks (ADR-0058 As built T64o).
func separated(doc *config.Config) {
	doc.Company("2000").Close.SignOffSeparation = nil
}

// separation is what the audit records of the tenant's sign-offs of the
// action say of the separation, joined by commas in the order written.
func separation(t *testing.T, e fixture.Env, action string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(string_agg(detail->>'sign_off_separation', ',' ORDER BY occurred_at, seq), '') FROM audit.audit_event
		WHERE tenant_id = $1 AND action = $2`, []any{e.Tenant.ID, action}, &got)
	return got
}

// TestSignOffSeparated: where the company separates them, a person who
// completed a task of the period close is refused its sign-off (409,
// naming the tasks), before the checks run, so nothing is kept; another
// person signs it off, and the audit record says the separation applied
// (T64o, product owner 2026-11-04).
func TestSignOffSeparated(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) { withSignOff(doc); separated(doc) }, signPermissions...)
	s := signable(t, e, 1)
	s.periods(t, "close")
	before := s.read(t)
	p := e.Problem(409, "closing.signer_is_preparer", "POST", s.path+":signOff", "", s.tag)
	want := "You completed task AP_CUTOFF, REVIEW of this close; company 2000's closes are signed off by someone who completed none of their tasks " +
		"(companies[].close.sign_off_separation). Another person signs it off."
	if p["detail"] != want {
		t.Errorf("the preparer's sign-off: got %v, want %s", p["detail"], want)
	}
	after := s.read(t)
	if after["version"] != before["version"] {
		t.Errorf("the close's version after the refusal: got %v, want %v", after["version"], before["version"])
	}
	if task := taskOf(t, after, "NO_PARKED"); task["check_result"] != nil {
		t.Errorf("the check after the refusal: got %v recorded, want it not run", task["check_result"])
	}
	fiona := e.Person(t, "Fiona Finance Director")
	var tag string
	off, tag := e.As(fiona).Must(200, "POST", s.path+":signOff", `{"note": "Signed off by someone who prepared none of it"}`, s.tag)
	wantFields(t, "the sign-off", off, map[string]any{"signed_by": fiona.ID.String(), "signed_by_name": "Fiona Finance Director"})
	if task := taskOf(t, s.read(t), "NO_PARKED"); task["completed_by"] != fiona.ID.String() {
		t.Errorf("NO_PARKED, first passed by the sign-off's checks: got completed by %v, want the signer %s", task["completed_by"], fiona.ID)
	}
	s.tag = tag
	// An unlock returns the periods to closed (T32); the signer signs off
	// again: the check her sign-off completed is not a preparation of hers
	// (review round 1).
	pgtest.Unguarded(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 1`, e.Tenant.ID, e.DE)
	s.read(t)
	e.As(fiona).Must(200, "POST", s.path+":signOff", `{"note": "Signed off again after the unlock"}`, s.tag)
	if got := separation(t, e, "closing.period_close.sign_off"); got != "true,true" {
		t.Errorf("the audit records' sign_off_separation: got %q, want true,true", got)
	}
}

// TestSignOffSeparatedDatabaseFailures: every statement of a separated
// sign-off by a person who prepared none of the close failing, the
// sign-off fails with it.
func TestSignOffSeparatedDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) { withSignOff(doc); separated(doc) }, signPermissions...)
	s := signable(t, e, 1)
	s.periods(t, "close")
	s.read(t)
	id := uuid.MustParse(strings.TrimPrefix(s.path, "/v1/period-closes/"))
	h := endpoints(t)
	apitest.EveryFailure(t, e.Tenant, e.Person(t, "Fiona Finance Director"), "sign off", func(ctx context.Context) error {
		_, err := h.SignOffPeriodClose(ctx, gen.NewOptPeriodCloseSignOffInput(gen.PeriodCloseSignOffInput{}), gen.SignOffPeriodCloseParams{ID: id, IfMatch: s.tag})
		return err
	})
}

// TestYearSignOffSeparated: a year close's preparers are the people who
// completed its tasks as they stand: one whose task was reopened and
// completed by another no longer counts for it, and a check counts for
// the person whose run passed it first. Each preparer is refused, naming
// their own tasks; a person who prepared none signs off.
func TestYearSignOffSeparated(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) { withYearSignOff(doc); separated(doc) }, signPermissions...)
	s := yearSignable(t, e)
	s.task(t, 200, "NO_PARKED:check", "")
	lockRegular(t, e, "0L", 12)
	lockRegular(t, e, "2L", 12)
	s.read(t)
	fiona := e.As(e.Person(t, "Fiona Finance Director"))
	_, s.tag = fiona.Must(200, "POST", s.path+"/tasks/TAX:reopen", `{"reason": "The provision changes"}`, s.tag)
	_, s.tag = fiona.Must(200, "POST", s.path+"/tasks/TAX:complete", "", s.tag)
	for _, tc := range []struct {
		name  string
		as    apitest.Client
		tasks string
	}{
		{"the first preparer, TAX no longer hers", e.Client, "AP_CUTOFF, NO_PARKED"},
		{"the one who completed TAX again", fiona, "TAX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.as.Problem(409, "closing.signer_is_preparer", "POST", s.path+":signOff", "", s.tag)
			want := "You completed task " + tc.tasks + " of this close; company 2000's closes are signed off by someone who completed none of their tasks " +
				"(companies[].close.sign_off_separation). Another person signs it off."
			if p["detail"] != want {
				t.Errorf("the sign-off: got %v, want %s", p["detail"], want)
			}
		})
	}
	gus := e.Person(t, "Gus Group Controller")
	off, tag := e.As(gus).Must(200, "POST", s.path+":signOff", `{"note": "2026 signed off"}`, s.tag)
	wantFields(t, "the sign-off", off, map[string]any{"signed_by": gus.ID.String(), "signed_by_name": "Gus Group Controller"})
	s.tag = tag
	pgtest.Unguarded(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 13`, e.Tenant.ID, e.DE)
	s.read(t)
	e.As(gus).Must(200, "POST", s.path+":signOff", `{"note": "Signed off again after the unlock"}`, s.tag)
	if got := separation(t, e, "closing.year_close.sign_off"); got != "true,true" {
		t.Errorf("the audit records' sign_off_separation: got %q, want true,true", got)
	}
}
