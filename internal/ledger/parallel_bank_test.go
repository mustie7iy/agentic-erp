package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestParallelBankPosting: a bank statement line's posting (T49f) goes to
// every ledger the company keeps, as a settlement does, and clears the
// copies of the payments' lines on the clearing account with its own copy's
// offsets; a posting rule's posting goes to every ledger too, and a period
// closed in a parallel ledger refuses the date (ADR-0045).
func TestParallelBankPosting(t *testing.T) {
	e := setup(t)
	doc := parallel(t)
	doc.Companies[1].ChartOfAccounts = append(doc.Companies[1].ChartOfAccounts,
		config.Account{Number: "102000", Name: "Bank clearing", Type: config.Asset, OpenItems: true})
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period <= 12
		AND status = 'closed'`, e.ten.ID)
	p := parallelEnv{env: e}
	p.leading, p.local = p.ledger(t, "0L"), p.ledger(t, "2L")
	bank, clearingAccount, expense := e.account(t, "2000", "100000"), e.account(t, "2000", "102000"), e.account(t, "2000", "520000")
	payment, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{expense, "100.00", ""}, line{clearingAccount, "-100.00", ""}), "")
	post := func(in ledger.BankPosting) ledger.Entry {
		t.Helper()
		var posted ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			posted, err = postings().PostBank(ctx, in)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return posted
	}
	cleared := post(ledger.BankPosting{LegalEntityID: e.de, DocumentType: "SA", PostingDate: date("2026-03-22"), Currency: "EUR",
		BankAccountID: uuid.MustParse(bank), Amount: -10000, ClearingAccountID: uuid.MustParse(clearingAccount),
		Entries: []ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.MustParse(payment["id"].(string))}}, SourceContext: "payments", SourceDocumentID: uuid.New()})
	if ledgers, _ := p.group(t, cleared.ID.String()); ledgers != "0L,2L" {
		t.Errorf("bank posting posted to: got %s, want 0L,2L", ledgers)
	}
	paymentCopy, own := p.copyOf(t, payment["id"]), p.copyOf(t, cleared.ID)
	wantCleared(t, map[string]string{payment["id"].(string): "2", paymentCopy: "2", own: "1"},
		map[string]string{payment["id"].(string): "the payment", paymentCopy: "the payment's copy", own: "the bank posting's copy"})

	charge := post(ledger.BankPosting{LegalEntityID: e.de, DocumentType: "SA", PostingDate: date("2026-03-23"), Currency: "EUR",
		BankAccountID: uuid.MustParse(bank), Amount: -500, CounterAccountID: uuid.MustParse(expense), SourceContext: "payments", SourceDocumentID: uuid.New()})
	if ledgers, _ := p.group(t, charge.ID.String()); ledgers != "0L,2L" {
		t.Errorf("posting rule's bank posting posted to: got %s, want 0L,2L", ledgers)
	}

	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND ledger_id = $2
		AND fiscal_year = 2026 AND period = 4`, e.ten.ID, uuid.MustParse(p.local))
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return postings().Postable(ctx, e.de, date("2026-04-15")) })
	pgtest.CheckErr(t, err, "in ledger 2L")
}
