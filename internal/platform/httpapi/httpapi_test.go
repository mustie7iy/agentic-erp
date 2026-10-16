package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/ogen-go/ogen/validate"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

func TestAdmin(t *testing.T) {
	m := telemetry.NewMetrics()
	healthy := Admin(func(context.Context) error { return nil }, m, nil, nil)
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })
	sick := Admin(func(context.Context) error { return errors.New("db down") }, m, stub, stub)
	for _, c := range []struct {
		h    http.Handler
		path string
		code int
	}{{healthy, "/livez", 200}, {healthy, "/readyz", 200}, {sick, "/readyz", 503}, {sick, "/livez", 200}, {healthy, "/metrics", 200}, {healthy, "/version", 200},
		{healthy, "/dev/token", 404}, {sick, "/dev/token", 201}, {healthy, "/control/v1/tenants", 404}, {sick, "/control/v1/tenants", 201}} {
		t.Run(fmt.Sprintf("%s=%d", c.path, c.code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			method := "GET"
			if c.path == "/dev/token" {
				method = "POST"
			}
			c.h.ServeHTTP(rec, httptest.NewRequest(method, c.path, nil))
			if rec.Code != c.code {
				t.Errorf("%s %s: got %d, want %d", method, c.path, rec.Code, c.code)
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[error]string{
		&ogenerrors.SecurityError{Err: errors.New("x")}:                                      "authn.invalid_token",
		&ogenerrors.DecodeRequestError{Err: errors.New("x")}:                                 "request.invalid_body",
		&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: errors.New("bad")}: "request.invalid_parameter",
		problem.Conflict("own.code", "x"):                                                    "own.code",
		errors.New("plain"):                                                                  "platform.internal",
		ht.ErrNotImplemented:                                                                 "platform.not_implemented",
	}
	required := &validate.Error{Fields: []validate.FieldError{{Name: "If-Match", Error: validate.ErrFieldRequired}}}
	other := &validate.Error{Fields: []validate.FieldError{{Name: "If-Match", Error: errors.New("too long")}}}
	cases[&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: required}] = "request.precondition_required"
	cases[&ogenerrors.DecodeParamError{Name: "Idempotency-Key", In: "header", Err: validate.ErrFieldRequired}] = "request.precondition_required"
	cases[&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: other}] = "request.invalid_parameter"
	cases[&ogenerrors.DecodeParamError{Name: "id", In: "path", Err: validate.ErrFieldRequired}] = "request.invalid_parameter"
	for err, code := range cases {
		t.Run(fmt.Sprintf("%T=%s", err, code), func(t *testing.T) {
			if p := toProblem(err); p.Code != code {
				t.Errorf("%T %v: got %s, want %s", err, err, p.Code, code)
			}
		})
	}
	var logs strings.Builder
	rec := httptest.NewRecorder()
	errorHandler(slog.New(slog.NewTextHandler(&logs, nil)))(context.Background(), rec, httptest.NewRequest("GET", "/x", nil), errors.New("boom"))
	if rec.Code != 500 {
		t.Errorf("error handler status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("error handler log: got %s, want it to mention boom", logs.String())
	}
	if _, err := (security{}).HandleOidc(context.Background(), "", gen.Oidc{}); err == nil {
		t.Fatal("request without a principal accepted")
	}
}

// failingMeter makes the generated server's instrument creation fail.
type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("no counters")
}

type failingProvider struct{ noop.MeterProvider }

func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return failingMeter{} }

func TestNewReportsServerErrors(t *testing.T) {
	_, err := New(gen.UnimplementedHandler{}, Pipeline{Verifier: verifier(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: telemetry.NewMetrics(), Tracer: tracenoop.NewTracerProvider(), Meter: failingProvider{}, Now: time.Now})
	if err == nil {
		t.Fatal("server construction failure not reported")
	}
}

// TestNewServesRoutesBesideTheContract: a route outside the generated
// server answers without a bearer token; the contract's operations need one.
func TestNewServesRoutesBesideTheContract(t *testing.T) {
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	h, err := New(gen.UnimplementedHandler{}, Pipeline{Verifier: verifier(t), Routes: map[string]http.Handler{"PUT /v1/uploads/{id}/content": route},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: telemetry.NewMetrics(), Tracer: tracenoop.NewTracerProvider(),
		Meter: noop.NewMeterProvider(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		code         int
	}{{"PUT", "/v1/uploads/x/content", http.StatusCreated}, {"GET", "/v1/operations", http.StatusUnauthorized}} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
			if rec.Code != c.code {
				t.Errorf("%s %s: got %d, want %d", c.method, c.path, rec.Code, c.code)
			}
		})
	}
}

// verifier accepts tokens of a test issuer.
func verifier(t *testing.T) *authn.Verifier {
	t.Helper()
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{authntest.New(t).Config()})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
