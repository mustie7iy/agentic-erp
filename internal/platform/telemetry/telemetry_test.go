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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
		SetOperation(r.Context(), "getOperation")
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

// TestSetOperationOutsideARequest: without Middleware's Info nothing is
// named, and nothing fails.
func TestSetOperationOutsideARequest(t *testing.T) {
	ctx := context.Background()
	SetOperation(ctx, "x")
	if got := reqctx.InfoFrom(ctx).Operation; got != "" {
		t.Errorf("got %q, want nothing outside a request", got)
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

// TestFailedRequestsMarkTheirSpan: a 5xx or a panic marks the request's
// span failed, which the cell's collector keeps whatever its sampling
// (ADR-0043); a client error does not.
func TestFailedRequestsMarkTheirSpan(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    codes.Code
	}{
		"ok":           {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }, codes.Unset},
		"client error": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }, codes.Unset},
		"server error": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, codes.Error},
		"panic":        {func(http.ResponseWriter, *http.Request) { panic("boom") }, codes.Error},
	} {
		t.Run(name, func(t *testing.T) {
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			Middleware(NewLogger(&bytes.Buffer{}, slog.LevelInfo), NewMetrics(), tp)(tc.handler).
				ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
			spans := rec.Ended()
			if len(spans) != 1 {
				t.Fatalf("spans: got %d, want 1", len(spans))
			}
			if got := spans[0].Status().Code; got != tc.want {
				t.Errorf("span status: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnsampledCallersAreRecorded: a caller's traceparent with the sampled
// flag off does not stop a failed request's span from being recorded.
func TestUnsampledCallersAreRecorded(t *testing.T) {
	tp, err := Tracing(context.Background(), "http://collector.invalid:4318/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	rec := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(rec)
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	Middleware(NewLogger(&bytes.Buffer{}, slog.LevelInfo), NewMetrics(), tp)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	})).ServeHTTP(httptest.NewRecorder(), r)
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans: got %d, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("span status: got %v, want %v", got, codes.Error)
	}
	tp.UnregisterSpanProcessor(rec)
	// The batcher holds the span for an endpoint that does not resolve:
	// shutting down with a cancelled context drops it without I/O.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = tp.Shutdown(ctx) // its error is the cancelled context's, which is the point
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

// TestCounter: a counter another package owns is registered with its
// catalog entry, so Check holds it and Series names it.
func TestCounter(t *testing.T) {
	m := NewMetrics()
	opts := prometheus.CounterOpts{Name: "erp_owned_total", Help: "Owned elsewhere."}
	m.Counter(prometheus.NewCounterVec(opts, []string{"tenant_id"}), opts, "tenant_id")
	if got := fmt.Sprint(m.Series()["erp_owned_total"]); got != "[tenant_id]" {
		t.Errorf("series: got %s, want [tenant_id]", got)
	}
	if err := m.Check(); err == nil || !strings.Contains(err.Error(), `erp_owned_total: label "tenant_id"`) {
		t.Errorf("catalog: got %v, want the owned counter's label refused", err)
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
		}, "over the ceiling of 60000"},
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

// deadlines records the deadlines a handler asks of the server's writer.
type deadlines struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (d *deadlines) SetReadDeadline(t time.Time) error  { d.read = t; return nil }
func (d *deadlines) SetWriteDeadline(t time.Time) error { d.write = t; return nil }

// TestHandlersReachTheServersWriter: behind the middleware, a handler's
// http.ResponseController still sets the server's deadlines (the model
// gateway's, the upload path's) rather than being refused as unsupported.
func TestHandlersReachTheServersWriter(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	h := Middleware(slog.New(slog.DiscardHandler), NewMetrics(), noop.NewTracerProvider())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(at); err != nil {
			t.Errorf("read deadline: %v", err)
		}
		if err := rc.SetWriteDeadline(at.Add(time.Minute)); err != nil {
			t.Errorf("write deadline: %v", err)
		}
	}))
	w := &deadlines{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/uploads/x/content", nil))
	if !w.read.Equal(at) {
		t.Errorf("read deadline: got %v, want %v", w.read, at)
	}
	if !w.write.Equal(at.Add(time.Minute)) {
		t.Errorf("write deadline: got %v, want %v", w.write, at.Add(time.Minute))
	}
}
