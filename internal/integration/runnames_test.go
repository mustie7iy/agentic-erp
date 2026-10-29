package integration_test

import (
	"testing"
)

// TestRunsNameTheirConnector: a migration run gives its connector's name
// beside its id, on create, read and every list page, as the connector is
// named now (ADR-0038 As built, review:batch24); an empty page asks for no
// names.
func TestRunsNameTheirConnector(t *testing.T) {
	e := setup(t)
	empty, _ := e.Must(200, "GET", "/v1/migration-runs", "", "")
	if n := len(empty["items"].([]any)); n != 0 {
		t.Fatalf("runs before any: got %d, want 0", n)
	}
	prd, qas := e.connector(t, "extract.json"), e.connector(t, "qas.json")
	first := e.run(t, prd, "documents", true, e.DE.String())
	second := e.run(t, qas, "balances", true, e.DE.String())
	for name, c := range map[string]struct {
		run  map[string]any
		want string
	}{"first": {first, "SAP extract.json"}, "second": {second, "SAP qas.json"}} {
		t.Run(name, func(t *testing.T) {
			if got := c.run["connector_name"]; got != c.want {
				t.Errorf("created run's connector_name: got %v, want %s", got, c.want)
			}
		})
	}
	_, tag := e.Must(200, "GET", "/v1/connectors/"+qas, "", "")
	e.Must(200, "PATCH", "/v1/connectors/"+qas, `{"name": "SAP QAS"}`, tag)
	got, _ := e.Must(200, "GET", "/v1/migration-runs/"+second["id"].(string), "", "")
	if got["connector_name"] != "SAP QAS" {
		t.Errorf("read run's connector_name: got %v, want SAP QAS", got["connector_name"])
	}
	want := map[string]string{first["id"].(string): "SAP extract.json", second["id"].(string): "SAP QAS"}
	seen := 0
	for token := ""; ; {
		page, _ := e.Must(200, "GET", "/v1/migration-runs?page_size=1"+token, "", "")
		for _, item := range page["items"].([]any) {
			r := item.(map[string]any)
			seen++
			if r["connector_name"] != want[r["id"].(string)] {
				t.Errorf("listed run %v connector_name: got %v, want %s", r["id"], r["connector_name"], want[r["id"].(string)])
			}
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	if seen != 2 {
		t.Errorf("listed runs: got %d, want 2", seen)
	}
}
