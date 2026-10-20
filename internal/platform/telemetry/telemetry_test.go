package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMiddlewareTracesLogsAndMeasures(t *testing.T) {
	var logs bytes.Buffer
	log := NewLogger(&logs, slog.LevelInfo)
	m := NewMetrics()
	tp, err := Tracing(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	h := Middleware(log, m, tp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = reqctx.TraceID(r.Context())
		reqctx.InfoFrom(r.Context()).Operation = "getOperation"
		w.Write([]byte("ok"))
	}))
	r := httptest.NewRequest("GET", "/v1/operations/x", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if seen != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("incoming trace not continued: %q", seen)
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line %s: %v", logs.Bytes(), err)
	}
	for field, want := range map[string]any{"operation": "getOperation", "status": float64(200), "trace_id": seen} {
		if got := line[field]; got != want {
			t.Errorf("log line %s: got %v, want %v", field, got, want)
		}
	}
	mrec := httptest.NewRecorder()
	m.Handler().ServeHTTP(mrec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(mrec.Body.String(), `erp_http_requests_total{code="200",operation="getOperation"} 1`) {
		t.Fatal(mrec.Body.String())
	}
}

func TestPanicsBecomeProblems(t *testing.T) {
	var logs bytes.Buffer
	h := Middleware(NewLogger(&logs, slog.LevelInfo), NewMetrics(), noop.NewTracerProvider())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqctx.TraceID(r.Context()) == "" {
			t.Error("no trace id without a tracer")
		}
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != 500 {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "platform.internal") {
		t.Errorf("body: got %s, want a platform.internal problem", rec.Body)
	}
	if !strings.Contains(logs.String(), "panic") {
		t.Errorf("logs: got %s, want the panic logged", logs.String())
	}
	if !strings.Contains(logs.String(), `"operation":"unmatched"`) {
		t.Fatal("unmatched operation not labelled")
	}
	// A panic after the response started cannot change the status.
	h = Middleware(NewLogger(&bytes.Buffer{}, slog.LevelInfo), NewMetrics(), noop.NewTracerProvider())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		w.WriteHeader(500) // ignored
		panic("late")
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 202 {
		t.Fatal(rec.Code)
	}
}

func TestOTLPExport(t *testing.T) {
	var spans atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spans.Add(1)
		w.WriteHeader(200)
	}))
	defer collector.Close()
	ctx := context.Background()
	tp, err := Tracing(ctx, collector.URL+"/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("t").Start(ctx, "s")
	span.End()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if spans.Load() == 0 {
		t.Fatal("no spans exported")
	}
	for _, bad := range []string{"::bad", "collector:4318", "ftp://x/y", "http://"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := Tracing(ctx, bad); err == nil {
				t.Errorf("got nil error for endpoint %q, want an error", bad)
			}
		})
	}
}

func TestGaugesAreReadOnScrape(t *testing.T) {
	m := NewMetrics()
	var backlog float64 = 7
	m.Gauge("erp_test_backlog", "A backlog.", func(context.Context) (float64, error) { return backlog, nil })
	m.Gauge("erp_test_broken", "Never readable.", func(context.Context) (float64, error) { return 0, errors.New("down") })
	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	// Collectors run concurrently, so the error counter shows the failure on this scrape or the next.
	got := scrape() + scrape()
	if !strings.Contains(got, "erp_test_backlog 7") {
		t.Errorf("got %s, want erp_test_backlog 7", got)
	}
	if strings.Contains(got, "erp_test_broken 0") {
		t.Errorf("got %s, want no value for the broken gauge", got)
	}
	if !regexp.MustCompile(`erp_gauge_read_errors_total\{gauge="erp_test_broken"\} [12]`).MatchString(got) {
		t.Errorf("got %s, want a read error counted for the broken gauge", got)
	}
	backlog = 0
	if got = scrape(); !strings.Contains(got, "erp_test_backlog 0") {
		t.Fatal(got)
	}
}

// TestInfo: an info metric is one sample of 1 per row of label values,
// for facts that hold for the process's life (ADR-0033's statement of
// encryption at rest).
func TestInfo(t *testing.T) {
	m := NewMetrics()
	m.Info("erp_test_info", "Facts.", []string{"store", "encryption"}, [][]string{{"postgres", "volume"}, {"objects", "sse-kms"}})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{`erp_test_info{encryption="volume",store="postgres"} 1`, `erp_test_info{encryption="sse-kms",store="objects"} 1`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("got %s, want %s", rec.Body.String(), want)
		}
	}
}

// The catalog (ADR-0043) holds every registered metric: names that pass
// promlint, labels from the allow-list, series under the ceiling.
func TestCatalog(t *testing.T) {
	m := NewMetrics()
	m.Gauge("erp_outbox_backlog", "Events waiting.", func(context.Context) (float64, error) { return 0, nil })
	m.Info("erp_operation_info", "Operations by latency class.", []string{"operation", "class"}, [][]string{{"getX", "read"}})
	if err := m.Check(); err != nil {
		t.Fatalf("catalog: got %v, want none", err)
	}
	for name, c := range map[string]struct {
		register func(*Metrics)
		want     string
	}{
		"label not allowed": {func(m *Metrics) {
			m.Info("erp_tenant_info", "Tenants.", []string{"tenant_id"}, nil)
		}, `label "tenant_id" is not in the catalog's allow-list`},
		"promlint": {func(m *Metrics) {
			m.Gauge("erp_backlog_total", "A gauge named as a counter.", func(context.Context) (float64, error) { return 0, nil })
		}, "erp_backlog_total: non-counter metrics should not have \"_total\" suffix"},
		"series ceiling": {func(m *Metrics) {
			m.counterVec("erp_by_everything_total", "Too many series.", "operation", "event", "code")
		}, "over the ceiling of 50000"},
	} {
		t.Run(name, func(t *testing.T) {
			m := NewMetrics()
			c.register(m)
			if err := m.Check(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("catalog: got %v, want %s", err, c.want)
			}
		})
	}
}

// Series names what a scrape exposes: a histogram as its three series.
func TestSeries(t *testing.T) {
	m := NewMetrics()
	m.Gauge("erp_outbox_backlog", "Events waiting.", func(context.Context) (float64, error) { return 0, nil })
	got := m.Series()
	for name, want := range map[string]string{"erp_outbox_backlog": "[]", "erp_http_requests_total": "[operation code]",
		"erp_http_request_duration_seconds_bucket": "[operation le]", "erp_http_request_duration_seconds_count": "[operation]",
		"erp_http_request_duration_seconds_sum": "[operation]"} {
		if labels, ok := got[name]; !ok || fmt.Sprint(labels) != want {
			t.Errorf("%s: got %v (present %v), want %s", name, labels, ok, want)
		}
	}
	if _, ok := got["erp_http_request_duration_seconds"]; ok {
		t.Errorf("histogram: got its bare name, want only _bucket, _sum and _count")
	}
}

func TestEventsPublished(t *testing.T) {
	m := NewMetrics()
	m.EventsPublished("ledger", "journal_entry_posted", 2)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if want := `erp_events_published_total{context="ledger",event="journal_entry_posted"} 2`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics: got %s, want %s", rec.Body.String(), want)
	}
}
