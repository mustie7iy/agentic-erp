package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Revaluation of intercompany items by trading partner (T68b2, ADR-0059
// §4, ADR-0042): an open item carries its line's trading partner, a
// flagged account's balance is one per currency and trading partner, and
// a total, and both lines it posts, one per account, currency, direction
// and trading partner, so a consolidation run matches a revalued
// intercompany balance by trading partner. The items naming none are
// valued and posted as they were before trading partners.

// tradingPartnerFixture is runFixture with an intercompany twin of each of
// its items in company 2000, naming company 1000 as trading partner: a
// USD invoice of 40.00 booked at 0.8 (a loss of 4.00 at 0.9) and USD
// 300.00 on the flagged bank account (a gain of 30.00).
func tradingPartnerFixture(t *testing.T) env {
	t.Helper()
	e := runFixture(t)
	expense, ap, bank := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000")
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: expense, Amount: 4000, TradingPartner: &e.us},
		ledger.PostingLine{AccountID: ap, Amount: -4000, PartnerID: &e.supplier, TradingPartner: &e.us})
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: bank, Amount: 30000, TradingPartner: &e.us},
		ledger.PostingLine{AccountID: expense, Amount: -30000, TradingPartner: &e.us})
	return e
}

// tpOf is a representation's trading partner as "-" for none, else its
// code or, unnamed, "id".
func tpOf(e env, m map[string]any) string {
	tp, ok := m["trading_partner"].(map[string]any)
	switch {
	case !ok:
		return "-"
	case tp["id"] != e.us.String():
		return fmt.Sprint("another ", tp["id"])
	case tp["code"] == nil:
		return "id"
	}
	return fmt.Sprint(tp["code"], " ", tp["name"])
}

// revaluationRows renders a preview's or run's items and totals: account
// number, trading partner, and the differences.
func revaluationRows(t *testing.T, e env, rep map[string]any) (items, totals string) {
	t.Helper()
	names := map[string]string{settleAccount(t, e, "2000", "211000").String(): "211000", settleAccount(t, e, "2000", "100000").String(): "100000"}
	amount := func(m map[string]any, k string) any { return m[k].(map[string]any)["amount"] }
	var is, ts []string
	for _, x := range apitest.List(t, rep, "items") {
		it := x.(map[string]any)
		is = append(is, fmt.Sprintf("%s %s %v %v %v", names[it["account_id"].(string)], tpOf(e, it), amount(it, "amount"), amount(it, "difference_lc"),
			amount(it, "difference_gc")))
	}
	for _, x := range apitest.List(t, rep, "totals") {
		s := x.(map[string]any)
		ts = append(ts, fmt.Sprintf("%s %s %v %v %v %v", names[s["account_id"].(string)], tpOf(e, s), s["direction"], s["item_count"],
			amount(s, "difference_lc"), amount(s, "difference_gc")))
	}
	return strings.Join(is, " | "), strings.Join(ts, " | ")
}

// tpEntryLines renders an entry's lines by account number, with each
// line's trading partner, and the sums of its local and group amounts.
func tpEntryLines(t *testing.T, e env, id string) string {
	t.Helper()
	numbers := map[uuid.UUID]string{}
	for _, n := range []string{"100000", "211099", "400000", "610000"} {
		numbers[settleAccount(t, e, "2000", n)] = n
	}
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = ledger.GetEntry(ctx, uuid.MustParse(id))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	var lc, gc money.Minor
	for _, l := range entry.Lines {
		tp := "-"
		if l.TradingPartnerID != nil {
			tp = map[bool]string{true: "1000", false: l.TradingPartnerID.String()}[*l.TradingPartnerID == e.us]
		}
		lines = append(lines, fmt.Sprintf("%s %s %d %d", numbers[l.AccountID], tp, l.LC, l.GC))
		lc, gc = lc+l.LC, gc+l.GC
	}
	return fmt.Sprintf("%s; sums %d %d", strings.Join(lines, " | "), lc, gc)
}

// TestRevaluationByTradingPartner: the preview values the intercompany
// invoice with its trading partner and the bank's balance once without
// one and once for company 1000; the totals are per trading partner, the
// ones naming none exactly runFixture's (a loss of 10.00, a gain of
// 50.00); the run posts each total's two lines with its trading partner,
// balanced, its reset likewise; it records and reads back the trading
// partners, named only to a caller who may act in company 1000; a second
// run of the key date is refused and the preview answers the same again.
func TestRevaluationByTradingPartner(t *testing.T) {
	e := tradingPartnerFixture(t)
	body := preview(e.de, "2026-03-31")
	wantItems := "100000 - 500.00 50.00 50.00 | 100000 1000 Acme Corp US 300.00 30.00 30.00 | 211000 - -100.00 -10.00 -10.00 | " +
		"211000 1000 Acme Corp US -40.00 -4.00 -4.00"
	wantTotals := "100000 - gain 1 50.00 50.00 | 100000 1000 Acme Corp US gain 1 30.00 30.00 | 211000 - loss 1 -10.00 -10.00 | " +
		"211000 1000 Acme Corp US loss 1 -4.00 -4.00"
	previewed, _ := e.in(t).Must(200, "POST", "/v1/revaluation-runs/preview", body, "")
	t.Run("preview", func(t *testing.T) {
		items, totals := revaluationRows(t, e, previewed)
		if items != wantItems {
			t.Errorf("items:\ngot  %s\nwant %s", items, wantItems)
		}
		if totals != wantTotals {
			t.Errorf("totals:\ngot  %s\nwant %s", totals, wantTotals)
		}
	})
	run, _ := e.in(t).Must(201, "POST", "/v1/revaluation-runs", body, "")
	id := apitest.String(t, run, "id")
	t.Run("entries", func(t *testing.T) {
		if g, w := tpEntryLines(t, e, apitest.String(t, run, "valuation_entry_id")),
			"100000 - 5000 5000 | 400000 - -5000 -5000 | 100000 1000 3000 3000 | 400000 1000 -3000 -3000 | "+
				"211099 - -1000 -1000 | 610000 - 1000 1000 | 211099 1000 -400 -400 | 610000 1000 400 400; sums 0 0"; g != w {
			t.Errorf("valuation entry:\ngot  %s\nwant %s", g, w)
		}
		if g, w := tpEntryLines(t, e, apitest.String(t, run, "reversal_entry_id")),
			"100000 - -5000 -5000 | 400000 - 5000 5000 | 100000 1000 -3000 -3000 | 400000 1000 3000 3000 | "+
				"211099 - 1000 1000 | 610000 - -1000 -1000 | 211099 1000 400 400 | 610000 1000 -400 -400; sums 0 0"; g != w {
			t.Errorf("reset entry:\ngot  %s\nwant %s", g, w)
		}
	})
	t.Run("recorded and read back", func(t *testing.T) {
		got, _ := e.in(t).Must(200, "GET", "/v1/revaluation-runs/"+id, "", "")
		for name, rep := range map[string]map[string]any{"posted": run, "read back": got} {
			items, totals := revaluationRows(t, e, rep)
			if items != wantItems || totals != wantTotals {
				t.Errorf("%s:\ngot  %s; %s\nwant %s; %s", name, items, totals, wantItems, wantTotals)
			}
		}
	})
	t.Run("named only with access to the trading partner", func(t *testing.T) {
		got, _ := e.actingIn(t, e.de).Must(200, "GET", "/v1/revaluation-runs/"+id, "", "")
		items, totals := revaluationRows(t, e, got)
		if w := strings.ReplaceAll(wantItems, "1000 Acme Corp US", "id"); items != w {
			t.Errorf("items:\ngot  %s\nwant %s", items, w)
		}
		if w := strings.ReplaceAll(wantTotals, "1000 Acme Corp US", "id"); totals != w {
			t.Errorf("totals:\ngot  %s\nwant %s", totals, w)
		}
	})
	t.Run("run twice", func(t *testing.T) {
		e.in(t).Problem(409, "ledger.revaluation_exists", "POST", "/v1/revaluation-runs", body, "")
		again, _ := e.in(t).Must(200, "POST", "/v1/revaluation-runs/preview", body, "")
		if fmt.Sprint(again["items"], again["totals"]) != fmt.Sprint(previewed["items"], previewed["totals"]) {
			t.Errorf("preview after the run:\ngot  %v %v\nwant %v %v", again["items"], again["totals"], previewed["items"], previewed["totals"])
		}
	})
}
