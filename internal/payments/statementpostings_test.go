package payments_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestStatementPostings: the statement line behind a journal entry its
// posting posted is named by the entry, with the statement's id and
// reference and the line's number; an entry no line posted is left out.
func TestStatementPostings(t *testing.T) {
	e := fixture.New(t)
	statement := e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-03-19", Amount: 500}, fixture.StatementLine{ValueDate: "2026-03-20", Amount: 10000, Rule: "NTRF"})
	entry, other := uuid.New(), uuid.New()
	pgtest.Exec(t, `UPDATE payments.statement_match SET journal_entry_year = 2026, journal_entry_id = $3, posting_date = '2026-03-20', version = version + 1
		WHERE tenant_id = $1 AND statement_id = $2 AND line_no = 2`, e.Tenant.ID, statement, entry)
	var got map[uuid.UUID]payments.PostedLine
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		got, err = payments.StatementPostings{}.Lines(ctx, []ledger.ItemRef{{FiscalYear: 2026, EntryID: entry}, {FiscalYear: 2026, EntryID: other}, {FiscalYear: 2025, EntryID: entry}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ref string // as the fixture wrote it
	pgtest.Scan(t, `SELECT statement_ref FROM payments.bank_statement WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, statement}, &ref)
	if want := (payments.PostedLine{StatementID: statement, StatementRef: ref, LineNo: 2}); len(got) != 1 || got[entry] != want || ref == "" {
		t.Errorf("got %+v, want the entry alone with %+v", got, want)
	}
	if _, err := (payments.StatementPostings{}).Lines(context.Background(), nil); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "statement lines of entries", func(ctx context.Context) error {
		_, err := payments.StatementPostings{}.Lines(ctx, []ledger.ItemRef{{FiscalYear: 2026, EntryID: entry}})
		return err
	})
}
