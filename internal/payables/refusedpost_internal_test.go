package payables

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestRefusedPostOutcomes: a rule's refusal, audited by the rule, and a
// failure that is no business outcome pass through unrecorded; a business
// refusal whose audit cannot be kept fails with the audit's error rather
// than lose it.
func TestRefusedPostOutcomes(t *testing.T) {
	ctx, id := context.Background(), uuid.Must(uuid.NewV7())
	for name, err := range map[string]error{
		"a rule's refusal":     problem.Unprocessable("request.rule_refused", "A validation rule of the tenant refuses the write."),
		"no business outcome":  errors.New("the database is gone"),
		"a precondition fails": problem.New(412, "request.version_mismatch", "Precondition failed", "Stale."),
	} {
		t.Run(name, func(t *testing.T) {
			if got := refusedPost(ctx, id, nil, err); !errors.Is(got, err) {
				t.Errorf("got %v, want %v as it was", got, err)
			}
		})
	}
	refused := problem.Conflict("ledger.period_not_open", "Period 2/2026 is closed.")
	if got := refusedPost(ctx, id, nil, refused); !errors.Is(got, audit.ErrNoBuffer) {
		t.Errorf("outside a unit of work: got %v, want the audit's %v", got, audit.ErrNoBuffer)
	}
}
