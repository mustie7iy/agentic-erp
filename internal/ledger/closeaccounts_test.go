package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestCloseAccountsAccess: a close's profit and loss read runs only in a
// request's unit of work, and shows nothing of a legal entity the caller
// may not act in; its rows and database failures are tested through GET
// /v1/period-closes/{id}/variances (internal/closing).
func TestCloseAccountsAccess(t *testing.T) {
	if _, err := (ledger.CloseAccounts{}).ProfitAndLoss(context.Background(), uuid.New(), 2026, 1); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	e := fixture.New(t, "ledger.entry.read")
	var got []ledger.AccountNet
	if err := apitest.InRequest(t, e.Tenant, e.Reader(t, "Reader of 1000", e.US), func(ctx context.Context) error {
		var err error
		got, err = ledger.CloseAccounts{}.ProfitAndLoss(ctx, e.DE, 2026, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("company 2000 for a reader of 1000: got %v, want none", got)
	}
}
