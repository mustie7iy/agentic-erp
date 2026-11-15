package watchers_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

var alertPerms = []string{"analytics.alert.read", "analytics.alert.acknowledge"}

// TestDeviationWatcher: a deviation compares the latest month with the
// average of the months before it, each with a figure.
func TestDeviationWatcher(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct{ periods, series string }{
		"two months":   {"2", "[2000 EUR 500.00 150.00 233.33 true]"},
		"three months": {"3", "[2000 EUR 500.00 - - false]"},
	} {
		t.Run(name, func(t *testing.T) {
			w := watch(t, e.Client, `{"name": "`+name+`", "metric": "revenue", "company": "2000", "grain": "month",
				"condition": {"kind": "deviation", "percent": "100", "periods": `+c.periods+`}}`)
			if got := fmt.Sprint(w["condition"]); got != "map[kind:deviation percent:100 periods:"+c.periods+"]" {
				t.Errorf("condition: got %s, want the deviation's settings", got)
			}
			if got := fmt.Sprint(series(evaluate(t, e.Client, w))); got != c.series {
				t.Errorf("series: got %s, want %s", got, c.series)
			}
		})
	}
	for name, body := range map[string]string{
		"no periods":          `{"kind": "deviation", "percent": "1"}`,
		"periods of a change": `{"kind": "change", "percent": "1", "periods": 2}`,
	} {
		t.Run(name, func(t *testing.T) {
			e.Problem(422, "analytics.invalid_watcher", "POST", "/v1/metric-watchers", `{"name": "x", "metric": "revenue", "grain": "month", "condition": `+body+`}`, "")
		})
	}
}

// TestAlerts: an alert is listed and read by a caller whose access covers
// its companies and who may read its metric's data; it is acknowledged
// once, at its version, by a person, with an audit record.
func TestAlerts(t *testing.T) {
	e := setup(t, alertPerms...)
	w := watch(t, e.Client, `{"name": "Above", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	alert := evaluate(t, e.Client, w)["alert"].(map[string]any)
	id := apitest.String(t, alert, "id")
	if alert["raised_as_name"] == "" || alert["acknowledged_at"] != nil {
		t.Errorf("alert: got %v, want its creator's name, open", alert)
	}
	page, _ := e.Must(200, "GET", "/v1/metric-alerts", "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != id {
		t.Errorf("list: got %v, want the alert", items)
	}
	us := e.As(e.Reader(t, "Una US", e.US))
	if page, _ := us.Must(200, "GET", "/v1/metric-alerts", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("a reader of 1000 alone: got %v, want no alert of 2000", page["items"])
	}
	us.Problem(404, "request.not_found", "GET", "/v1/metric-alerts/"+id, "", "")
	us.Problem(404, "request.not_found", "POST", "/v1/metric-alerts/"+id+":acknowledge", "", `"1"`)
	blind := e.As(e.alertsOnly(t))
	if page, _ := blind.Must(200, "GET", "/v1/metric-alerts", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("a reader without the journal's read: got %v, want no alert of revenue", page["items"])
	}
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", "/v1/metric-alerts/"+id+":acknowledge", "", `"1"`)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/metric-alerts/"+id+":acknowledge", "", `"2"`)
	acked, tag := e.Must(200, "POST", "/v1/metric-alerts/"+id+":acknowledge", "", `"1"`)
	if acked["status"] != "acknowledged" || acked["acknowledged_by"] != e.Tenant.Human.ID.String() || acked["acknowledged_by_name"] == nil ||
		acked["acknowledged_at"] == nil || tag != `"2"` {
		t.Errorf("acknowledged: got %v %s, want acknowledged by the caller at version 2", acked, tag)
	}
	e.Problem(409, "analytics.alert_acknowledged", "POST", "/v1/metric-alerts/"+id+":acknowledge", "", `"2"`)
	got, _ := e.Must(200, "GET", "/v1/metric-alerts/"+id, "", "")
	if got["status"] != "acknowledged" {
		t.Errorf("get: got %v, want acknowledged", got)
	}
	for query, want := range map[string]int{"?status=open": 0, "?status=acknowledged": 1, "?watcher_id=" + apitest.String(t, w, "id"): 1,
		"?watcher_id=" + e.Tenant.ID.String(): 0} {
		if page, _ := e.Must(200, "GET", "/v1/metric-alerts"+query, "", ""); len(page["items"].([]any)) != want {
			t.Errorf("list%s: got %d, want %d", query, len(page["items"].([]any)), want)
		}
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'analytics.metric_alert.acknowledge'`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records: got %d, want the acknowledgement's", audits)
	}
}

// TestAlertPages: the alerts are listed the latest first, a page at a
// time.
func TestAlertPages(t *testing.T) {
	e := setup(t, alertPerms...)
	var want []any
	for _, month := range []string{"1", "2", "3"} {
		w := watch(t, e.Client, `{"name": "`+month+`", "metric": "revenue", "company": "2000", "grain": "month",
			"filters": [{"dim": "fiscal_month", "op": "in", "values": ["`+month+`"]}], "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
		want = append([]any{evaluate(t, e.Client, w)["alert"].(map[string]any)["id"]}, want...)
	}
	var got []any
	token := ""
	for {
		page, _ := e.Must(200, "GET", "/v1/metric-alerts?page_size=2"+token, "", "")
		for _, a := range page["items"].([]any) {
			got = append(got, a.(map[string]any)["id"])
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("alerts: got %v, want %v, the latest first", got, want)
	}
}

// alertsOnly is a person in every company holding analytics.alert.read
// alone: no read of any metric's data.
func (e env) alertsOnly(t *testing.T) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	role := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Al Alerts', true, 'active', $2, $2)`, e.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, 'alerts_only', 'Alerts', 'active', $3, $3)`,
		e.Tenant.ID, role, a.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, 'analytics.alert.read', current_date - 1, $3)`, e.Tenant.ID, role, a.ID)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.Tenant.ID, a.ID, role)
	return a
}
