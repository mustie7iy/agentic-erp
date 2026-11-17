package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// releasesOf answers Releasing with its runs, or its error.
type releasesOf struct {
	runs []ReleasingRun
	err  error
}

func (r releasesOf) Releasing(context.Context, uuid.UUID, time.Time, time.Time) ([]ReleasingRun, error) {
	return r.runs, r.err
}

// TestNoRelease: a regular period is not closed or locked while a payment
// run releases in it, the refusal naming each run by number and id; a
// special period takes no payment and asks nothing, and a ledger wired
// without the payments port refuses rather than close unchecked.
func TestNoRelease(t *testing.T) {
	a, b := ReleasingRun{ID: uuid.New(), DocumentNumber: "PR-1"}, ReleasingRun{ID: uuid.New(), DocumentNumber: "PR-2"}
	down := errors.New("port down")
	regular, special := Period{}, Period{IsSpecial: true}
	for name, c := range map[string]struct {
		releases PaymentReleases
		p        Period
		want     error
		detail   string
	}{
		"none releasing": {releasesOf{}, regular, nil, ""},
		"special":        {nil, special, nil, ""},
		"no port":        {nil, regular, errNoPaymentReleases, ""},
		"port failing":   {releasesOf{err: down}, regular, down, ""},
		"one run": {releasesOf{runs: []ReleasingRun{a}}, regular, nil, "The period cannot be closed while payment run PR-1 (" + a.ID.String() +
			") of the company is being released with payments dated in it not posted yet: resume the release (:release) and let it finish first."},
		"two runs": {releasesOf{runs: []ReleasingRun{a, b}}, regular, nil, "The period cannot be closed while payment runs PR-1 (" + a.ID.String() +
			"), PR-2 (" + b.ID.String() + ") of the company are being released with payments dated in it not posted yet: resume the release (:release) " +
			"and let it finish first."},
	} {
		t.Run(name, func(t *testing.T) {
			err := noRelease(context.Background(), c.releases, c.p, Closed)
			if c.detail != "" {
				if p := problem.From(err); p.Code != "ledger.payment_release_pending" || p.Detail != c.detail {
					t.Errorf("got %v %q, want ledger.payment_release_pending %q", p.Code, p.Detail, c.detail)
				}
				return
			}
			if !errors.Is(err, c.want) { // errors.Is(err, nil) holds for a nil err alone
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}
