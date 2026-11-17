package apitools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// TestDays: a run of one company is dated in the company's time zone, as
// the API gives it, any other in UTC; a company or zone that cannot be
// read stops the run rather than date it wrongly (review:batch77).
func TestDays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/legal-entities":
			_, _ = w.Write([]byte(`{"items": [{"id": "to", "code": "3000"}, {"id": "bad", "code": "9000"}, {"id": "gone", "code": "8000"}, {"id": "none", "code": "6000"}]}`)) // a test's answer
		case "/v1/legal-entities/to/today":
			_, _ = w.Write([]byte(`{"date": "2026-11-12", "time_zone": "America/Toronto"}`)) // a test's answer
		case "/v1/legal-entities/none/today":
			_, _ = w.Write([]byte(`{"date": "2026-11-12"}`)) // a test's answer
		case "/v1/legal-entities/bad/today":
			_, _ = w.Write([]byte(`{"date": "2026-11-12", "time_zone": "Mars/Olympus"}`)) // a test's answer
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code": "request.not_found", "status": 404}`)) // a test's answer
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	at := func() time.Time { return time.Date(2026, 11, 13, 4, 30, 0, 0, time.UTC) } // 23:30 on the 12th in Toronto
	days := apitools.Days{API: &apitools.Client{BaseURL: srv.URL, Token: "t"}, Now: at}
	for name, c := range map[string]struct {
		codes []string
		days  apitools.Days
		want  string
	}{
		"one company":   {[]string{"3000"}, days, "2026-11-12"},
		"two companies": {[]string{"3000", "4000"}, days, "2026-11-13"},
		"every company": {nil, days, "2026-11-13"},
		"no API to ask": {[]string{"3000"}, apitools.Days{Now: at}, "2026-11-13"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := c.days.OfCompanies(ctx, c.codes)
			if err != nil || got != c.want {
				t.Errorf("today: got %q, %v, want %q", got, err, c.want)
			}
		})
	}
	for name, c := range map[string]struct {
		days  apitools.Days
		codes []string
		want  string
	}{
		"unknown company":   {days, []string{"7000"}, "no company 7000"},
		"no such entity":    {days, []string{"8000"}, "API 404"},
		"unknown time zone": {days, []string{"9000"}, `time zone "Mars/Olympus"`},
		"no time zone":      {days, []string{"6000"}, `time zone "": the API gave none`},
		"API unreachable":   {apitools.Days{API: &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}}, []string{"3000"}, "today of company 3000"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.days.OfCompanies(ctx, c.codes); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("today: got %v, want an error saying %q", err, c.want)
			}
		})
	}
	if got, err := (apitools.Days{API: days.API}).Of(ctx, "to"); err != nil || got != "2026-11-12" {
		t.Errorf("today without a clock: got %q, %v, want the API's date 2026-11-12", got, err)
	}
	if _, err := (apitools.Days{API: &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}}).Of(ctx, "to"); err == nil {
		t.Error("today of an unreachable API: got a date, want an error")
	}
}
