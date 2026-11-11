package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestConsolidationReadsOutsideRequest: the ledger's reads for
// consolidation run only in a request's or activity's unit of work; their
// figures and database failures are tested through the consolidation
// runs (internal/consolidation).
func TestConsolidationReadsOutsideRequest(t *testing.T) {
	ctx, r, id := context.Background(), ledger.ConsolidationReads{}, uuid.New()
	for name, read := range map[string]func() error{
		"the leading ledger": func() error { _, err := r.LeadingLedger(ctx); return err },
		"a period":           func() error { _, _, err := r.PeriodOf(ctx, id, id, 2026, 11); return err },
		"periods":            func() error { _, err := r.PeriodsOf(ctx, []uuid.UUID{id}, id, 2026, 11); return err },
		"the accounts":       func() error { _, err := r.AccountsOf(ctx, id); return err },
		"the balances":       func() error { _, _, err := r.BalancesThrough(ctx, id, id, 2026, 11, nil); return err },
		"a rate":             func() error { _, _, err := r.RateOn(ctx, "average", "USD", "EUR", time.Now()); return err },
	} {
		if err := read(); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
}

// valuedLines renders the lines BalancesThrough gives company 2000
// through April 2026, naming the valued accounts asked for: each line's
// account number, trading partner, local amount and the number of the
// account it values, "-" for none.
func valuedLines(t *testing.T, e env, valued ...string) string {
	t.Helper()
	numbers := map[uuid.UUID]string{}
	for _, n := range []string{"100000", "211000", "400000", "610000"} {
		numbers[settleAccount(t, e, "2000", n)] = n
	}
	ids := make([]uuid.UUID, len(valued))
	for i, n := range valued {
		ids[i] = settleAccount(t, e, "2000", n)
	}
	var lines []ledger.RevaluationLine
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		leading, err := ledger.ConsolidationReads{}.LeadingLedger(ctx)
		if err != nil {
			return err
		}
		_, lines, err = ledger.ConsolidationReads{}.BalancesThrough(ctx, e.de, leading, 2026, 4, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range lines {
		tp, of := "-", "-"
		if l.TradingPartnerID != nil {
			tp = "1000"
		}
		if l.ValuedAccountID != nil {
			of = numbers[*l.ValuedAccountID]
		}
		out = append(out, fmt.Sprintf("%s %s %d of %s", numbers[l.AccountID], tp, l.LC, of))
	}
	return strings.Join(out, " | ")
}

// TestRevaluationLinesNameTheirValuedAccount: a line of a revaluation
// run's entries on a profit and loss account names the account whose
// difference it offsets, for the accounts asked for (ADR-0059 As built:
// a net investment's exchange differences). tradingPartnerFixture's run
// at 31 March has four totals: the bank 100000's gains without a trading
// partner and with company 1000, offset on 400000, and the payables
// 211000's losses, posted on its adjustment account 211099 and offset on
// 610000. Each offset names its total's account, the adjustment account's
// too, in the valuation entry and in the reset; a line of an account not
// asked for names none, and no line does when none is asked for. With the
// run cancelled the reversals' lines name them as the entries they
// reverse do.
func TestRevaluationLinesNameTheirValuedAccount(t *testing.T) {
	e := tradingPartnerFixture(t)
	run, etag := e.in(t).Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	both := "400000 - -5000 of 100000 | 400000 - 5000 of 100000 | 400000 1000 -3000 of 100000 | 400000 1000 3000 of 100000 | " +
		"610000 - 1000 of 211000 | 610000 - -1000 of 211000 | 610000 1000 400 of 211000 | 610000 1000 -400 of 211000"
	for _, c := range []struct {
		name   string
		valued []string
		want   string
	}{
		{"both accounts", []string{"100000", "211000"}, both},
		{"the reconciliation account alone", []string{"211000"}, strings.ReplaceAll(both, "of 100000", "of -")},
		{"none", nil, strings.ReplaceAll(strings.ReplaceAll(both, "of 100000", "of -"), "of 211000", "of -")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := valuedLines(t, e, c.valued...); got != c.want {
				t.Errorf("lines:\ngot  %s\nwant %s", got, c.want)
			}
		})
	}
	t.Run("a cancelled run", func(t *testing.T) {
		e.in(t).Must(200, "POST", "/v1/revaluation-runs/"+apitest.String(t, run, "id")+":cancel", `{"reason": "March's rate was keyed wrongly."}`, etag)
		got := strings.Split(valuedLines(t, e, "100000"), " | ")
		if len(got) != 16 {
			t.Fatalf("lines: got %d (%v), want 16: the entries' eight and their reversals'", len(got), got)
		}
		var named, sum int
		for _, l := range got {
			var account, tp, of string
			var lc int
			if _, err := fmt.Sscanf(l, "%s %s %d of %s", &account, &tp, &lc, &of); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			if (account == "400000") != (of == "100000") {
				t.Errorf("line %q: got valued account %s, want 100000 exactly on 400000's lines", l, of)
			}
			if of == "100000" {
				named, sum = named+1, sum+lc
			}
		}
		if named != 8 || sum != 0 {
			t.Errorf("lines naming 100000: got %d summing to %d, want 8 summing to 0", named, sum)
		}
	})
}
