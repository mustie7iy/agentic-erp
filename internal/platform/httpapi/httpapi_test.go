package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/ogen-go/ogen/validate"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestAdmin(t *testing.T) {
	m := telemetry.NewMetrics()
	healthy := Admin(func(context.Context) error { return nil }, m)
	sick := Admin(func(context.Context) error { return errors.New("db down") }, m)
	for _, c := range []struct {
		h    http.Handler
		path string
		code int
	}{{healthy, "/livez", 200}, {healthy, "/readyz", 200}, {sick, "/readyz", 503}, {sick, "/livez", 200}, {healthy, "/metrics", 200}} {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s: %d", c.path, rec.Code)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[error]string{
		&ogenerrors.SecurityError{Err: errors.New("x")}:                                      "authn.invalid_token",
		&ogenerrors.DecodeRequestError{Err: errors.New("x")}:                                 "request.invalid_body",
		&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: errors.New("bad")}: "request.invalid_parameter",
		problem.Conflict("own.code", "x"):                                                    "own.code",
		errors.New("plain"):                                                                  "platform.internal",
	}
	required := &validate.Error{Fields: []validate.FieldError{{Name: "If-Match", Error: validate.ErrFieldRequired}}}
	other := &validate.Error{Fields: []validate.FieldError{{Name: "If-Match", Error: errors.New("too long")}}}
	cases[&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: required}] = "request.precondition_required"
	cases[&ogenerrors.DecodeParamError{Name: "Idempotency-Key", In: "header", Err: validate.ErrFieldRequired}] = "request.precondition_required"
	cases[&ogenerrors.DecodeParamError{Name: "If-Match", In: "header", Err: other}] = "request.invalid_parameter"
	cases[&ogenerrors.DecodeParamError{Name: "id", In: "path", Err: validate.ErrFieldRequired}] = "request.invalid_parameter"
	for err, code := range cases {
		if p := toProblem(err); p.Code != code {
			t.Errorf("%T: %s, want %s", err, p.Code, code)
		}
	}
	var logs strings.Builder
	rec := httptest.NewRecorder()
	errorHandler(slog.New(slog.NewTextHandler(&logs, nil)))(context.Background(), rec, httptest.NewRequest("GET", "/x", nil), errors.New("boom"))
	if rec.Code != 500 || !strings.Contains(logs.String(), "boom") {
		t.Fatalf("%d %s", rec.Code, logs.String())
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
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{Verifier: v, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: telemetry.NewMetrics(),
		Tracer: tracenoop.NewTracerProvider(), Meter: failingProvider{}, Now: time.Now})
	if err == nil {
		t.Fatal("server construction failure not reported")
	}
}
