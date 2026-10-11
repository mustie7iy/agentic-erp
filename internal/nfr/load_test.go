package nfr

import (
	"context"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
)

// TestLoad drives the mix of 05-scalability §5.8 (posting, lists, single
// reads, approval decisions, trial balance) through the public API on a
// real listener, with the projector and relay running, and checks every
// operation against the p95 and p99 budgets of 01-requirements §1.3.
//
// ERP_LOAD_USERS (default 16), ERP_LOAD_DURATION (default 10s) and
// ERP_LOAD_RPS (default 0: as fast as the users go) size the run; the
// nightly suite runs it longer and at the largest tenant's rate.
func TestLoad(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("load suite: runs without -short and without the race detector (make load, nightly)")
	}
	s := Start(t, Options{Users: int(Setting("ERP_LOAD_USERS", 16)), Events: true})
	ctx := context.Background()
	warm := NewStats()
	s.Run(ctx, 2*time.Second, 0, warm) // connections, caches and plans warm up
	st := NewStats()
	s.Run(ctx, Setting("ERP_LOAD_DURATION", 10*time.Second), int(Setting("ERP_LOAD_RPS", 0)), st)
	st.Report(t, "load")
	st.CheckBudgets(t)
	if err := s.Drain(30 * time.Second); err != nil {
		t.Error(err)
	}
	if err := s.Consistent(ctx); err != nil {
		t.Error(err)
	}
	p, o := s.Backlogs()
	t.Logf("after the run: %d entries unprojected, %d events unpublished", p, o)
}

// TestLoadShedsAHotPrincipal checks that one principal over its quota is
// refused at the door (04 §4.11) while the others are served.
func TestLoadShedsAHotPrincipal(t *testing.T) {
	s := Start(t, Options{Users: 2, Limits: ratelimit.New(ratelimit.Quota{}, ratelimit.Quota{Rate: 20, Burst: 20})})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hot, quiet := s.Users[0], s.Users[1]
	path := "/v1/journal-entries?legal_entity_id=" + s.Env.DE.String() + "&fiscal_year=2026&page_size=10"
	refused, served, quietServed := 0, 0, 0
	var refusal []byte
	for i := range 60 {
		res := s.Call(ctx, Req{Method: "GET", Path: path, As: hot.Actor})
		switch {
		case res.Status == 429:
			refused++
			refusal = res.Body
			if res.Took > 50*time.Millisecond {
				t.Errorf("a refusal took %v", res.Took)
			}
		case res.OK():
			served++
		default:
			t.Fatalf("hot user: %d %v %s", res.Status, res.Err, res.Body)
		}
		if i%3 == 0 { // 20 requests, within the quiet user's own burst
			if other := s.Call(ctx, Req{Method: "GET", Path: path, As: quiet.Actor}); other.OK() {
				quietServed++
			} else {
				t.Fatalf("quiet user: %d %v %s", other.Status, other.Err, other.Body)
			}
		}
	}
	if served < 20 || refused == 0 || !contains(string(refusal), "request.rate_limited") || quietServed != 20 {
		t.Fatalf("hot user served %d, refused %d; quiet user served %d: %s", served, refused, quietServed, refusal)
	}
}
