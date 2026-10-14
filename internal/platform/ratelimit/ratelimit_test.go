package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestParseQuota(t *testing.T) {
	for in, want := range map[string]Quota{"2000/10000": {2000, 10000}, "50": {50, 50}, "0.5": {0.5, 1}, "off": {}} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseQuota(in)
			if err != nil {
				t.Fatalf("ParseQuota: %v", err)
			}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
	for _, in := range []string{"", "x", "-1", "0", "inf", "10/0", "10/x", "10/-5"} {
		t.Run("invalid "+in, func(t *testing.T) {
			if got, err := ParseQuota(in); err == nil {
				t.Errorf("got %+v, want an error", got)
			}
		})
	}
	if got := New(Quota{}, Quota{}); got != nil {
		t.Errorf("New without quotas: got %v, want nil", got)
	}
	if got := New(Quota{Rate: 1, Burst: 1}, Quota{}); got == nil {
		t.Error("New with a tenant quota: got nil, want a limiter")
	}
}

// serve sends a request as the subject of the tenant at the limiter's clock.
func serve(l *Limiter, tenant uuid.UUID, subject string) *httptest.ResponseRecorder {
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	req := httptest.NewRequest("GET", "/v1/x", nil)
	if subject != "" {
		req = req.WithContext(reqctx.WithClaims(req.Context(), reqctx.Claims{TenantID: tenant, Subject: subject}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// expect checks a response's status and the given headers.
func expect(t *testing.T, step string, rec *httptest.ResponseRecorder, code int, headers map[string]string) {
	t.Helper()
	if rec.Code != code {
		t.Errorf("%s: status got %d, want %d", step, rec.Code, code)
	}
	for name, want := range headers {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s: %s got %q, want %q", step, name, got, want)
		}
	}
}

func TestQuotasAreEnforcedAndReported(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	l := &Limiter{Tenant: Quota{Rate: 10, Burst: 5}, Principal: Quota{Rate: 1, Burst: 2}, Now: func() time.Time { return now }}
	ten := uuid.New()
	// The principal's bucket is the tighter one and is what the headers report.
	rec := serve(l, ten, "alice")
	expect(t, "first", rec, 204, map[string]string{"RateLimit-Limit": "2", "RateLimit-Remaining": "1", "RateLimit-Reset": "1"})
	serve(l, ten, "alice")
	rec = serve(l, ten, "alice")
	expect(t, "over the principal quota", rec, 429, map[string]string{"Retry-After": "1", "RateLimit-Remaining": "0", "RateLimit-Reset": "2"})
	// Alice's refused request did not count against the tenant: two of its five tokens are spent.
	// Another principal gets through and still sees its own, tighter quota.
	expect(t, "bob", serve(l, ten, "bob"), 204, map[string]string{"RateLimit-Limit": "2"})
	serve(l, ten, "carol")
	// The fifth token is the tenant's last: its quota is now the tighter one, and the next caller is refused.
	expect(t, "dave", serve(l, ten, "dave"), 204, map[string]string{"RateLimit-Limit": "5", "RateLimit-Remaining": "0"})
	rec = serve(l, ten, "erin")
	expect(t, "over the tenant quota", rec, 429, map[string]string{"RateLimit-Limit": "5"})
	if !strings.Contains(rec.Body.String(), "Tenant quota of 10") {
		t.Errorf("over the tenant quota: got body %s, want it to name the tenant quota of 10", rec.Body)
	}
	// Tokens come back with time; another tenant is independent.
	now = now.Add(500 * time.Millisecond)
	if rec = serve(l, ten, "erin"); rec.Code != 204 {
		t.Fatalf("refilled: %d", rec.Code)
	}
	if rec = serve(l, uuid.New(), "alice"); rec.Code != 204 {
		t.Fatalf("other tenant: %d", rec.Code)
	}
	// Without claims nothing is limited or reported.
	expect(t, "anonymous", serve(l, ten, ""), 204, map[string]string{"RateLimit-Limit": ""})
	// Buckets that refilled completely are swept once a minute; the caller's own stays.
	now = now.Add(2 * time.Minute)
	serve(l, ten, "alice")
	if n := len(l.buckets); n != 2 { // alice's and the tenant's
		t.Fatalf("buckets after the sweep: %d", n)
	}
	// A bucket that is not full yet survives a sweep: with one request per 100 s, x's bucket outlives the first sweep.
	slow := &Limiter{Principal: Quota{Rate: 0.01, Burst: 1}, Now: func() time.Time { return now }}
	serve(slow, ten, "x")
	now = now.Add(sweep + time.Second)
	serve(slow, ten, "y")
	if n := len(slow.buckets); n != 2 {
		t.Fatalf("buckets after the sweep: %d", n)
	}
	now = now.Add(100 * time.Second)
	serve(slow, ten, "z")
	if n := len(slow.buckets); n != 1 {
		t.Fatalf("buckets after the second sweep: %d", n)
	}
	// With the tenant quota off only principals are limited; the real clock works too.
	l = &Limiter{Principal: Quota{Rate: 1000, Burst: 1}}
	expect(t, "principal only, first", serve(l, ten, "x"), 204, nil)
	expect(t, "principal only, second", serve(l, ten, "x"), 429, nil)
}

// A principal is its issuer and subject (F7): another trusted issuer that
// mints tokens with the same subject is a different caller, which the unit
// of work refuses, and its requests must not spend the real principal's quota.
func TestPrincipalBucketsAreKeyedByIssuer(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	l := &Limiter{Principal: Quota{Rate: 1, Burst: 1}, Now: func() time.Time { return now }}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	ten := uuid.New()
	send := func(issuer, subject string) int {
		req := httptest.NewRequest("GET", "/v1/x", nil)
		req = req.WithContext(reqctx.WithClaims(req.Context(), reqctx.Claims{TenantID: ten, Issuer: issuer, Subject: subject}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := send("https://rogue.example", "alice"); got != 204 {
		t.Fatalf("rogue issuer's first request: got %d, want 204", got)
	}
	if got := send("https://idp.example", "alice"); got != 204 {
		t.Errorf("real alice after the rogue issuer's request: got %d, want 204 (separate buckets)", got)
	}
	// An issuer and subject that concatenate to the same text are still two principals.
	if got := send("https://idp.example/a", "b"); got != 204 {
		t.Fatalf("issuer .../a, subject b: got %d, want 204", got)
	}
	if got := send("https://idp.example", "a/b"); got != 204 {
		t.Errorf("issuer ..., subject a/b: got %d, want 204 (separate buckets)", got)
	}
}
