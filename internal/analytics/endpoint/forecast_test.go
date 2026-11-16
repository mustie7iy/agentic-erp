package endpoint_test

import (
	"fmt"
	"strings"
	"testing"
)

// points are a forecast's points as "label value currency", with "*" for
// a history period with data.
func points(f map[string]any, key string) string {
	var out []string
	for _, p := range f[key].([]any) {
		pt := p.(map[string]any)
		s := fmt.Sprint(pt["label"], " ", pt["value"], " ", pt["currency"])
		if pt["has_data"] == true {
			s += "*"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// TestForecast: a metric's history is answered as the caller's query
// and forecast by the average of its last months, exactly; a month
// without data is zero (T97a).
func TestForecast(t *testing.T) {
	e := seeded(t, perms...)
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-01-15", "posting_date": "2026-01-15",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "100.01", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-100.01", "currency": "EUR"}}]}`, e.DE, e.Account(t, "2000", "100000"), e.Account(t, "2000", "400000")), "")
	f, _ := e.Must(200, "POST", "/v1/analytics-forecasts",
		`{"metric": "revenue", "companies": ["2000"], "grain": "month", "periods_ahead": 2, "method": "moving_average", "window": 3}`, "")
	if got := points(f, "history"); got != "2026-01 100.01 EUR*, 2026-02 0.00 EUR, 2026-03 500.00 EUR*" {
		t.Errorf("history: got %s, want January and March with February at zero", got)
	}
	// (100.01 + 0 + 500.00) / 3 = 200.003..., rounded to the cent.
	if got := points(f, "forecast"); got != "2026-04 200.00 EUR, 2026-05 200.00 EUR" {
		t.Errorf("forecast: got %s, want the three months' average", got)
	}
	for k, want := range map[string]any{"currency": "EUR", "type": "money", "metric_label": "Revenue", "history_total": "600.01", "forecast_total": "400.00",
		"window": float64(3), "source": "database", "data_through": "2026-03-20", "method": "moving_average"} {
		if f[k] != want {
			t.Errorf("%s: got %v, want %v", k, f[k], want)
		}
	}
	if got := fmt.Sprint(f["used"]); got != "[2026-01 2026-02 2026-03]" || !strings.Contains(fmt.Sprint(f["basis"]), "arithmetic projection") {
		t.Errorf("used and basis: got %s, %v", got, f["basis"])
	}
	y, _ := e.Must(200, "POST", "/v1/analytics-forecasts",
		`{"metric": "revenue", "where": [{"dim": "company", "op": "in", "values": ["2000"]}], "grain": "year", "periods_ahead": 1, "method": "moving_average", "window": 1}`, "")
	if got := points(y, "history") + " / " + points(y, "forecast"); got != "2026 600.01 EUR* / 2027 600.01 EUR" {
		t.Errorf("by year: got %s, want 2026's revenue carried to 2027", got)
	}
}

// TestForecastRefusals: what the semantic layer or the method refuses is
// a problem naming it.
func TestForecastRefusals(t *testing.T) {
	e := seeded(t, perms...)
	for name, c := range map[string]struct{ body, code string }{
		"two currencies":     {`{"metric": "revenue", "grain": "month", "periods_ahead": 1, "method": "moving_average", "window": 1}`, "analytics.invalid_query"},
		"an unknown metric":  {`{"metric": "nope", "grain": "month", "periods_ahead": 1, "method": "moving_average", "window": 1}`, "analytics.invalid_query"},
		"a ratio":            {`{"metric": "operating_margin", "grain": "month", "periods_ahead": 1, "method": "moving_average", "window": 1}`, "analytics.forecast_ratio"},
		"no window":          {`{"metric": "revenue", "grain": "month", "periods_ahead": 1, "method": "moving_average"}`, "request.validation_failed"},
		"too short a window": {`{"metric": "revenue", "companies": ["2000"], "grain": "month", "periods_ahead": 1, "method": "moving_average", "window": 2}`, "analytics.forecast_history_short"},
		"no year before": {`{"metric": "revenue", "companies": ["2000"], "grain": "quarter", "periods_ahead": 1, "method": "same_period_last_year"}`,
			"analytics.forecast_history_short"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, c.code, "POST", "/v1/analytics-forecasts", c.body, "")
		})
	}
}

// TestForecastScenario: a scenario raises one account's forecast by ten
// percent; the answer gives the baseline, the scenario and their
// difference per period ahead and in total, and the adjustment's part;
// an adjustment on a dimension the metric lacks or two on one row are
// refused (T97b).
func TestForecastScenario(t *testing.T) {
	e := seeded(t, perms...)
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-01-15", "posting_date": "2026-01-15",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "100.01", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-100.01", "currency": "EUR"}}]}`, e.DE, e.Account(t, "2000", "100000"), e.Account(t, "2000", "400000")), "")
	const ask = `{"metric": "revenue", "companies": ["2000"], "grain": "month", "periods_ahead": 2, "method": "moving_average", "window": 3, "scenario": %s}`
	f, _ := e.Must(200, "POST", "/v1/analytics-forecasts", fmt.Sprintf(ask, `{"adjustments": [{"dimension": "account", "value": "400000", "percent": "10"}]}`), "")
	if got := points(f, "forecast"); got != "2026-04 200.00 EUR, 2026-05 200.00 EUR" {
		t.Errorf("forecast: got %s, want the baseline", got)
	}
	sc, _ := f["scenario"].(map[string]any)
	var periods []string
	for _, p := range sc["periods"].([]any) {
		pt := p.(map[string]any)
		periods = append(periods, fmt.Sprint(pt["label"], " ", pt["baseline"], " ", pt["scenario"], " ", pt["difference"], " ", pt["currency"], " ", pt["fiscal_year"], " ", pt["period"]))
	}
	if got := strings.Join(periods, ", "); got != "2026-04 200.00 220.00 20.00 EUR 2026 4, 2026-05 200.00 220.00 20.00 EUR 2026 5" {
		t.Errorf("scenario's periods: got %s", got)
	}
	if got := fmt.Sprint(sc["baseline_total"], " ", sc["scenario_total"], " ", sc["difference_total"], " ", sc["currency"]); got != "400.00 440.00 40.00 EUR" {
		t.Errorf("scenario's totals: got %s", got)
	}
	part, _ := sc["adjustments"].([]any)[0].(map[string]any)
	if got := fmt.Sprint(part["dimension"], " ", part["value"], " ", part["percent"], " ", part["baseline_total"], " ", part["scenario_total"]); got != "account 400000 10 400.00 440.00" {
		t.Errorf("the adjustment's part: got %s", got)
	}
	for name, c := range map[string]struct{ scenario, code, field string }{
		"a dimension revenue lacks": {`{"adjustments": [{"dimension": "supplier", "value": "S1", "percent": "10"}]}`, "request.validation_failed", "scenario.adjustments[0].dimension"},
		"two on one row": {`{"adjustments": [{"dimension": "account", "value": "400000", "percent": "10"}, {"dimension": "company", "value": "2000", "percent": "5"}]}`,
			"analytics.scenario_overlap", "scenario.adjustments[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.In(t).Problem(422, c.code, "POST", "/v1/analytics-forecasts", fmt.Sprintf(ask, c.scenario), "")
			errs, _ := got["errors"].([]any)
			if len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("errors: got %v, want one on %s", got["errors"], c.field)
			}
		})
	}
	y, _ := e.Must(200, "POST", "/v1/analytics-forecasts", `{"metric": "revenue", "companies": ["2000"], "grain": "year", "periods_ahead": 1,
		"method": "moving_average", "window": 1, "scenario": {"adjustments": [{"dimension": "company", "value": "2000", "percent": "-50"}]}}`, "")
	sc, _ = y["scenario"].(map[string]any)
	if p, _ := sc["periods"].([]any)[0].(map[string]any); p["label"] != "2027" || p["period"] != nil || p["scenario"] != "300.00" || p["difference"] != "-300.01" {
		t.Errorf("by year: got %v, want 2027 with no period, 600.01 less half of it rounded away from zero (300.01)", p)
	}
}
