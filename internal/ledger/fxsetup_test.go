package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// fxValuations is company 2000's foreign-currency valuation as the ledger
// keeps it: its rate type, flagged accounts and active valuations, as
// account:adjustment:gain:loss.
func fxValuations(t *testing.T, e env) (rateType, flagged, valued string) {
	t.Helper()
	pgtest.Scan(t, `SELECT (SELECT rate_type FROM ledger.fx_valuation WHERE tenant_id = $1 AND legal_entity_id = $2),
		(SELECT coalesce(string_agg(g.number, ',' ORDER BY g.number), '') FROM ledger.company_account c JOIN ledger.gl_account g ON g.id = c.account_id
			WHERE c.tenant_id = $1 AND c.legal_entity_id = $2 AND c.revalue_foreign_currency),
		(SELECT coalesce(string_agg(a.number || ':' || coalesce(j.number, '') || ':' || g.number || ':' || l.number, ',' ORDER BY a.number), '')
			FROM ledger.fx_valuation_account v JOIN ledger.gl_account a ON a.id = v.account_id LEFT JOIN ledger.gl_account j ON j.id = v.adjustment_account_id
			JOIN ledger.gl_account g ON g.id = v.gain_account_id JOIN ledger.gl_account l ON l.id = v.loss_account_id
			WHERE v.tenant_id = $1 AND v.legal_entity_id = $2 AND v.is_active)`,
		[]any{e.ten.ID, e.de}, &rateType, &flagged, &valued)
	return rateType, flagged, valued
}

// TestFXValuationSetup: the setup keeps each company's rate type, flagged
// accounts and valuation accounts (ADR-0042), changes them with the
// document and deactivates and unflags what it no longer has.
func TestFXValuationSetup(t *testing.T) {
	e := setup(t)
	apply := func(t *testing.T, change func(co *config.Company)) {
		t.Helper()
		doc := document(t)
		co := doc.Company("2000")
		co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "211099", Name: "Payables valuation", Type: config.Liability})
		change(co)
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
		}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, want ...string) {
		t.Helper()
		rateType, flagged, valued := fxValuations(t, e)
		if got := strings.Join([]string{rateType, flagged, valued}, " | "); got != strings.Join(want, " | ") {
			t.Errorf("FX valuation: got %s, want %s", got, strings.Join(want, " | "))
		}
	}
	check(t, "average", "", "")
	t.Run("configured", func(t *testing.T) {
		apply(t, func(co *config.Company) {
			co.Account("100000").RevalueForeignCurrency = true
			co.FXValuation = config.FXValuation{RateType: "bank_selling", Accounts: []config.FXValuationAccount{
				{Account: "100000", UnrealisedGain: "400000", UnrealisedLoss: "610000"},
				{Account: "211000", Adjustment: "211099", UnrealisedGain: "400000", UnrealisedLoss: "610000"}}}
		})
		check(t, "bank_selling", "100000", "100000::400000:610000,211000:211099:400000:610000")
	})
	t.Run("changed", func(t *testing.T) {
		apply(t, func(co *config.Company) {
			co.FXValuation = config.FXValuation{Accounts: []config.FXValuationAccount{
				{Account: "211000", Adjustment: "211099", UnrealisedGain: "400000", UnrealisedLoss: "520000"}}}
		})
		check(t, "average", "", "211000:211099:400000:520000")
	})
	t.Run("removed", func(t *testing.T) {
		apply(t, func(*config.Company) {})
		check(t, "average", "", "")
	})
}

// TestFXValuationLedgerSetup: the setup keeps the rate types of the
// parallel ledgers that value at their own (T52f, ADR-0045), changes them
// with the document and deactivates what it no longer names; and each of
// its statements fails in turn.
func TestFXValuationLedgerSetup(t *testing.T) {
	p := setupParallel(t)
	apply := func(t *testing.T, rateType string) {
		t.Helper()
		doc := revaluationDoc(parallel(t), true)
		if rateType != "" {
			doc.Company("2000").FXValuation.Ledgers = []config.FXValuationLedger{{Code: "2L", RateType: rateType}}
		}
		applyRevaluation(t, p.env, doc)
	}
	check := func(t *testing.T, want string) {
		t.Helper()
		var got string
		pgtest.Scan(t, `SELECT coalesce(string_agg(l.code || ':' || v.rate_type || ':' || v.is_active, ','), '') FROM ledger.fx_valuation_ledger v
			JOIN ledger.ledger l ON l.id = v.ledger_id WHERE v.tenant_id = $1 AND v.legal_entity_id = $2`, []any{p.ten.ID, p.de}, &got)
		if got != want {
			t.Errorf("ledgers' rate types: got %q, want %q", got, want)
		}
	}
	for _, c := range []struct{ name, rateType, want string }{
		{"configured", "bank_buying", "2L:bank_buying:true"},
		{"unchanged", "bank_buying", "2L:bank_buying:true"},
		{"changed", "planning", "2L:planning:true"},
		{"removed", "", "2L:planning:false"},
		{"named again", "average", "2L:average:true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			apply(t, c.rateType)
			check(t, c.want)
		})
	}
	doc := revaluationDoc(parallel(t), true)
	doc.Company("2000").FXValuation.Ledgers = []config.FXValuationLedger{{Code: "2L", RateType: "bank_selling"}}
	apitest.EveryFailure(t, p.ten, p.ten.Human, "setup apply FX valuation per ledger", func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	})
}
