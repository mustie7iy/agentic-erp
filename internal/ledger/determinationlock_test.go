package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestDeterminationAndActivationSerialize (review:batch93): a goods
// movement reading its valuation class's stock account and an activation
// changing that account run in two transactions, each order in turn, and
// whichever comes second waits for the first to commit. A movement after
// the activation posts to the new account, not the one it replaced; an
// activation after a movement sees its posting and keeps the account
// (409 ledger.determination_key_posted), so the class's stock never
// splits across two accounts.
func TestDeterminationAndActivationSerialize(t *testing.T) {
	e := setup(t)
	if err := e.applyTax(t, withKeys(t, "", "530000", "101000")); err != nil {
		t.Fatal(err)
	}
	key := config.InventoryKey(config.InventoryStock, "3000")
	bank := uuid.MustParse(e.account(t, "2000", "100000"))
	// pid is the backend of the transaction serializeOn holds open, set
	// before that transaction signals that it holds its locks.
	var pid int32
	holding := func(work func(ctx context.Context) error) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			return work(ctx)
		}
	}
	blocked := func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}
	var determined uuid.UUID
	movement := func(ctx context.Context) error {
		stock, ok, err := ledger.Accounts{}.Determination(ctx, e.de, key)
		if err != nil || !ok {
			return err
		}
		determined = stock
		_, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-02"), PostingDate: date("2026-03-02"),
			Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: stock, Amount: 100}, {AccountID: bank, Amount: -100}}})
		return err
	}
	activation := func(stock string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, withKeys(t, "", "530000", stock))
		}
	}

	t.Run("activation first", func(t *testing.T) {
		first, second := serializeOn(t, e.ten, "the account determination", blocked, holding(activation("520000")), movement)
		if first != nil || second != nil {
			t.Fatalf("activation, movement: got %v, %v, want both committed", first, second)
		}
		if want := uuid.MustParse(e.account(t, "2000", "520000")); determined != want {
			t.Errorf("stock account the movement posted to: got %s, want the activation's %s", determined, want)
		}
	})
	t.Run("movement first", func(t *testing.T) {
		first, second := serializeOn(t, e.ten, "the account determination", blocked, holding(movement), activation("101000"))
		if first != nil {
			t.Fatalf("movement: got %v, want it committed", first)
		}
		if got := problem.From(second); got.Code != "ledger.determination_key_posted" || got.Status != 409 {
			t.Errorf("activation after the movement: got %v, want 409 ledger.determination_key_posted", second)
		}
		if got, want := e.determined(t, key), e.account(t, "2000", "520000"); got != want {
			t.Errorf("class 3000's stock account after the refusal: got %s, want %s kept", got, want)
		}
	})
}
