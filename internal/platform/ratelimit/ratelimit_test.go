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
		if got, err := ParseQuota(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1", "0", "inf", "10/0", "10/x", "10/-5"} {
		if _, err := ParseQuota(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if New(Quota{}, Quota{}) != nil || New(Quota{Rate: 1, Burst: 1}, Quota{}) == nil {
		t.Fatal("New")
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

func TestQuotasAreEnforcedAndReported(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	l := &Limiter{Tenant: Quota{Rate: 10, Burst: 5}, Principal: Quota{Rate: 1, Burst: 2}, Now: func() time.Time { return now }}
	ten := uuid.New()
	// The principal's bucket is the tighter one and is what the headers report.
	rec := serve(l, ten, "alice")
	if rec.Code != 204 || rec.Header().Get("RateLimit-Limit") != "2" || rec.Header().Get("RateLimit-Remaining") != "1" || rec.Header().Get("RateLimit-Reset") != "1" {
		t.Fatalf("first: %d %v", rec.Code, rec.Header())
	}
	serve(l, ten, "alice")
	rec = serve(l, ten, "alice")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("RateLimit-Remaining") != "0" || rec.Header().Get("RateLimit-Reset") != "2" {
		t.Fatalf("over the principal quota: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	// Alice's refused request did not count against the tenant: two of its five tokens are spent.
	// Another principal gets through and still sees its own, tighter quota.
	if rec = serve(l, ten, "bob"); rec.Code != 204 || rec.Header().Get("RateLimit-Limit") != "2" {
		t.Fatalf("bob: %d %v", rec.Code, rec.Header())
	}
	serve(l, ten, "carol")
	// The fifth token is the tenant's last: its quota is now the tighter one, and the next caller is refused.
	if rec = serve(l, ten, "dave"); rec.Code != 204 || rec.Header().Get("RateLimit-Limit") != "5" || rec.Header().Get("RateLimit-Remaining") != "0" {
		t.Fatalf("dave: %d %v", rec.Code, rec.Header())
	}
	if rec = serve(l, ten, "erin"); rec.Code != 429 || rec.Header().Get("RateLimit-Limit") != "5" || !strings.Contains(rec.Body.String(), "Tenant quota of 10") {
		t.Fatalf("over the tenant quota: %d %v %s", rec.Code, rec.Header(), rec.Body)
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
	if rec = serve(l, ten, ""); rec.Code != 204 || rec.Header().Get("RateLimit-Limit") != "" {
		t.Fatalf("anonymous: %d %v", rec.Code, rec.Header())
	}
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
	if a, b := serve(l, ten, "x"), serve(l, ten, "x"); a.Code != 204 || b.Code != 429 {
		t.Fatalf("principal only: %d %d", a.Code, b.Code)
	}
}
