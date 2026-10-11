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

	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/ogen-go/ogen/validate"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestAdmin(t *testing.T) {
	m := telemetry.NewMetrics()
	healthy := Admin(func(context.Context) error { return nil }, m, nil)
	sick := Admin(func(context.Context) error { return errors.New("db down") }, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	for _, c := range []struct {
		h    http.Handler
		path string
		code int
	}{{healthy, "/livez", 200}, {healthy, "/readyz", 200}, {sick, "/readyz", 503}, {sick, "/livez", 200}, {healthy, "/metrics", 200},
		{healthy, "/dev/token", 404}, {sick, "/dev/token", 201}} {
		rec := httptest.NewRecorder()
		method := "GET"
		if c.path == "/dev/token" {
			method = "POST"
		}
		c.h.ServeHTTP(rec, httptest.NewRequest(method, c.path, nil))
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
		ht.ErrNotImplemented:                                                                 "platform.not_implemented",
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

// TestQuotasAndDeadlines checks the hardening in the request chain: the
// rate limiter answers before the unit of work, and a request past its
// deadline is a 503 the client can retry.
func TestQuotasAndDeadlines(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ten := pgtest.NewTenant(t, "platform.operation.read")
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	keys, _ := fieldcrypt.New([]byte(strings.Repeat("f", 32)))
	base := Config{Pool: pgtest.Pool(t), AppRole: "erp_app", Verifier: v, Pages: pages, FieldKeys: keys, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: telemetry.NewMetrics(), Tracer: tracenoop.NewTracerProvider(), Meter: noop.NewMeterProvider(), Now: time.Now}
	get := func(h http.Handler) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer "+is.Token(t, ten.ID, ten.Human.Subject, nil))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	limited := base
	limited.Limits = &ratelimit.Limiter{Principal: ratelimit.Quota{Rate: 0.001, Burst: 1}}
	h, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(h); rec.Code != 200 || rec.Header().Get("RateLimit-Remaining") != "0" {
		t.Fatalf("first: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h); rec.Code != 429 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "request.rate_limited") {
		t.Fatalf("second: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	expiring := base
	expiring.RequestTimeout = time.Nanosecond
	if h, err = New(expiring); err != nil {
		t.Fatal(err)
	}
	if rec := get(h); rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "platform.unavailable") {
		t.Fatalf("expired: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}
