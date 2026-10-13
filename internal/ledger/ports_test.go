package ledger_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
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
		if tc.RatePct != "19.000000" {
			t.Errorf("V1 rate: got %s, want 19.000000", tc.RatePct)
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

// TestTaxCodes follows the configuration's tax codes: valid from the first
// fiscal year, a changed rate starts today and ends the old one, a removed
// code ends today, and a second change on the same day is refused.
func TestTaxCodes(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	rows := func() string {
		var out string
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT string_agg(code || ' ' || trim_scale(rate_pct) || ' ' || (valid_from = current_date)::text || ' ' ||
			coalesce((valid_to = current_date)::text, 'open'), ', ' ORDER BY code, valid_from) FROM ledger.tax_code WHERE legal_entity_id = $1`, e.de).Scan(&out); err != nil {
			t.Fatalf("list tax codes: %v", err)
		}
		return out
	}
	// Set up when 2026 began; the first activation shows as not today.
	if got := rows(); got != "V0 0 false open, V1 19 false open" {
		t.Fatalf("initial: %s", got)
	}
	changed := document(t)
	codes := changed.Companies[1].TaxCodes // V1 19%, V0 0%
	codes[0].RatePercent = "16"
	changed.Companies[1].TaxCodes = codes[:1] // V0 removed
	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	apply(changed)
	if got := rows(); got != "V0 0 false true, V1 19 false true, V1 16 true open" {
		t.Fatalf("changed: %s", got)
	}
	again := document(t)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := s.Check(ctx, changed, again)
		var paths []string
		for _, f := range fs {
			paths = append(paths, f.Path)
		}
		if strings.Join(paths, ",") != "companies[2000].tax_codes[V1]" {
			t.Errorf("same-day change: %v", fs)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A code removed today comes back from today.
	readded := document(t)
	readded.Companies[1].TaxCodes = slices.Concat(changed.Companies[1].TaxCodes[:1], readded.Companies[1].TaxCodes[1:2])
	apply(readded)
	if got := rows(); !strings.Contains(got, "V0 0 true open") {
		t.Fatalf("re-added: %s", got)
	}
}
