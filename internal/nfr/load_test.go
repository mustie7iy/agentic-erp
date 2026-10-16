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
// weekly suite runs it longer and at the largest tenant's rate.
func TestLoad(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("load suite: runs without -short and without the race detector (make load, weekly)")
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
	// The limiter's clock stands still: its buckets refill at 20 tokens a
	// second, so on the wall clock a run slower than 20 requests a second
	// (a busy machine, the race detector) got its tokens back as fast as it
	// spent them and was never refused. With no time passing, each user has
	// exactly its burst of 20, whatever the machine's speed.
	limits := ratelimit.New(ratelimit.Quota{}, ratelimit.Quota{Rate: 20, Burst: 20})
	frozen := time.Now()
	limits.Now = func() time.Time { return frozen }
	s := Start(t, Options{Users: 2, Limits: limits})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
	if served != 20 {
		t.Errorf("hot user served: got %d, want its burst of 20", served)
	}
	if refused != 40 {
		t.Errorf("hot user refused: got %d, want the 40 requests past its burst", refused)
	}
	if !contains(string(refusal), "request.rate_limited") {
		t.Errorf("refusal: got %s, want a request.rate_limited problem", refusal)
	}
	if quietServed != 20 {
		t.Errorf("quiet user served: got %d, want all 20", quietServed)
	}
}
