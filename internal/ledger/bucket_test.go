package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestEntryLocksNeedATenant: the entry locks are keyed by the tenant of the
// transaction; a transaction without one must fail rather than take no
// lock at all (a NULL key makes the strict lock functions skip silently).
func TestEntryLocksNeedATenant(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app"})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: uuid.New(), Kind: "human"})
	_, err = ledger.OpenItems{}.Unpayable(ctx, []ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.Must(uuid.NewV7()), LineNo: 1}})
	pgtest.WantErr(t, err, "no tenant")
}
