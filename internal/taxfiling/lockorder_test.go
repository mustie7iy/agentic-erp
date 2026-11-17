package taxfiling_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// recording notes the statements a transaction runs, in order.
type recording struct {
	pgx.Tx
	sql *[]string
}

func (r recording) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	*r.sql = append(*r.sql, sql)
	return r.Tx.Exec(ctx, sql, args...) //nolint:wrapcheck // a test double passing the real answer through
}

func (r recording) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	*r.sql = append(*r.sql, sql)
	return r.Tx.Query(ctx, sql, args...) //nolint:wrapcheck // as above
}

func (r recording) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	*r.sql = append(*r.sql, sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

// TestPrepareLocksTheRegistrationFirst: preparing a return locks its
// registration before anything of it is read, so the companies, frequency
// and periods it prepares from are those the lock holds, not a version a
// change committed while it waited replaced.
func TestPrepareLocksTheRegistrationFirst(t *testing.T) {
	e := env(t)
	h := endpoints(t, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	gstID := uuid.MustParse(id(t, e, "GST"))
	var sql []string
	_ = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { // the outcome does not matter, only the order
		tx, ok := reqctx.TxFrom(ctx)
		if !ok {
			t.Fatal("no transaction in the request")
		}
		ctx = reqctx.WithTx(ctx, recording{Tx: tx, sql: &sql})
		_, err := h.PrepareTaxReturn(ctx, &gen.TaxReturnPrepare{PeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, gen.PrepareTaxReturnParams{ID: gstID})
		return err
	})
	if len(sql) == 0 || !strings.Contains(sql[0], "tax.registration") || !strings.Contains(sql[0], "FOR UPDATE") {
		t.Fatalf("first statement: got %q, want the registration locked FOR UPDATE", sql)
	}
}
