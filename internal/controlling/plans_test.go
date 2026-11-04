package controlling_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
)

// TestPlansAccess: the close's plan port runs only in a request's unit of
// work, and shows no plan of a legal entity the caller may not act in;
// its sums and database failures are tested through GET
// /v1/period-closes/{id}/variances (internal/closing).
func TestPlansAccess(t *testing.T) {
	if _, _, err := (controlling.Plans{}).Category(context.Background(), "PLAN"); err == nil {
		t.Error("a category outside a unit of work: got no error, want one")
	}
	if _, err := (controlling.Plans{}).Amounts(context.Background(), uuid.New(), uuid.New(), 2026, 1, false); err == nil {
		t.Error("amounts outside a unit of work: got no error, want one")
	}
	p := setup(t)
	cat := uuid.MustParse(p.category(t, "PLAN"))
	p.batch(t, cat.String(), "budget", line(p.c.Admin, p.office, 1, "100.00"))
	for name, who := range map[string]struct {
		reader bool
		want   int
	}{"a reader of 1000": {true, 0}, "a person of every company": {false, 1}} {
		t.Run(name, func(t *testing.T) {
			actor := p.Tenant.Human
			if who.reader {
				actor = p.Reader(t, "Reader of 1000", p.US)
			}
			var got map[uuid.UUID]int64
			if err := apitest.InRequest(t, p.Tenant, actor, func(ctx context.Context) error {
				amounts, err := controlling.Plans{}.Amounts(ctx, p.DE, cat, 2026, 1, false)
				got = map[uuid.UUID]int64{}
				for k, v := range amounts {
					got[k] = int64(v)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != who.want {
				t.Errorf("company 2000's plan: got %v, want %d accounts", got, who.want)
			}
		})
	}
}
