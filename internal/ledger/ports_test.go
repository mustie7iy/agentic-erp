package ledger_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func postings() ledger.Postings {
	return ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
}

// TestPostingsPort posts and reverses an entry the way payables does.
func TestPostingsPort(t *testing.T) {
	e := setup(t)
	expense, ap := uuid.MustParse(e.account(t, "2000", "520000")), uuid.MustParse(e.account(t, "2000", "211000"))
	invoice := uuid.New()
	ref := "INV-1"
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
			PostingDate: date("2026-03-02"), Currency: "EUR", Reference: &ref, SourceContext: "payables", SourceDocumentID: invoice,
			Lines: []ledger.PostingLine{{AccountID: expense, Amount: 11900}, {AccountID: ap, Amount: -11900, PartnerID: &e.supplier}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if entry.SourceContext != "payables" {
		t.Errorf("entry source context: got %s, want payables", entry.SourceContext)
	}
	if entry.SourceDocumentID == nil || *entry.SourceDocumentID != invoice {
		t.Errorf("entry source document: got %v, want %s", entry.SourceDocumentID, invoice)
	}
	if len(entry.Lines) != 2 {
		t.Fatalf("entry lines: got %d %+v, want 2", len(entry.Lines), entry.Lines)
	}
	if entry.Lines[1].TC != -11900 {
		t.Errorf("entry line 2 amount: got %d, want -11900", entry.Lines[1].TC)
	}
	got, _ := e.Must(200, "GET", "/v1/journal-entries?source_context=payables&source_document_id="+invoice.String(), "", "")
	if len(got["items"].([]any)) != 1 {
		t.Fatalf("by source: %v", got)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		cleared, err := postings().IsCleared(ctx, entry.FiscalYear, entry.ID)
		if err == nil && cleared {
			t.Error("cleared")
		}
		day := date("2026-03-05")
		rev, err := postings().Reverse(ctx, entry.ID, &day, nil)
		if err != nil {
			return err
		}
		if rev.ReversalOfID == nil {
			t.Error("reversal of: got nil, want the original entry")
		}
		if rev.PostingDate != day {
			t.Errorf("reversal posting date: got %v, want %v", rev.PostingDate, day)
		}
		if rev.SourceContext != "payables" {
			t.Errorf("reversal source context: got %s, want payables", rev.SourceContext)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-02"), Currency: "EUR", SourceContext: "payables", SourceDocumentID: invoice,
				Lines: []ledger.PostingLine{{AccountID: expense, Amount: 100}, {AccountID: ap, Amount: -100, PartnerID: &e.supplier}}})
			return err
		},
		"cleared": func(ctx context.Context) error { _, err := postings().IsCleared(ctx, 2026, entry.ID); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}

func TestAccountsAndCalendarPorts(t *testing.T) {
	e := setup(t)
	ap, bank := uuid.MustParse(e.account(t, "2000", "211000")), uuid.MustParse(e.account(t, "2000", "100000"))
	a, c := ledger.Accounts{}, ledger.Calendar{LegalEntities: tenancy.Directory{}}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		if id, ok, err := a.Determination(ctx, e.de, "ap_reconciliation"); err != nil || !ok {
			t.Errorf("determination: got %v %v, want found", ok, err)
		} else if id != ap {
			t.Errorf("determination: got %s, want %s", id, ap)
		}
		if _, ok, err := a.Determination(ctx, e.de, "fx_gain"); ok || err != nil {
			t.Errorf("missing determination: %v %v", ok, err)
		}
		if ok, err := a.IsPostable(ctx, e.de, bank); !ok || err != nil {
			t.Errorf("bank: %v %v", ok, err)
		}
		if ok, _ := a.IsPostable(ctx, e.de, ap); ok {
			t.Error("a reconciliation account is postable directly")
		}
		tc, ok, err := a.TaxCode(ctx, e.de, "V1", date("2026-03-01"))
		if err != nil || !ok {
			t.Errorf("V1: got %v %v, want found", ok, err)
		}
		if c, ok := tc.Single(); !ok || c.RatePct != "19.000000" {
			t.Errorf("V1: got %+v, want one component of 19.000000", tc.Components)
		}
		if tc.Direction != "input" {
			t.Errorf("V1 direction: got %s, want input", tc.Direction)
		}
		if _, ok, err := a.TaxCode(ctx, e.de, "V1", date("2025-12-31")); ok || err != nil {
			t.Errorf("before the first fiscal year: %v %v", ok, err)
		}
		if y, err := c.FiscalYear(ctx, e.de, date("2026-07-01")); err != nil || y != 2026 {
			t.Errorf("fiscal year: %d %v", y, err)
		}
		if _, err := c.FiscalYear(ctx, e.de, date("2031-07-01")); problem.From(err).Code != "ledger.no_fiscal_period" {
			t.Errorf("no fiscal year: %v", err)
		}
		if _, err := c.FiscalYear(ctx, uuid.New(), date("2026-07-01")); problem.From(err).Code != "ledger.no_fiscal_period" {
			t.Errorf("unknown legal entity: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"determination": func(ctx context.Context) error { _, _, err := a.Determination(ctx, e.de, "bank"); return err },
		"postable":      func(ctx context.Context) error { _, err := a.IsPostable(ctx, e.de, bank); return err },
		"tax code": func(ctx context.Context) error {
			_, _, err := a.TaxCode(ctx, e.de, "V1", date("2026-03-01"))
			return err
		},
		"fiscal year": func(ctx context.Context) error { _, err := c.FiscalYear(ctx, e.de, date("2026-07-01")); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}

// TestAccountBalances: an account's balances on dates are what the
// leading ledger posted to it on or before each date, in the dates' order
// (T49h): in the legal entity's local currency every entry's local
// amount, in another currency the transaction amounts of the entries in it
// (T53a).
func TestAccountBalances(t *testing.T) {
	e := setup(t)
	bank, revenue, expense := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "520000")
	e.rate(t, "USD", "EUR", "2026-01-01", "0.5")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "100.00", ""}, line{revenue, "-100.00", ""}), "")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{expense, "30.00", ""}, line{bank, "-30.00", ""}), "")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-04-02", "USD", line{expense, "10.00", ""}, line{bank, "-10.00", ""}), "")
	a, account := ledger.Accounts{}, uuid.MustParse(bank)
	dates := []time.Time{date("2026-04-02"), date("2026-03-14"), date("2026-03-15"), date("2026-03-31")}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		for currency, wants := range map[string][]money.Minor{"EUR": {6500, 0, 10000, 7000}, "USD": {-1000, 0, 0, 0}, "GBP": {0, 0, 0, 0}} {
			got, err := a.Balances(ctx, e.de, account, currency, dates)
			if err != nil {
				return err
			}
			for i, want := range wants {
				if got[i] != want {
					t.Errorf("balance in %s on %s: got %d, want %d", currency, dates[i].Format(time.DateOnly), got[i], want)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context) error { _, err := a.Balances(ctx, e.de, account, "EUR", dates); return err }
	if call(context.Background()) == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "balances", call)
}

// TestReversalBelongsToTheOwner: an entry another context posted for one of
// its documents is reversed by that context, never through the ledger's
// :reverse, which would leave the document standing over a reversed entry
// and bypass the owner's permission and checks. The owner's port still
// reverses it.
func TestReversalBelongsToTheOwner(t *testing.T) {
	e := setup(t)
	expense, ap := uuid.MustParse(e.account(t, "2000", "520000")), uuid.MustParse(e.account(t, "2000", "211000"))
	post := func(source string) ledger.Entry {
		t.Helper()
		var entry ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-02"), Currency: "EUR", SourceContext: source, SourceDocumentID: uuid.New(),
				Lines: []ledger.PostingLine{{AccountID: expense, Amount: 100}, {AccountID: ap, Amount: -100, PartnerID: &e.supplier}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	for _, c := range []struct{ source, endpoint string }{
		{"payables", "/v1/supplier-invoices/"},
		{"procurement", "/v1/goods-receipts/"},
		{"receivables", "no reversal path exists yet"},
		{"assets", "no reversal path exists yet"},
	} {
		t.Run(c.source, func(t *testing.T) {
			entry := post(c.source)
			_, tag := e.Must(200, "GET", "/v1/journal-entries/"+entry.ID.String(), "", "")
			refused := e.Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+entry.ID.String()+":reverse", "", tag)
			if detail, _ := refused["detail"].(string); !strings.Contains(detail, c.endpoint) {
				t.Errorf("detail: got %q, want it to name %s", detail, c.endpoint)
			} else if !strings.HasPrefix(c.endpoint, "/v1/") && strings.Contains(detail, "/v1/") {
				t.Errorf("detail: got %q, want no endpoint named where none exists", detail)
			}
			if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, err := postings().Reverse(ctx, entry.ID, nil, nil)
				return err
			}); err != nil {
				t.Errorf("reversal through the owner's port: got %v, want nil", err)
			}
		})
	}
	// Entries of contexts without a reversal of their own (a migration run's load) stay the ledger's to reverse.
	for _, source := range []string{"integration", "payments"} {
		entry := post(source)
		_, tag := e.Must(200, "GET", "/v1/journal-entries/"+entry.ID.String(), "", "")
		e.Must(200, "POST", "/v1/journal-entries/"+entry.ID.String()+":reverse", "", tag)
	}
	// A payables entry without its invoice has no endpoint to name: it is refused without one.
	orphan := post("payables")
	// Journal entries are insert-only (a trigger refuses the update), so the
	// fixture skips triggers for its one statement, as superuser.
	if err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `UPDATE ledger.journal_entry SET source_document_id = NULL WHERE id = $1`, orphan.ID)
		return err
	}); err != nil {
		t.Fatalf("detach the invoice from its entry: %v", err)
	}
	_, tag := e.Must(200, "GET", "/v1/journal-entries/"+orphan.ID.String(), "", "")
	refused := e.Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+orphan.ID.String()+":reverse", "", tag)
	detail, _ := refused["detail"].(string)
	if strings.Contains(detail, "/v1/") {
		t.Errorf("detail for a payables entry without its invoice: got %q, want no endpoint named", detail)
	}
	if !strings.Contains(detail, "no reversal path") {
		t.Errorf("detail for a payables entry without its invoice: got %q, want it to say no reversal path exists", detail)
	}
}
