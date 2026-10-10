package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	json.Unmarshal(logs.Bytes(), &line)
	if line["operation"] != "getOperation" || line["status"] != float64(200) || line["trace_id"] != seen {
		t.Fatalf("log line %v", line)
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
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "platform.internal") || !strings.Contains(logs.String(), "panic") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
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
		if _, err := Tracing(ctx, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
