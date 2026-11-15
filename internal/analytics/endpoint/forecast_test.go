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
