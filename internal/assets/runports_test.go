package assets_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestRunLedgerReads: what the ledger publishes for a depreciation run
// (ADR-0057 §4, §7, T57f): whether a period is open in a ledger (409
// ledger.period_not_open once closed, 422 ledger.no_fiscal_period for one
// not set up there), and local amounts in the group currency at the
// date's average rate, rounded half away from zero (422
// ledger.no_exchange_rate without a rate, an error out of range).
func TestRunLedgerReads(t *testing.T) {
	e := newRuns(t)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 7`, e.DE, e.l0)
	june := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		fn   func(ctx context.Context) (string, error)
		want string
	}{
		"an open period": {func(ctx context.Context) (string, error) {
			return "", ledger.AssetReads{}.PeriodOpen(ctx, e.DE, e.l0, 2026, 6)
		}, ""},
		"a closed period": {func(ctx context.Context) (string, error) {
			return "", ledger.AssetReads{}.PeriodOpen(ctx, e.DE, e.l0, 2026, 7)
		}, "ledger.period_not_open"},
		"a ledger the company does not keep": {func(ctx context.Context) (string, error) {
			return "", ledger.AssetReads{}.PeriodOpen(ctx, e.DE, e.l3, 2026, 6)
		}, "ledger.no_fiscal_period"},
		"into the group currency": {func(ctx context.Context) (string, error) {
			out, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "USD", june, []money.Minor{1000, -1000, 1, 0})
			return fmt.Sprint(out), err
		}, "[1087 minor -1087 minor 1 minor 0 minor]"},
		"in the same currency": {func(ctx context.Context) (string, error) {
			out, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "EUR", june, []money.Minor{1234})
			return fmt.Sprint(out), err
		}, "[1234 minor]"},
		"without a rate": {func(ctx context.Context) (string, error) {
			_, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "GBP", june, []money.Minor{1})
			return "", err
		}, "ledger.no_exchange_rate"},
		"out of range": {func(ctx context.Context) (string, error) {
			_, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "USD", june, []money.Minor{math.MaxInt64})
			return "", err
		}, "out of range"},
		"an unknown currency": {func(ctx context.Context) (string, error) {
			_, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "XXX", june, nil)
			return "", err
		}, "ledger.no_exchange_rate"},
		"a currency not set up": {func(ctx context.Context) (string, error) {
			_, err := ledger.AssetReads{}.GroupAmounts(ctx, "XXX", "XXX", june, nil)
			return "", err
		}, "XXX"},
	} {
		t.Run(name, func(t *testing.T) {
			var got string
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				got, err = c.fn(ctx)
				return err
			})
			if err != nil {
				got = fmt.Sprint(problem.From(err).Code, " ", err)
			}
			if !strings.Contains(got, c.want) || (c.want == "" && err != nil) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"the period's status": func(ctx context.Context) error { return ledger.AssetReads{}.PeriodOpen(ctx, e.DE, e.l0, 2026, 6) },
		"the group amounts": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.GroupAmounts(ctx, "EUR", "USD", june, []money.Minor{1})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Errorf("outside a unit of work: got nil, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
}

// TestLedgerSpecificPosting: a context's posting naming a ledger goes to
// that ledger alone (ledger.Posting.LedgerID, ADR-0057 §4), and to one
// its company does not keep is refused with 422 ledger.ledger_not_kept.
func TestLedgerSpecificPosting(t *testing.T) {
	e := newRuns(t)
	post := func(l uuid.UUID) (ledger.Entry, error) {
		var out ledger.Entry
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			lc, gc := money.Minor(1000), money.Minor(1087)
			nlc, ngc := -lc, -gc
			p := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: taxfiling.Holds{}}
			var err error
			out, err = p.Post(ctx, ledger.Posting{LegalEntityID: e.DE, LedgerID: &l, DocumentType: "AF", DocumentDate: assetDay, PostingDate: assetDay,
				Currency: "EUR", SourceContext: "assets", SourceDocumentID: uuid.Must(uuid.NewV7()), Lines: []ledger.PostingLine{
					{AccountID: e.Account(t, "2000", "620000"), Amount: lc, LC: &lc, GC: &gc, CostCenterID: &e.c.Admin},
					{AccountID: e.Account(t, "2000", "160900"), Amount: nlc, LC: &nlc, GC: &ngc}}})
			return err
		})
		return out, err
	}
	en, err := post(e.l2)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "the entry's ledger", en.LedgerID, e.l2)
	if _, err := post(e.l3); problem.From(err).Code != "ledger.ledger_not_kept" {
		t.Errorf("a ledger not kept: got %v, want ledger.ledger_not_kept", err)
	}
}
