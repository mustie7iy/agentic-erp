package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestResetClearingRefusals (T75i): a clearing is reset only in a
// request's unit of work and only by an entry that made one; a second
// reset that reaches the database is answered as already reset. The
// reset itself, its locks, its refusals under them and its database
// failures are tested through the incoming payments it undoes
// (internal/receivables, reset_test.go) and the parallel ledgers it
// reaches (TestParallelClearing).
func TestResetClearingRefusals(t *testing.T) {
	if _, err := (ledger.Postings{}).ResetClearing(context.Background(), ledger.ClearingReset{}); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a unit of work: got %v, want %v", err, reqctx.ErrOutsideRequest)
	}
	e := setup(t)
	bank, expense := e.account(t, "2000", "100000"), e.account(t, "2000", "520000")
	posted, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "10.00", ""}, line{bank, "-10.00", ""}), "")
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().ResetClearing(ctx, ledger.ClearingReset{Entry: ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.MustParse(posted["id"].(string))}})
		return err
	})
	if p := problem.From(err); p.Status != 422 || p.Code != "ledger.not_a_clearing_entry" {
		t.Errorf("an entry that cleared nothing: got %v, want 422 ledger.not_a_clearing_entry", err)
	}
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "clearing_reset_pkey"}); p.Status != 409 || p.Code != "ledger.clearing_already_reset" {
		t.Errorf("clearing_reset_pkey: got %v, want 409 ledger.clearing_already_reset", p)
	}
}
