package ledger_test

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// pairRate adds an average rate between two currencies.
func pairRate(t *testing.T, e env, from, to, validFrom, rate string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', $2, $3, $4, $5, 'ECB', $6)`, e.ten.ID, from, to, validFrom, rate, e.ten.Human.ID)
}

// TestApplyReceiptInAnotherCurrency (T75n, ADR-0063 §6 As built): items in
// another currency than the receipt are translated into it at the average
// rate valid on the posting date and cleared at their own local and group
// amounts, what is left being the realized exchange difference; a residual
// of a translated item is in the receipt's currency at the item's local
// rate; each item takes the rate of its own currency.
func TestApplyReceiptInAnotherCurrency(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.20")
	settleRate(t, r.env, "GBP", "2026-04-01", "1.10")
	settleRate(t, r.env, "USD", "2026-03-01", "0.90")
	gain := settleAccount(t, r.env, "2000", "400000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
		r.ten.ID, r.de, gain, r.ten.Human.ID)
	for _, c := range []struct {
		name         string
		in           func() ledger.ReceiptApplication
		lines        string
		items        []money.Minor
		translations []ledger.Translation
	}{
		{"local items paid in GBP at the receipt's rate", func() ledger.ReceiptApplication {
			return r.application(r.received(t, 10000, "GBP", "2026-03-20"), north, "GBP", "2026-03-21", r.owed(t, north, 12000, "EUR", "2026-03-10"))
		}, "103000 10000/12000/12000; 120000 -10000/-12000/-12000", []money.Minor{10000}, []ledger.Translation{{Currency: "EUR", Amount: 12000, Rate: "0.833333333333"}}},
		{"a later rate realizes a gain", func() ledger.ReceiptApplication {
			return r.application(r.received(t, 10000, "GBP", "2026-03-25"), north, "GBP", "2026-04-10", r.owed(t, north, 11000, "EUR", "2026-03-10"))
		}, "103000 10000/12000/12000; 120000 -10000/-11000/-11000; 400000 0/-1000/0; 400000 0/0/-1000", []money.Minor{10000},
			[]ledger.Translation{{Currency: "EUR", Amount: 11000, Rate: "0.909090909091"}}},
		{"items in two other currencies, each at its rate", func() ledger.ReceiptApplication {
			return r.application(r.received(t, 22800, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, 10000, "GBP", "2026-03-10"),
				r.owed(t, north, 12000, "USD", "2026-03-10"))
		}, "103000 22800/22800/22800; 120000 -12000/-12000/-12000; 120000 -10800/-10800/-10800", []money.Minor{12000, 10800},
			[]ledger.Translation{{Currency: "GBP", Amount: 10000, Rate: "1.200000000000"}, {Currency: "USD", Amount: 12000, Rate: "0.900000000000"}}},
		{"a residual at the item's local rate", func() ledger.ReceiptApplication {
			in := r.application(r.received(t, 6000, "GBP", "2026-03-20"), north, "GBP", "2026-03-21", r.owed(t, north, 12000, "EUR", "2026-03-10"))
			in.Residuals = []ledger.Residual{{Amount: 4000}}
			return in
		}, "103000 6000/7200/7200; 120000 -10000/-12000/-12000; 120000 4000/4800/4800", []money.Minor{10000},
			[]ledger.Translation{{Currency: "EUR", Amount: 12000, Rate: "0.833333333333"}}},
		{"an item in the receipt's currency is not translated", func() ledger.ReceiptApplication {
			return r.application(r.received(t, 10000, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, 10000, "EUR", "2026-03-10"))
		}, "103000 10000/10000/10000; 120000 -10000/-10000/-10000", []money.Minor{10000}, []ledger.Translation{{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			applied := r.apply(t, c.in())
			if got := posted(t, r.env, applied.Entry.ID); got != c.lines {
				t.Errorf("lines: got %s, want %s", got, c.lines)
			}
			if len(applied.Items) != len(c.items) || len(applied.Translations) != len(c.items) {
				t.Fatalf("items: got %v with translations %+v, want %v", applied.Items, applied.Translations, c.items)
			}
			for i, want := range c.items {
				if applied.Items[i] != want {
					t.Errorf("item %d: got %d, want %d", i+1, applied.Items[i], want)
				}
				if applied.Translations[i] != c.translations[i] {
					t.Errorf("item %d's translation: got %+v, want %+v", i+1, applied.Translations[i], c.translations[i])
				}
			}
		})
	}
	if open := receivable(t, r.env, north); len(open) != 1 || open[0].Amount != 4000 || open[0].Currency != "GBP" {
		t.Errorf("the customer's open items: got %+v, want the residual of GBP 40.00 alone", open)
	}
}

// TestApplyReceiptInAnotherCurrencyRefusals: an item whose currency has no
// rate into the receipt's on the posting date, one that is nothing in the
// receipt's currency and one whose translation is out of range are
// refused, each naming its item.
func TestApplyReceiptInAnotherCurrencyRefusals(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north, south := r.newCustomer(t, "BP00000031", true), r.newCustomer(t, "BP00000032", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.20")
	settleRate(t, r.env, "USD", "2026-03-01", "0.90")
	settleRate(t, r.env, "JPY", "2026-03-01", "0.025")
	dollars, yen := r.received(t, 10000, "USD", "2026-03-20"), r.received(t, 100, "JPY", "2026-03-20")
	sterling, cent := r.owed(t, north, 10000, "GBP", "2026-03-10"), r.owed(t, north, 1, "EUR", "2026-03-10")
	souths := r.owed(t, south, 10000, "GBP", "2026-03-10")
	for _, c := range []struct {
		name, code, detail, field string
		in                        ledger.ReceiptApplication
	}{
		{"no rate between the currencies", "ledger.no_exchange_rate", "Item 1 is in GBP, the receipt in USD, and no average exchange rate between the two is valid on 2026-03-21",
			"items[0]", r.application(dollars, north, "USD", "2026-03-21", sterling)},
		{"another customer's item before its rate", "ledger.not_the_customers_item", "", "items[0]", r.application(dollars, north, "USD", "2026-03-21", souths)},
		{"an item that is nothing in the receipt's currency", "ledger.item_rounds_to_zero", "Item 1, 0.01 in EUR, is nothing in JPY", "items[0]",
			r.application(yen, north, "JPY", "2026-03-21", cent)},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := r.refusal(t, c.in)
			if p.Code != c.code || !strings.Contains(p.Detail, c.detail) {
				t.Errorf("got %s (%s), want %s with %q", p.Code, p.Detail, c.code, c.detail)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field errors: got %+v, want one naming %s", p.Errors, c.field)
			}
		})
	}
	t.Run("a translation out of range", func(t *testing.T) {
		pairRate(t, r.env, "GBP", "USD", "2026-03-21", "1000000000000000")
		p := r.refusal(t, r.application(dollars, north, "USD", "2026-03-21", sterling))
		if p.Code != "ledger.amount_too_large" || len(p.Errors) != 1 || p.Errors[0].Field != "items[0]" {
			t.Errorf("got %s with %+v, want ledger.amount_too_large naming items[0]", p.Code, p.Errors)
		}
	})
	if open := receivable(t, r.env, north); len(open) != 2 {
		t.Errorf("the customer's open items after the refusals: got %d, want 2", len(open))
	}
}
