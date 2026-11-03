package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withAssetAccount is withUnflagged102000 with company 2000's new account
// 160000 an asset reconciliation account (ADR-0057 §3), and numbers
// flagged too.
func withAssetAccount(tb testing.TB, numbers ...string) *config.Config {
	tb.Helper()
	return flagged(withUnflagged102000(tb), numbers...)
}

// flagged is doc with company 2000's new account 160000 an asset
// reconciliation account, and numbers flagged too.
func flagged(doc *config.Config, numbers ...string) *config.Config {
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "160000", Name: "Machinery", Type: config.Asset, AssetReconciliation: true})
	for _, n := range numbers {
		de.Account(n).AssetReconciliation = true
	}
	return doc
}

// apply activates doc's ledger section as a change's activation does.
func apply(t *testing.T, e env, doc *config.Config) error {
	t.Helper()
	return apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	})
}

// TestAssetReconciliationSetup: only an account the activation creates
// becomes an asset reconciliation account (ADR-0057 §3, As built T57a):
// an existing one, posted or not, is refused on submission and at
// activation, since a posting in flight while it turned could stay on it;
// a flagged account stays one, and is not open-item managed, as supplier
// reconciliation accounts are.
func TestAssetReconciliationSetup(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	bank := e.account(t, "2000", "100000")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{e.account(t, "2000", "510000"), "10.00", ""}, line{bank, "-10.00", ""}), "")
	if err := apply(t, e, withUnflagged102000(t)); err != nil {
		t.Fatal(err)
	}
	existing := func(number string) string {
		return "companies[2000].chart_of_accounts[" + number + "].asset_reconciliation: account " + number + " exists already, so it cannot become " +
			"an asset reconciliation account; add a new account flagged asset_reconciliation, before any balance is posted or migrated onto it"
	}
	turnedOff := "companies[2000].chart_of_accounts[160000].asset_reconciliation: an asset reconciliation account stays one once activated"
	for _, c := range []struct {
		name           string
		base, proposed *config.Config
		want           string
	}{
		{"a new account", withUnflagged102000(t), withAssetAccount(t), ""},
		{"an existing account without postings", withUnflagged102000(t), withAssetAccount(t, "102000"), existing("102000")},
		{"a posted account", withUnflagged102000(t), withAssetAccount(t, "100000"), existing("100000")},
		{"a posted account, first activation", nil, withAssetAccount(t, "100000"), existing("100000")},
		{"already flagged", withAssetAccount(t, "100000"), withAssetAccount(t, "100000"), ""},
		{"turned off", withAssetAccount(t), unflagged160000(t), turnedOff},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				fs, err := s.Check(ctx, c.base, c.proposed)
				for _, f := range fs {
					got = append(got, f.Path+": "+f.Message)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
	err := apply(t, e, withAssetAccount(t, "102000"))
	if p := (*problem.Problem)(nil); !errors.As(err, &p) || p.Code != "ledger.asset_reconciliation_not_new" || p.Status != 409 {
		t.Fatalf("activation flagging an existing account: got %v, want 409 ledger.asset_reconciliation_not_new", err)
	}
	if got := reconciliationKind(t, e, e.account(t, "2000", "102000")); got != "none" {
		t.Errorf("account 102000 after the refused activation: got %s, want none", got)
	}
	if err := apply(t, e, withAssetAccount(t)); err != nil {
		t.Fatal(err)
	}
	cost := e.account(t, "2000", "160000")
	if got := reconciliationKind(t, e, cost); got != "asset" {
		t.Errorf("account 160000: got reconciliation kind %s, want asset", got)
	}
	if openItemManaged(t, e, cost) {
		t.Error("account 160000 open-item managed: got true, want false")
	}
	if !openItemManaged(t, e, e.account(t, "2000", "211000")) {
		t.Error("supplier reconciliation account 211000 open-item managed: got false, want true")
	}
}

// unflagged160000 is withAssetAccount with account 160000 no longer an
// asset reconciliation account.
func unflagged160000(tb testing.TB) *config.Config {
	tb.Helper()
	doc := withAssetAccount(tb)
	doc.Companies[1].Account("160000").AssetReconciliation = false
	return doc
}

// withUnflagged102000 is the document with company 2000's account 102000,
// an asset account that reconciles nothing.
func withUnflagged102000(tb testing.TB) *config.Config {
	tb.Helper()
	doc := document(tb)
	doc.Companies[1].ChartOfAccounts = append(doc.Companies[1].ChartOfAccounts, config.Account{Number: "102000", Name: "Machinery 2", Type: config.Asset})
	return doc
}

// reconciliationKind reads the account's reconciliation kind through the
// API.
func reconciliationKind(t *testing.T, e env, account string) string {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/accounts/"+account, "", "")
	return got["reconciliation_kind"].(string)
}

// TestAssetRanges: a company with asset accounts numbers its assets and
// their entries in the current fiscal year, the one its date falls in
// (ADR-0057 §3): each of the three ranges is required.
func TestAssetRanges(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}, Now: func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }}
	doc := withAssetAccount(t)
	doc.Companies[1].AssetAccounts = []config.AssetAccounts{{Class: "MACHINES", AcquisitionCost: "160000"}}
	check := func() []string {
		var got []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := s.Check(ctx, document(t), doc)
			for _, f := range fs {
				got = append(got, f.Path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := "number_ranges[2000/asset/2026] number_ranges[2000/journal_aa/2026] number_ranges[2000/journal_af/2026]"
	if got := strings.Join(check(), " "); got != want {
		t.Errorf("findings without the ranges: got %q, want %q", got, want)
	}
	for _, kind := range config.AssetRanges {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: kind, FiscalYear: 2026, First: 1, Last: 999})
	}
	if got := check(); len(got) != 0 {
		t.Errorf("findings with the ranges: got %v, want none", got)
	}
}

// TestAssetReconciliationPostings: a line on an asset reconciliation
// account is refused in a manual entry, a parked entry, a template and
// another context's posting (422 ledger.asset_reconciliation_account,
// naming the line), and posts from the assets context and a migration run
// (ADR-0057 §3); the database's refusal maps to the same problem.
func TestAssetReconciliationPostings(t *testing.T) {
	e := setup(t)
	if err := apply(t, e, withAssetAccount(t)); err != nil {
		t.Fatal(err)
	}
	cost, bank := e.account(t, "2000", "160000"), e.account(t, "2000", "100000")
	body := entry(e.de, "2026-03-15", "EUR", line{bank, "-10.00", ""}, line{cost, "10.00", ""})
	for _, c := range []struct{ name, path, body string }{
		{"a manual entry", "/v1/journal-entries", body},
		{"a parked entry", "/v1/parked-journal-entries", body},
		{"a template", "/v1/entry-templates", templateBody(e.de, "Machine", "", line{bank, "-10.00", ""}, line{cost, "10.00", ""})},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(422, "ledger.asset_reconciliation_account", "POST", c.path, c.body, "")
			if errs, _ := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "lines[1].account_id" {
				t.Errorf("refusal's field: got %v, want lines[1].account_id", m["errors"])
			}
		})
	}
	post := func(source string) error {
		return apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"),
				Currency: "EUR", SourceContext: source, SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					{AccountID: uuid.MustParse(cost), Amount: money.Minor(5000)}, {AccountID: uuid.MustParse(bank), Amount: -5000}}})
			return err
		})
	}
	for _, c := range []struct {
		source string
		want   string
	}{{"payables", "ledger.asset_reconciliation_account"}, {"payments", "ledger.asset_reconciliation_account"}, {"assets", ""}, {"integration", ""}} {
		t.Run("from "+c.source, func(t *testing.T) {
			err := post(c.source)
			if c.want == "" {
				if err != nil {
					t.Fatalf("a posting from %s: got %v, want it posted", c.source, err)
				}
				return
			}
			pgtest.WantErr(t, err, c.want)
		})
	}
	p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "journal_line_asset_reconciliation"})
	if p.Code != "ledger.asset_reconciliation_account" || p.Status != 422 {
		t.Errorf("constraint journal_line_asset_reconciliation: got %d %s, want 422 ledger.asset_reconciliation_account", p.Status, p.Code)
	}
}
