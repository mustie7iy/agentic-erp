package reports_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// usRevenue posts revenue of the amount in USD in company 1000.
func (e env) usRevenue(t *testing.T, amount string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-20", "posting_date": "2026-03-20",
		"currency": "USD", "lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "USD"}},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": "USD"}}]}`,
		e.US, e.Account(t, "1000", "100000"), amount, e.Account(t, "1000", "400000"), amount), "")
}

// shareWith shares the report with the body's person or role and returns
// the share's id and ETag.
func (e env) shareWith(t *testing.T, report, body string) (string, string) {
	t.Helper()
	s, tag := e.Must(201, "POST", "/v1/reports/"+report+"/shares", body, "")
	return apitest.String(t, s, "id"), tag
}

// person is a share's body for the person.
func person(a pgtest.Actor) string { return fmt.Sprintf(`{"principal_id": %q}`, a.ID) }

// roleID is the id of the tenant's role with the code.
func (e env) roleID(t *testing.T, code string) string {
	t.Helper()
	var id string
	pgtest.Scan(t, `SELECT id::text FROM identity.role WHERE tenant_id = $1 AND code = $2`, []any{e.Tenant.ID, code}, &id)
	return id
}

// TestShareWithPerson: a viewer reads and runs a report shared with them
// with their own company access, but does not change or share it; ending
// the share ends their access at once (T95c).
func TestShareWithPerson(t *testing.T) {
	e := setup(t)
	e.usRevenue(t, "300.00")
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	vera := e.Reader(t, "Vera Viewer", e.DE)
	viewer := e.As(vera)
	viewer.Problem(404, "request.not_found", "GET", "/v1/reports/"+id, "", "")
	share, shareTag := e.shareWith(t, id, person(vera))
	got, _ := viewer.Must(200, "GET", "/v1/reports/"+id, "", "")
	if got["access"] != "viewer" || got["owner_id"] != e.Tenant.Human.ID.String() {
		t.Errorf("viewer's report: got access %v of %v, want viewer of the owner's", got["access"], got["owner_id"])
	}
	if mine, _ := e.Must(200, "GET", "/v1/reports/"+id, "", ""); mine["access"] != "owner" {
		t.Errorf("owner's report: got access %v, want owner", mine["access"])
	}
	run, _ := viewer.Must(200, "POST", "/v1/reports/"+id+":run", "", tag)
	if c := cells(t, run); c != "2000 500.00 EUR" {
		t.Errorf("viewer's run: got %s, want company 2000's alone", c)
	}
	run, _ = e.Must(200, "POST", "/v1/reports/"+id+":run", "", tag)
	if c := cells(t, run); c != "1000 300.00 USD, 2000 500.00 EUR" {
		t.Errorf("owner's run: got %s, want both companies'", c)
	}
	for name, c := range map[string]struct{ method, path, body string }{
		"remove":      {"POST", "/v1/reports/" + id + ":remove", ""},
		"schedule":    {"POST", "/v1/reports/" + id + ":schedule", `{"frequency": "daily", "company": "2000"}`},
		"unschedule":  {"POST", "/v1/reports/" + id + ":unschedule", ""},
		"share":       {"POST", "/v1/reports/" + id + "/shares", person(e.Approver)},
		"list shares": {"GET", "/v1/reports/" + id + "/shares", ""},
		"end a share": {"POST", "/v1/reports/" + id + "/shares/" + share + ":end", ""},
	} {
		t.Run(name, func(t *testing.T) {
			ifMatch := tag
			if name == "end a share" {
				ifMatch = shareTag
			}
			viewer.In(t).Problem(403, "analytics.report_not_owner", c.method, c.path, c.body, ifMatch)
		})
	}
	ended, endedTag := e.Must(200, "POST", "/v1/reports/"+id+"/shares/"+share+":end", "", shareTag)
	if ended["status"] != "ended" || endedTag != `"2"` || ended["updated_by"] != e.Tenant.Human.ID.String() {
		t.Errorf("ended share: got %v at %s, want ended at \"2\" by the owner", ended, endedTag)
	}
	viewer.Problem(404, "request.not_found", "GET", "/v1/reports/"+id, "", "")
	viewer.Problem(404, "request.not_found", "POST", "/v1/reports/"+id+":run", "", tag)
	e.Problem(409, "analytics.report_share_ended", "POST", "/v1/reports/"+id+"/shares/"+share+":end", "", endedTag)
	var actions []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action ORDER BY seq), '{}') FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{e.Tenant.ID, share}, &actions)
	if got := strings.Join(actions, " "); got != "analytics.report_share.create analytics.report_share.end" {
		t.Errorf("audit: got %s, want the create and the end", got)
	}
}

// TestShareWithRole: a role's share reaches the people holding it, never
// an agent holding it; the list filters the reports shared with the
// caller from their own.
func TestShareWithRole(t *testing.T) {
	e := setup(t)
	rep, _ := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	vera := e.Reader(t, "Vera Viewer", e.DE)
	viewer := e.As(vera)
	s, _ := e.Must(201, "POST", "/v1/reports/"+id+"/shares", fmt.Sprintf(`{"role_id": %q}`, e.roleID(t, "all")), "")
	if s["role_code"] != "all" || s["role_name"] != "All" || s["principal_id"] != nil || s["report_title"] != "Revenue by company" {
		t.Errorf("role's share: got %v, want the role named and the report's title", s)
	}
	own, _ := viewer.Must(201, "POST", "/v1/reports", reportOf(""), "")
	for filter, want := range map[string]string{"": id + " " + apitest.String(t, own, "id"), "?shared=true": id, "?shared=false": apitest.String(t, own, "id")} {
		page, _ := viewer.Must(200, "GET", "/v1/reports"+filter, "", "")
		var ids []string
		for _, it := range apitest.List(t, page, "items") {
			ids = append(ids, fmt.Sprint(it.(map[string]any)["id"]))
		}
		if got := strings.Join(ids, " "); got != want && got != reverse(want) {
			t.Errorf("viewer's list %s: got %s, want %s", filter, got, want)
		}
	}
	machine, _ := e.Must(201, "POST", "/v1/reports/"+id+"/shares", fmt.Sprintf(`{"role_id": %q}`, e.roleID(t, "machine")), "")
	if machine["status"] != "active" {
		t.Errorf("machine role's share: got %v, want active", machine)
	}
	e.As(e.Tenant.Agent).Problem(404, "request.not_found", "GET", "/v1/reports/"+id, "", "")
	page, _ := e.As(e.Tenant.Agent).Must(200, "GET", "/v1/reports", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("the agent's list: got %v, want none: a share reaches people alone", items)
	}
}

// reverse is a two-id list the other way round: a viewer's list is by
// title, and both reports have the same title.
func reverse(ids string) string {
	f := strings.Fields(ids)
	if len(f) != 2 {
		return ids
	}
	return f[1] + " " + f[0]
}

// TestShareRefusals: a share is with one person or role, never the owner,
// an agent or one the tenant lacks, once at a time, of an active report.
func TestShareRefusals(t *testing.T) {
	e := setup(t)
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	vera := e.Reader(t, "Vera Viewer", e.DE)
	share, _ := e.shareWith(t, id, person(vera))
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND code = 'machine'`, e.Tenant.ID)
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"neither":          {422, "request.validation_failed", `{}`},
		"both":             {422, "request.validation_failed", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, vera.ID, e.roleID(t, "all"))},
		"the owner":        {422, "analytics.report_share_owner", person(e.Tenant.Human)},
		"an agent":         {422, "analytics.report_share_not_person", person(e.Tenant.Agent)},
		"a service":        {422, "analytics.report_share_not_person", person(e.Tenant.Service)},
		"an unknown one":   {422, "request.reference_not_found", fmt.Sprintf(`{"principal_id": %q}`, uuid.New())},
		"an unknown role":  {422, "request.reference_not_found", fmt.Sprintf(`{"role_id": %q}`, uuid.New())},
		"an inactive role": {422, "request.reference_not_found", fmt.Sprintf(`{"role_id": %q}`, e.roleID(t, "machine"))},
		"twice":            {409, "analytics.report_share_exists", person(vera)},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "POST", "/v1/reports/"+id+"/shares", c.body, "")
		})
	}
	e.Problem(412, "request.version_mismatch", "POST", "/v1/reports/"+id+"/shares/"+share+":end", "", `"9"`)
	other, _ := e.save(t, reportOf(""))
	e.Problem(404, "request.not_found", "POST", "/v1/reports/"+apitest.String(t, other, "id")+"/shares/"+share+":end", "", `"1"`)
	e.Must(200, "POST", "/v1/reports/"+id+":remove", "", tag)
	e.Problem(409, "analytics.report_removed", "POST", "/v1/reports/"+id+"/shares", person(e.Approver), "")
	got, _ := e.As(vera).Must(200, "GET", "/v1/reports/"+id, "", "")
	if got["status"] != "removed" {
		t.Errorf("a removed report shared: got %v, want it read as removed", got["status"])
	}
	e.As(vera).Problem(409, "analytics.report_removed", "POST", "/v1/reports/"+id+":run", "", `"2"`)
}

// TestSharePages: a report's shares are listed the earliest first, a
// page at a time, filtered by status, each person named.
func TestSharePages(t *testing.T) {
	e := setup(t)
	rep, _ := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	first, tag := e.shareWith(t, id, person(e.Reader(t, "Ann Early", e.DE)))
	e.shareWith(t, id, person(e.Reader(t, "Bob Late", e.US)))
	e.Must(200, "POST", "/v1/reports/"+id+"/shares/"+first+":end", "", tag)
	var names []string
	path := "/v1/reports/" + id + "/shares?page_size=1"
	for range 3 {
		page, _ := e.Must(200, "GET", path, "", "")
		for _, it := range apitest.List(t, page, "items") {
			s := it.(map[string]any)
			names = append(names, fmt.Sprint(s["principal_name"], " ", s["status"]))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/reports/" + id + "/shares?page_size=1&page_token=" + url.QueryEscape(next)
	}
	if got := strings.Join(names, ", "); got != "Ann Early ended, Bob Late active" {
		t.Errorf("shares: got %s, want Ann's ended then Bob's active", got)
	}
	page, _ := e.Must(200, "GET", "/v1/reports/"+id+"/shares?status=active", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 1 || items[0].(map[string]any)["principal_name"] != "Bob Late" {
		t.Errorf("active shares: got %v, want Bob's", items)
	}
}

// TestSharedRuns: a kept run names the companies it covered and is read
// only by a viewer whose access covers them; a failed run covers none,
// and a run kept before T95c is its report owner's alone.
func TestSharedRuns(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	e.runOnce(t)
	run := e.runs(t, id)[0]
	if got := fmt.Sprint(run["companies"]); got != "[1000 2000]" {
		t.Errorf("run's companies: got %s, want the owner's, 1000 and 2000", got)
	}
	runID := apitest.String(t, run, "id")
	vera, all := e.Reader(t, "Vera Viewer", e.DE), e.Person(t, "Paula Partner")
	e.shareWith(t, id, person(vera))
	e.shareWith(t, id, person(all))
	viewer := e.As(vera)
	page, _ := viewer.Must(200, "GET", "/v1/report-runs?report_id="+id, "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("viewer's runs: got %v, want none: the run covers company 1000", items)
	}
	viewer.Problem(403, "analytics.report_run_access", "GET", "/v1/report-runs/"+runID, "", "")
	if got, _ := e.As(all).Must(200, "GET", "/v1/report-runs/"+runID, "", ""); cells(t, got) != "2000 500.00 EUR" {
		t.Errorf("a viewer of every company: got %v, want the kept results", got)
	}
	failed, legacy := uuid.New(), uuid.New()
	pgtest.Exec(t, `INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, failure_code, failure_detail, companies, permissions)
		VALUES ($1, $2, $3, current_date - 1, 'failed', $4, 'x.refused', 'Refused.', '{}', '{}'),
		       ($1, $5, $3, current_date - 2, 'failed', $4, 'x.refused', 'Refused.', NULL, NULL)`, e.Tenant.ID, failed, id, e.Tenant.Human.ID, legacy)
	page, _ = viewer.Must(200, "GET", "/v1/report-runs?report_id="+id, "", "")
	if items := apitest.List(t, page, "items"); len(items) != 1 || items[0].(map[string]any)["id"] != failed.String() {
		t.Errorf("viewer's runs: got %v, want the failed run alone", items)
	}
	if got, _ := viewer.Must(200, "GET", "/v1/report-runs/"+failed.String(), "", ""); fmt.Sprint(got["companies"]) != "[]" {
		t.Errorf("failed run's companies: got %v, want none", got["companies"])
	}
	e.As(all).Problem(403, "analytics.report_run_access", "GET", "/v1/report-runs/"+legacy.String(), "", "")
	mine, _ := e.Must(200, "GET", "/v1/report-runs/"+legacy.String(), "", "")
	if _, ok := mine["companies"]; ok {
		t.Errorf("a run kept before T95c: got companies %v, want none named", mine["companies"])
	}
	if got := len(e.runs(t, id)); got != 3 {
		t.Errorf("owner's runs: got %d, want 3", got)
	}
}

// role creates an active role with the permissions, held by the people
// from yesterday, and returns its id.
func (e env) role(t *testing.T, code string, permissions []string, holders ...pgtest.Actor) string {
	t.Helper()
	id := uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, $3, $3, 'active', $4, $4)`,
		e.Tenant.ID, id, code, e.Tenant.Human.ID)
	for _, p := range permissions {
		pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`, e.Tenant.ID, id, p, e.Tenant.Human.ID)
	}
	for _, a := range holders {
		pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`, e.Tenant.ID, a.ID, id, e.Tenant.Human.ID)
	}
	return id.String()
}

// TestSharedRunPermissions: a kept run names the permissions its queries
// read their data with, and a viewer of every company who lacks one, or
// may not use one for a segregation-of-duties rule, does not read it, as
// they could not run the report.
func TestSharedRunPermissions(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	e.runOnce(t)
	run := e.runs(t, id)[0]
	if got := fmt.Sprint(run["permissions"]); got != "[ledger.account.read ledger.entry.read partner.supplier.read tenancy.org.read]" {
		t.Errorf("run's permissions: got %s, want the general ledger's, its names' and the organization's", got)
	}
	runID := apitest.String(t, run, "id")
	rita := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	rita.Subject = "human|" + rita.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Rita Reports', true, 'active', $2, $2)`, e.Tenant.ID, rita.ID, pgtest.Issuer(t), rita.Subject)
	e.role(t, "reports_only", []string{"analytics.report.read", "analytics.query.run", "tenancy.org.read"}, rita)
	e.shareWith(t, id, person(rita))
	viewer := e.As(rita)
	page, _ := viewer.Must(200, "GET", "/v1/report-runs?report_id="+id, "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("runs of a viewer without the ledger's read: got %v, want none", items)
	}
	viewer.Problem(403, "analytics.report_run_access", "GET", "/v1/report-runs/"+runID, "", "")
	viewer.Problem(403, "identity.permission_missing", "POST", "/v1/reports/"+id+":run", "", `"2"`)
	paula := e.Person(t, "Paula Partner")
	e.shareWith(t, id, person(paula))
	e.As(paula).Must(200, "GET", "/v1/report-runs/"+runID, "", "")
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'analytics.report.read', 'ledger.entry.read', 'Reports or ledger', current_date - 1, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	e.As(paula).Problem(403, "identity.sod_conflict", "GET", "/v1/report-runs/"+runID, "", "")
}

// TestShareWithInactiveRole: a share with a role reaches its holders
// while the role is active alone.
func TestShareWithInactiveRole(t *testing.T) {
	e := setup(t)
	rep, _ := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	vera := e.Reader(t, "Vera Viewer", e.DE)
	role := e.role(t, "temp_auditors", nil, vera)
	e.shareWith(t, id, fmt.Sprintf(`{"role_id": %q}`, role))
	e.As(vera).Must(200, "GET", "/v1/reports/"+id, "", "")
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE id = $1`, role)
	e.As(vera).Problem(404, "request.not_found", "GET", "/v1/reports/"+id, "", "")
}

// TestShareLocks: two shares of one report at once wait for each other on
// the report's row, and the second is refused; ending a share takes the
// share's row alone, so it does not wait for a change of the report.
func TestShareLocks(t *testing.T) {
	e := setup(t)
	h := endpoints(t, facts{})
	rep, _ := e.save(t, reportOf(""))
	id := uuid.MustParse(apitest.String(t, rep, "id"))
	vera := e.Reader(t, "Vera Viewer", e.DE)
	in := reports.ShareSet{PrincipalID: &vera.ID}
	share := func(ctx context.Context) error { _, err := h.CreateShare(ctx, id, in); return err }
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, share, share)
	if first != nil || problem.From(second).Code != "analytics.report_share_exists" {
		t.Errorf("two shares at once: got %v and %v, want the first made and the second refused", first, second)
	}
	var shareID uuid.UUID
	pgtest.Scan(t, `SELECT id FROM analytics.report_share WHERE report_id = $1`, []any{id}, &shareID)
	removed, hold := make(chan struct{}), make(chan struct{})
	_, done := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if _, err := h.Remove(ctx, id, etag.IfMatch(`"1"`)); err != nil {
			close(removed)
			return err
		}
		close(removed)
		<-hold
		return nil
	})
	<-removed
	ended := make(chan error, 1)
	go func() {
		ended <- apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.EndShare(ctx, id, shareID, etag.IfMatch(`"1"`))
			return err
		})
	}()
	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("end while the report is being removed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("end while the report is being removed: it waited for the report's lock")
	}
	close(hold)
	if err := <-done; err != nil {
		t.Errorf("remove: %v", err)
	}
}

// grantees fail on the call named.
type grantees struct {
	reports.Grantees
	fail string
}

var errGrantees = errors.New("identity down")

func (g grantees) Kinds(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	if g.fail == "kinds" {
		return nil, errGrantees
	}
	return g.Grantees.Kinds(ctx, ids)
}

func (g grantees) RolesHeld(ctx context.Context, p uuid.UUID) ([]uuid.UUID, error) {
	if g.fail == "held" {
		return nil, errGrantees
	}
	return g.Grantees.RolesHeld(ctx, p)
}

func (g grantees) Roles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]reports.Role, error) {
	if g.fail == "roles" {
		return nil, errGrantees
	}
	return g.Grantees.Roles(ctx, ids)
}

// companiesFailing fails to name companies.
type companiesFailing struct{}

func (companiesFailing) Codes(context.Context, authz.Access) ([]string, error) {
	return nil, errGrantees
}

// TestSharesDatabaseFailures calls the shares' handlers outside a unit of
// work and with each of their statements failing, and with each port
// failing.
func TestSharesDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, facts{})
	rep, _ := e.save(t, reportOf(""))
	id := uuid.MustParse(apitest.String(t, rep, "id"))
	vera, ann := e.Reader(t, "Vera Viewer", e.DE), e.Reader(t, "Ann Other", e.DE)
	share, _ := e.shareWith(t, id.String(), person(vera))
	role := uuid.MustParse(e.roleID(t, "all"))
	callsOf := func(h *reports.Endpoints) map[string]func(ctx context.Context) error {
		return map[string]func(ctx context.Context) error{
			"share": func(ctx context.Context) error {
				_, err := h.CreateReportShare(ctx, &gen.ReportShareCreate{PrincipalID: gen.NewOptUUID(ann.ID)}, gen.CreateReportShareParams{ID: id})
				return err
			},
			"share with a role": func(ctx context.Context) error {
				_, err := h.CreateReportShare(ctx, &gen.ReportShareCreate{RoleID: gen.NewOptUUID(role)}, gen.CreateReportShareParams{ID: id})
				return err
			},
			"end": func(ctx context.Context) error {
				_, err := h.EndReportShare(ctx, gen.EndReportShareParams{ID: id, ShareID: uuid.MustParse(share), IfMatch: `"1"`})
				return err
			},
			"list": func(ctx context.Context) error {
				_, err := h.ListReportShares(ctx, gen.ListReportSharesParams{ID: id})
				return err
			},
		}
	}
	for name, call := range callsOf(h) {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("list page size", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListReportShares(ctx, gen.ListReportSharesParams{ID: id, PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	for _, fail := range []string{"kinds", "held", "roles"} {
		t.Run("grantees failing on "+fail, func(t *testing.T) {
			h := endpoints(t, facts{})
			h.Grantees = grantees{Grantees: h.Grantees, fail: fail}
			calls := callsOf(h)
			call := calls["share"]
			if fail == "roles" {
				call = calls["share with a role"]
			}
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); !errors.Is(err, errGrantees) {
				t.Errorf("got %v, want the port's failure", err)
			}
			if fail == "kinds" {
				return
			}
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, calls["list"]); !errors.Is(err, errGrantees) {
				t.Errorf("list: got %v, want the port's failure", err)
			}
		})
	}
	t.Run("no names", func(t *testing.T) {
		h := endpoints(t, facts{})
		h.People = people{}
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, callsOf(h)["list"]); !errors.Is(err, errPeople) {
			t.Errorf("got %v, want the names' failure", err)
		}
	})
	t.Run("no audit buffer", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			tx, _ := reqctx.TxFrom(ctx)
			p, _ := reqctx.PrincipalFrom(ctx)
			return callsOf(h)["share"](reqctx.WithPrincipal(reqctx.WithTx(context.Background(), tx), p))
		})
		if !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("got %v, want the audit's failure", err)
		}
	})
	t.Run("no companies", func(t *testing.T) {
		h := endpoints(t, facts{})
		h.Companies = companiesFailing{}
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListReportRuns(ctx, gen.ListReportRunsParams{})
			return err
		})
		if !errors.Is(err, errGrantees) {
			t.Errorf("got %v, want the port's failure", err)
		}
	})
}
