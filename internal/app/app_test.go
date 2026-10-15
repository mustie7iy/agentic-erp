package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// TestQuotasAndDeadlines checks the hardening in the request chain: the
// rate limiter answers before the unit of work, and a request past its
// deadline is a 503 the client can retry. The chain is platform/httpapi's,
// but the check runs here because it needs a real operation behind the
// unit of work (the database answering the deadline).
func TestQuotasAndDeadlines(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ten := pgtest.NewTenant(t, "platform.operation.read")
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	legacy, _ := fieldcrypt.New([]byte(strings.Repeat("f", 32)))
	keys := &fieldcrypt.Keyring{Legacy: legacy}
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
	rec := get(h)
	if rec.Code != 200 {
		t.Fatalf("first status: got %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("first RateLimit-Remaining: got %q, want 0", got)
	}
	rec = get(h)
	if rec.Code != 429 {
		t.Fatalf("second status: got %d, want 429 (%s)", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("second Retry-After: got empty, want set (%v)", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "request.rate_limited") {
		t.Errorf("second body: got %s, want request.rate_limited", rec.Body)
	}
	expiring := base
	expiring.RequestTimeout = time.Nanosecond
	if h, err = New(expiring); err != nil {
		t.Fatal(err)
	}
	rec = get(h)
	if rec.Code != 503 {
		t.Fatalf("expired status: got %d, want 503 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("expired Retry-After: got %q, want 1", got)
	}
	if !strings.Contains(rec.Body.String(), "platform.unavailable") {
		t.Errorf("expired body: got %s, want platform.unavailable", rec.Body)
	}
}
