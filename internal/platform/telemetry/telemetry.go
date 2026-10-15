// Package telemetry makes every request traceable end to end (01-requirements
// §1.3): a W3C trace per request, one structured log line with tenant,
// principal and operation, Prometheus metrics per operation, and recovery
// from handler panics.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// NewLogger writes JSON log lines at the given level.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Tracing returns a tracer provider. With an OTLP/HTTP endpoint spans are
// exported; without one they still carry trace ids into logs and problems.
func Tracing(ctx context.Context, otlpEndpoint string) (*sdktrace.TracerProvider, error) {
	if otlpEndpoint == "" {
		return sdktrace.NewTracerProvider(), nil
	}
	// The exporter accepts any string and fails only when exporting, so a
	// misconfigured endpoint is caught here, at startup.
	u, err := url.Parse(otlpEndpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("telemetry: OTLP endpoint %q must be an http(s) URL", otlpEndpoint)
	}
	exp, _ := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(otlpEndpoint)) // the HTTP client starts without I/O and cannot fail
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp)), nil
}

// Metrics are the HTTP metrics, in their own registry.
type Metrics struct {
	Registry   *prometheus.Registry
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	readErrors *prometheus.CounterVec // created by the first Gauge
}

// NewMetrics registers the request metrics.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "erp_http_requests_total", Help: "HTTP requests by operation and status code.",
		}, []string{"operation", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "erp_http_request_duration_seconds", Help: "HTTP request latency by operation.",
			Buckets: []float64{.005, .01, .025, .05, .1, .15, .2, .3, .5, 1, 2.5, 5},
		}, []string{"operation"}),
	}
	m.Registry.MustRegister(m.requests, m.duration)
	return m
}

// Handler serves the metrics in Prometheus format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// statusWriter records the status code written.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(b) //nolint:wrapcheck // transparent http.ResponseWriter decorator: net/http and handlers compare its errors (http.ErrBodyNotAllowed, http.ErrHandlerTimeout) with ==
}

func randomTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(b)
}

// Middleware traces, logs and measures each request and turns a panic into
// a 500 problem.
func Middleware(log *slog.Logger, m *Metrics, tp trace.TracerProvider) func(http.Handler) http.Handler {
	tracer := tp.Tracer("github.com/mustie7iy/agentic-erp")
	prop := propagation.TraceContext{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer))
			traceID := randomTraceID()
			if sc := span.SpanContext(); sc.HasTraceID() {
				traceID = sc.TraceID().String()
			}
			ctx = reqctx.WithTraceID(ctx, traceID)
			ctx, info := reqctx.WithInfo(ctx)
			sw := &statusWriter{ResponseWriter: w}
			r = r.WithContext(ctx)
			defer func() {
				if rec := recover(); rec != nil {
					log.ErrorContext(ctx, "panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()), "trace_id", traceID)
					if !sw.wrote {
						problem.Write(sw, r, problem.Internal(fmt.Errorf("panic: %v", rec)))
					}
				}
				op := info.Operation
				if op == "" {
					op = "unmatched"
				}
				elapsed := time.Since(start)
				m.requests.WithLabelValues(op, strconv.Itoa(sw.status)).Inc()
				m.duration.WithLabelValues(op).Observe(elapsed.Seconds())
				span.SetAttributes(attribute.String("erp.operation", op), attribute.Int("http.response.status_code", sw.status))
				span.End()
				log.InfoContext(ctx, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
					"duration_ms", elapsed.Milliseconds(), "operation", op, "trace_id", traceID,
					"tenant_id", info.TenantID, "principal_id", info.PrincipalID)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}
