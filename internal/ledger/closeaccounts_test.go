package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestCloseAccountsAccess: a close's profit and loss read and its read of
// cited entries run only in a request's unit of work, and show nothing of
// a legal entity the caller may not act in; their rows and database
// failures are tested through GET /v1/period-closes/{id}/variances and the
// variance comments (internal/closing).
func TestCloseAccountsAccess(t *testing.T) {
	if _, err := (ledger.CloseAccounts{}).ProfitAndLoss(context.Background(), uuid.New(), 2026, 1); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	if _, err := (ledger.CloseAccounts{}).Cited(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}); err == nil {
		t.Error("cited entries outside a unit of work: got no error, want one")
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
	entry := e.PostedBy(t, "", "2026-01-15", 1000)
	var cited map[uuid.UUID]ledger.CitedEntry
	if err := apitest.InRequest(t, e.Tenant, e.Reader(t, "Reader of 1000 too", e.US), func(ctx context.Context) error {
		var err error
		cited, err = ledger.CloseAccounts{}.Cited(ctx, e.DE, e.Account(t, "2000", "100000"), []uuid.UUID{entry})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(cited) != 0 {
		t.Errorf("company 2000's entry for a reader of 1000: got %v, want none", cited)
	}
	if _, err := (ledger.CloseAccounts{}).Accounts(context.Background(), uuid.New(), nil, []string{"100000"}); err == nil {
		t.Error("an accrual's accounts outside a unit of work: got no error, want one")
	}
	if _, err := (ledger.ParkedEntries{}).Statuses(context.Background(), []uuid.UUID{uuid.New()}); err == nil {
		t.Error("parked entries' statuses outside a unit of work: got no error, want one")
	}
	if _, err := (ledger.CloseAccounts{}).ProfitCenter(context.Background(), uuid.New(), 2026, time.Now(), uuid.New(), nil, nil); err == nil {
		t.Error("an accrual's profit center outside a unit of work: got no error, want one")
	}
	var accounts []ledger.CloseAccount
	if err := apitest.InRequest(t, e.Tenant, e.Reader(t, "Reader of 1000 as well", e.US), func(ctx context.Context) error {
		var err error
		accounts, err = ledger.CloseAccounts{}.Accounts(ctx, e.DE, []uuid.UUID{e.Account(t, "2000", "100000")}, []string{"100000"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Errorf("company 2000's accounts for a reader of 1000: got %v, want none", accounts)
	}
}
