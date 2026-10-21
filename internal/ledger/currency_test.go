package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDocumentCurrencyOnEveryPostingPath: every posting path of the
// ledger posts a document in a currency other than the legal entity's,
// kept on the entry header, with each line converted at the posting's rate
// (ADR-0005, ADR-0042): another context's posting, its reversal, a manual
// entry posted from parking, a clearing and a down payment clearing.
// Payment settlement and GR/IR postings are tested in foreign currency
// where they are built (TestForeignExchange in payments,
// TestGRIRForeignCurrency).
func TestDocumentCurrencyOnEveryPostingPath(t *testing.T) {
	e := setup(t)
	downPayments(t, e)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	expense, ap, bank, dp := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000"),
		settleAccount(t, e, "2000", "159000")
	a := "A"
	post := func(t *testing.T, lines ...ledger.PostingLine) ledger.Entry {
		t.Helper()
		var entry ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-02"),
				PostingDate: date("2026-03-02"), Currency: "USD", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: lines})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	committed := func(t *testing.T, fn func(ctx context.Context) error) {
		t.Helper()
		if err := apitest.Committed(t, e.ten, e.ten.Human, fn); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("another context's posting and its reversal", func(t *testing.T) {
		invoice := post(t, ledger.PostingLine{AccountID: expense, Amount: 10000}, ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &e.supplier})
		committed(t, func(ctx context.Context) error { _, err := postings().Reverse(ctx, invoice.ID, nil, nil); return err })
	})
	t.Run("a manual entry posted from parking", func(t *testing.T) {
		parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "USD", line{bank.String(), "12.50", ""},
			line{expense.String(), "-12.50", ""}), "")
		committed(t, func(ctx context.Context) error {
			_, err := endpoints().PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: uuid.MustParse(parked["id"].(string)), IfMatch: "*"})
			return err
		})
	})
	t.Run("a clearing", func(t *testing.T) {
		invoice := post(t, ledger.PostingLine{AccountID: expense, Amount: 500}, ledger.PostingLine{AccountID: ap, Amount: -500, PartnerID: &e.vendor})
		payment := post(t, ledger.PostingLine{AccountID: ap, Amount: 500, PartnerID: &e.vendor}, ledger.PostingLine{AccountID: bank, Amount: -500})
		e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{map[string]any{"fiscal_year": 2026, "id": invoice.ID.String()}, 2},
			ref{map[string]any{"fiscal_year": 2026, "id": payment.ID.String()}, 1}), "")
	})
	t.Run("a down payment clearing", func(t *testing.T) {
		paid := post(t, ledger.PostingLine{AccountID: dp, Amount: 2000, PartnerID: &e.supplier, SpecialGL: &a}, ledger.PostingLine{AccountID: bank, Amount: -2000})
		committed(t, func(ctx context.Context) error {
			_, err := postings().ClearDownPayments(ctx, ledger.DownPaymentClearing{LegalEntityID: e.de, SupplierID: e.supplier, APAccountID: ap,
				DocumentType: "SA", PostingDate: date("2026-03-20"), Currency: "USD", Items: []ledger.ItemRef{{FiscalYear: paid.FiscalYear, EntryID: paid.ID, LineNo: 1}},
				SourceContext: "payables", SourceDocumentID: uuid.New(), Terms: &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date("2026-03-15"), NetDays: 30}})
			return err
		})
	})
	var entries, documentTypes, foreign, converted int
	pgtest.Scan(t, `SELECT count(DISTINCT e.id), count(DISTINCT e.document_type),
			count(DISTINCT e.id) FILTER (WHERE e.currency_tc = 'USD' AND e.currency_lc = 'EUR' AND e.rate_tc_lc = 0.8),
			count(*) FILTER (WHERE l.amount_lc * 5 = l.amount_tc * 4)
		FROM ledger.journal_entry e JOIN ledger.journal_line l ON l.tenant_id = e.tenant_id AND l.fiscal_year = e.fiscal_year AND l.journal_entry_id = e.id
		WHERE e.tenant_id = $1 AND e.legal_entity_id = $2`, []any{e.ten.ID, e.de}, &entries, &documentTypes, &foreign, &converted)
	if entries != 8 {
		t.Errorf("entries: got %d, want 8 (invoice, reversal, manual, invoice, payment, clearing, down payment, its clearing)", entries)
	}
	if foreign != entries {
		t.Errorf("entries in USD at 0.8 into EUR: got %d, want all %d", foreign, entries)
	}
	var lines int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_line WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{e.ten.ID, e.de}, &lines)
	if converted != lines {
		t.Errorf("lines converted at 0.8: got %d, want all %d", converted, lines)
	}
	if documentTypes < 2 {
		t.Errorf("document types: got %d, want the clearing's AB among them", documentTypes)
	}
}
