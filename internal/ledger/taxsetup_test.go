package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// taxRates lists company 2000's live tax rates as "code component rate
// from to", today's date shown as "today", an open end as "open".
func (e env) taxRates(tb testing.TB, codes ...string) string {
	tb.Helper()
	var out string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT coalesce(string_agg(code || ' ' || component || ' ' || trim_scale(rate_pct) || ' '
			|| CASE WHEN valid_from = current_date THEN 'today' ELSE valid_from::text END || ' '
			|| CASE WHEN valid_to IS NULL THEN 'open' WHEN valid_to = current_date THEN 'today' ELSE valid_to::text END, ', '
			ORDER BY code, component, valid_from), '')
		FROM ledger.tax_rate WHERE legal_entity_id = $1 AND withdrawn_at IS NULL AND code = ANY($2)`, e.de, codes).Scan(&out); err != nil {
		tb.Fatalf("list tax rates: %v", err)
	}
	return out
}

// applyTax activates doc's ledger section and returns the problem, if any.
func (e env) applyTax(tb testing.TB, doc *config.Config) error {
	tb.Helper()
	return apitest.Committed(tb, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	})
}

// postTaxed posts an entry of company 2000 on the date with a line of the
// tax code.
func (e env) postTaxed(tb testing.TB, code, day string) {
	tb.Helper()
	expense, bank := uuid.MustParse(e.account(tb, "2000", "520000")), uuid.MustParse(e.account(tb, "2000", "100000"))
	if err := apitest.Committed(tb, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
			Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: expense, Amount: 100, TaxCode: &code}, {AccountID: bank, Amount: -100}}})
		return err
	}); err != nil {
		tb.Fatalf("post with %s on %s: %v", code, day, err)
	}
}

// TestTaxCodesSingleRate follows the single-rate form (T4) on dated rates:
// valid from the first fiscal year, a changed rate starts today and ends
// the old one, a removed code ends today, a second change on one day
// replaces the first (no posting used it), and a re-added code starts
// today.
func TestTaxCodesSingleRate(t *testing.T) {
	e := setup(t)
	if got := e.taxRates(t, "V0", "V1"); got != "V0 tax 0 2026-01-01 open, V1 tax 19 2026-01-01 open" {
		t.Fatalf("initial: got %s", got)
	}
	changed := document(t)
	changed.Companies[1].TaxCodes[0].RatePercent = "16"
	changed.Companies[1].TaxCodes = changed.Companies[1].TaxCodes[:1] // V0 removed
	if err := e.applyTax(t, changed); err != nil {
		t.Fatal(err)
	}
	if got, want := e.taxRates(t, "V0", "V1"), "V0 tax 0 2026-01-01 today, V1 tax 19 2026-01-01 today, V1 tax 16 today open"; got != want {
		t.Errorf("changed: got %s, want %s", got, want)
	}
	again := document(t)
	again.Companies[1].TaxCodes[0].RatePercent = "17"
	if err := e.applyTax(t, again); err != nil {
		t.Fatalf("second change today: %v", err)
	}
	want := "V0 tax 0 2026-01-01 today, V0 tax 0 today open, V1 tax 19 2026-01-01 today, V1 tax 17 today open"
	if got := e.taxRates(t, "V0", "V1"); got != want {
		t.Errorf("changed again, V0 re-added: got %s, want %s", got, want)
	}
	if err := e.applyTax(t, again); err != nil {
		t.Fatal(err)
	}
	if got := e.taxRates(t, "V0", "V1"); got != want {
		t.Errorf("applied twice: got %s, want %s", got, want)
	}
}

// withCodes is the test document with company 2000's chart holding a BC
// input tax account and the codes added or replaced.
func withCodes(tb testing.TB, codes ...config.TaxCode) *config.Config {
	tb.Helper()
	doc := document(tb)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "141000", Name: "GST paid", Type: config.Asset})
	for _, c := range codes {
		replaced := false
		for i := range de.TaxCodes {
			if de.TaxCodes[i].Code == c.Code {
				de.TaxCodes[i], replaced = c, true
			}
		}
		if !replaced {
			de.TaxCodes = append(de.TaxCodes, c)
		}
	}
	return doc
}

// bc is a BC purchase code: GST 5%, then 4% from 2027-04-01 (set up
// ahead), and PST 7% non-deductible.
func bc(gst ...config.TaxRate) config.TaxCode {
	if len(gst) == 0 {
		gst = []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01", ValidTo: "2027-03-31"}, {RatePercent: "4", ValidFrom: "2027-04-01"}}
	}
	return config.TaxCode{Code: "I5", Description: "GST 5% + BC PST 7%", Category: "S", Components: []config.TaxComponent{
		{Key: "gst", Jurisdiction: "CA", AccountKey: config.KeyInput, Account: "141000", Rates: gst},
		{Key: "pst_bc", Jurisdiction: "CA-BC", AccountKey: config.KeyNonDeductible, Rates: []config.TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}},
	}}
}

// TestTaxCodesWithComponents: every dated rate is written when the
// configuration is activated, future ones too, and the ledger answers a
// code's components at a date.
func TestTaxCodesWithComponents(t *testing.T) {
	e := setup(t)
	if err := e.applyTax(t, withCodes(t, bc())); err != nil {
		t.Fatal(err)
	}
	if got, want := e.taxRates(t, "I5"), "I5 gst 5 2026-01-01 2027-04-01, I5 gst 4 2027-04-01 open, I5 pst_bc 7 2026-01-01 open"; got != want {
		t.Errorf("rates: got %s, want %s", got, want)
	}
	gst := uuid.MustParse(e.account(t, "2000", "141000"))
	for _, c := range []struct {
		day, rates string
	}{{"2026-06-01", "gst 5.000000, pst_bc 7.000000"}, {"2027-04-01", "gst 4.000000, pst_bc 7.000000"}} {
		t.Run(c.day, func(t *testing.T) {
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				tc, ok, err := ledger.Accounts{}.TaxCode(ctx, e.de, "I5", date(c.day))
				if err != nil || !ok {
					t.Fatalf("I5: got %v %v, want found", ok, err)
				}
				var rates []string
				for _, p := range tc.Components {
					rates = append(rates, p.Key+" "+p.RatePct)
				}
				if got := strings.Join(rates, ", "); got != c.rates {
					t.Errorf("components: got %s, want %s", got, c.rates)
				}
				if tc.Direction != "input" || tc.Kind != "standard" || tc.Category != "S" {
					t.Errorf("definition: got %s %s %s, want input standard S", tc.Direction, tc.Kind, tc.Category)
				}
				if g := tc.Components[0]; g.AccountID == nil || *g.AccountID != gst || g.Jurisdiction != "CA" || g.RecoverablePct != "100" {
					t.Errorf("gst: got %+v, want account 141000, CA, 100%% recoverable", g)
				}
				if p := tc.Components[1]; p.AccountID != nil || p.AccountKey != "non_deductible" || p.RecoverablePct != "0" {
					t.Errorf("pst: got %+v, want non-deductible without an account", p)
				}
				if _, ok := tc.Single(); ok {
					t.Error("single: got true for two components, want false")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestTaxRatesInUse: a rate a posting used is never changed, withdrawn or
// shortened past the posting, and no new rate covers a posted date (409
// ledger.tax_rate_in_use); a future rate nothing used changes freely.
func TestTaxRatesInUse(t *testing.T) {
	e := setup(t)
	if err := e.applyTax(t, withCodes(t, bc())); err != nil {
		t.Fatal(err)
	}
	e.postTaxed(t, "I5", "2026-03-15")
	refused := map[string]config.TaxCode{
		"a used rate changed": bc(config.TaxRate{RatePercent: "6", ValidFrom: "2026-01-01", ValidTo: "2027-03-31"},
			config.TaxRate{RatePercent: "4", ValidFrom: "2027-04-01"}),
		"a used rate ended before its posting": bc(config.TaxRate{RatePercent: "5", ValidFrom: "2026-01-01", ValidTo: "2026-02-28"},
			config.TaxRate{RatePercent: "4", ValidFrom: "2027-04-01"}),
		"a used rate starting later": bc(config.TaxRate{RatePercent: "5", ValidFrom: "2026-04-01", ValidTo: "2027-03-31"},
			config.TaxRate{RatePercent: "4", ValidFrom: "2027-04-01"}),
	}
	added := bc()
	added.Components = append(added.Components, config.TaxComponent{Key: "extra", AccountKey: config.KeyInput, Account: "140000",
		Rates: []config.TaxRate{{RatePercent: "1", ValidFrom: "2026-01-01"}}})
	refused["a new component over posted dates"] = added
	for name, code := range refused {
		t.Run(name, func(t *testing.T) {
			err := e.applyTax(t, withCodes(t, code))
			if got := problem.From(err); got.Status != 409 || got.Code != "ledger.tax_rate_in_use" {
				t.Errorf("activation: got %v, want 409 ledger.tax_rate_in_use", err)
			}
		})
	}
	if got, want := e.taxRates(t, "I5"), "I5 gst 5 2026-01-01 2027-04-01, I5 gst 4 2027-04-01 open, I5 pst_bc 7 2026-01-01 open"; got != want {
		t.Errorf("after the refusals: got %s, want %s", got, want)
	}
	future := bc(config.TaxRate{RatePercent: "5", ValidFrom: "2026-01-01", ValidTo: "2027-06-30"}, config.TaxRate{RatePercent: "3.5", ValidFrom: "2027-07-01"})
	if err := e.applyTax(t, withCodes(t, future)); err != nil {
		t.Fatalf("a future rate changed: %v", err)
	}
	if got, want := e.taxRates(t, "I5"), "I5 gst 5 2026-01-01 2027-07-01, I5 gst 3.5 2027-07-01 open, I5 pst_bc 7 2026-01-01 open"; got != want {
		t.Errorf("future rate moved: got %s, want %s", got, want)
	}
}

// TestTaxCodesFixed: a code's direction and a component's account key
// never change once activated: submission reports them and activation
// refuses them (409 ledger.tax_code_fixed). A component's account changes
// and keeps the former one, whose lines stay its tax lines.
func TestTaxCodesFixed(t *testing.T) {
	e := setup(t)
	turned := withCodes(t, config.TaxCode{Code: "V1", Description: "Output", Direction: config.TaxOutput, RatePercent: "19", Account: "140000"})
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := s.Check(ctx, document(t), turned)
		var paths []string
		for _, f := range fs {
			paths = append(paths, f.Path)
		}
		if got, want := strings.Join(paths, ", "), "companies[2000].tax_codes[V1].components[tax].account_key, companies[2000].tax_codes[V1].direction"; got != want {
			t.Errorf("findings: got %s, want %s", got, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := problem.From(e.applyTax(t, turned)); got.Status != 409 || got.Code != "ledger.tax_code_fixed" {
		t.Errorf("activation: got %v, want 409 ledger.tax_code_fixed", got)
	}
	moved := withCodes(t, config.TaxCode{Code: "V1", Description: "Input tax, standard rate", RatePercent: "19", Account: "141000"})
	if err := e.applyTax(t, moved); err != nil {
		t.Fatal(err)
	}
	var former []uuid.UUID
	pgtest.Scan(t, `SELECT former_account_ids FROM ledger.tax_component WHERE legal_entity_id = $1 AND code = 'V1'`, []any{e.de}, &former)
	if old := uuid.MustParse(e.account(t, "2000", "140000")); len(former) != 1 || former[0] != old {
		t.Errorf("former accounts: got %v, want [%s]", former, old)
	}
}

// TestTaxSetupWaitsForPostings: a setup changing a rate waits for a
// posting of the code in flight, which holds the code's definition, and
// then sees it: the used rate is not withdrawn (409 ledger.tax_rate_in_use).
func TestTaxSetupWaitsForPostings(t *testing.T) {
	e := setup(t)
	if err := e.applyTax(t, withCodes(t, bc())); err != nil {
		t.Fatal(err)
	}
	expense, bank := uuid.MustParse(e.account(t, "2000", "520000")), uuid.MustParse(e.account(t, "2000", "100000"))
	post := func(code string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"),
				Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
				Lines: []ledger.PostingLine{{AccountID: expense, Amount: 100, TaxCode: &code}, {AccountID: bank, Amount: -100}}})
			return err
		}
	}
	changed := withCodes(t, bc(config.TaxRate{RatePercent: "6", ValidFrom: "2026-01-01", ValidTo: "2027-03-31"}, config.TaxRate{RatePercent: "4", ValidFrom: "2027-04-01"}))
	change := func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, changed)
	}
	t.Run("posting first", func(t *testing.T) {
		first, second := serializeOn(t, e.ten, "the tax code", lockWait(t, "FOR UPDATE"), post("I5"), change)
		if first != nil {
			t.Fatalf("posting: got %v, want it committed", first)
		}
		if got := problem.From(second); got.Status != 409 || got.Code != "ledger.tax_rate_in_use" {
			t.Errorf("setup after the posting: got %v, want 409 ledger.tax_rate_in_use", second)
		}
	})
	t.Run("setup first", func(t *testing.T) {
		// I6 is valid from 2026-03-01; the setup moves both its components'
		// start to 2026-04-01 while the posting of 2026-03-15 waits for it,
		// which then finds no rate in force.
		from := func(day string) config.TaxCode {
			c := bc(config.TaxRate{RatePercent: "5", ValidFrom: day})
			c.Code, c.Components[1].Rates[0].ValidFrom = "I6", day
			return c
		}
		if err := e.applyTax(t, withCodes(t, bc(), from("2026-03-01"))); err != nil {
			t.Fatal(err)
		}
		takeBack := func(ctx context.Context) error {
			return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, withCodes(t, bc(), from("2026-04-01")))
		}
		first, second := serializeOn(t, e.ten, "the tax code", lockWait(t, "FOR KEY SHARE"), takeBack, post("I6"))
		if first != nil {
			t.Fatalf("setup: got %v, want it committed", first)
		}
		if got := problem.From(second); got.Status != 422 || got.Code != "ledger.unknown_tax_code" {
			t.Errorf("posting after the setup: got %v, want 422 ledger.unknown_tax_code", second)
		}
	})
}

// lockWait reports whether a session of this test database waits for a
// lock on a tax code definition taken with the given clause.
func lockWait(t *testing.T, clause string) func() bool {
	t.Helper()
	admin := pgtest.Admin(t)
	return func() bool {
		var n int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND datname = current_database() AND pid <> pg_backend_pid() AND query LIKE '%tax_code_definition%' || $1 || '%'`, clause).Scan(&n); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return n > 0
	}
}
