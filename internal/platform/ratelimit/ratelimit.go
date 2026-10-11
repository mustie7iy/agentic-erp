// Package ratelimit enforces the API's request quotas (04-api-design §4.11)
// so that one hot tenant or principal cannot starve the others in a cell
// (05-scalability §5.7). Each tenant and each principal has a token bucket;
// every response reports the caller's quota as RateLimit-Limit,
// RateLimit-Remaining and RateLimit-Reset, and a request over quota is
// refused with 429 and Retry-After before it reaches the database.
package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Quota is a sustained rate with a burst: a bucket of Burst tokens refilled
// at Rate tokens per second. A zero Rate means no limit.
type Quota struct {
	Rate  float64
	Burst int
}

// ParseQuota reads "rate" or "rate/burst" (requests per second; the burst
// defaults to the rate), or "off" for no limit.
func ParseQuota(s string) (Quota, error) {
	if s == "off" {
		return Quota{}, nil
	}
	rate, burst, _ := strings.Cut(s, "/")
	q := Quota{}
	var err error
	if q.Rate, err = strconv.ParseFloat(rate, 64); err != nil || q.Rate <= 0 || math.IsInf(q.Rate, 0) {
		return Quota{}, fmt.Errorf("quota %q: want requests per second as rate or rate/burst, or off", s)
	}
	q.Burst = int(math.Ceil(q.Rate))
	if burst != "" {
		if q.Burst, err = strconv.Atoi(burst); err != nil || q.Burst < 1 {
			return Quota{}, fmt.Errorf("quota %q: the burst must be a whole number of requests, at least 1", s)
		}
	}
	return q, nil
}

func (q Quota) limited() bool { return q.Rate > 0 }

// Limiter holds the buckets of one replica. Limits are per replica: with n
// replicas behind a balancer a caller gets up to n times the quota, which
// is the usual trade-off until limits move to the gateway.
type Limiter struct {
	Tenant    Quota // per tenant, keyed by tenant id
	Principal Quota // per principal, keyed by tenant and subject
	Now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time // last request
	full   time.Time // when the bucket is full again if left alone
}

// sweep is how often buckets that have refilled completely are dropped, so
// the map holds only recent callers.
const sweep = time.Minute

// state is a bucket's answer to one request.
type state struct {
	ok        bool
	remaining int // whole tokens left after this request
	reset     int // seconds until the bucket is full again
	retry     int // seconds until one token is back, when refused
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// take spends one token of the key's bucket under q.
func (l *Limiter) take(key string, q Quota, now time.Time) state {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
		l.swept = now
	}
	if now.Sub(l.swept) >= sweep {
		for k, b := range l.buckets {
			if !now.Before(b.full) {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(q.Burst), at: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(float64(q.Burst), b.tokens+now.Sub(b.at).Seconds()*q.Rate)
	b.at = now
	s := state{ok: b.tokens >= 1}
	if s.ok {
		b.tokens--
	} else {
		s.retry = int(math.Ceil((1 - b.tokens) / q.Rate))
	}
	s.remaining = int(b.tokens)
	refill := (float64(q.Burst) - b.tokens) / q.Rate
	s.reset = int(math.Ceil(refill))
	b.full = now.Add(time.Duration(refill * float64(time.Second)))
	return s
}

// Middleware enforces the quotas for the authenticated caller. It runs
// after authentication (the claims name the tenant and subject) and before
// the unit of work, so a refused request costs no transaction. The
// principal's quota is checked first, so a hot principal's refused requests
// do not count against its tenant.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := reqctx.ClaimsFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		now := l.now()
		type check struct {
			q    Quota
			key  string
			what string
		}
		tightest := state{remaining: math.MaxInt}
		var limit Quota
		for _, c := range []check{{l.Principal, claims.TenantID.String() + "/" + claims.Subject, "Principal"}, {l.Tenant, claims.TenantID.String(), "Tenant"}} {
			if !c.q.limited() {
				continue
			}
			s := l.take(c.key, c.q, now)
			if !s.ok {
				report(w, c.q, s)
				p := problem.New(http.StatusTooManyRequests, "request.rate_limited", "Too many requests",
					fmt.Sprintf("%s quota of %s requests per second exceeded.", c.what, strconv.FormatFloat(c.q.Rate, 'f', -1, 64)))
				p.RetryAfter = s.retry
				problem.Write(w, r, p)
				return
			}
			if s.remaining < tightest.remaining {
				tightest, limit = s, c.q
			}
		}
		if limit.limited() {
			report(w, limit, tightest)
		}
		next.ServeHTTP(w, r)
	})
}

func report(w http.ResponseWriter, q Quota, s state) {
	h := w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(q.Burst))
	h.Set("RateLimit-Remaining", strconv.Itoa(s.remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(s.reset))
}

// New builds a limiter from the two quotas, or nil when both are off.
func New(tenant, principal Quota) *Limiter {
	if !tenant.limited() && !principal.limited() {
		return nil
	}
	return &Limiter{Tenant: tenant, Principal: principal}
}
