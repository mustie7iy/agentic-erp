package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// determine sets an account determination key of a legal entity.
func determine(t *testing.T, e env, le uuid.UUID, key string, account uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, $3, $4, $5)`,
		e.ten.ID, le, key, account, e.ten.Human.ID)
}

// lineAmounts is an entry's amounts per account, as "tc/lc" summed over
// its lines (an exchange difference has a line per currency).
func lineAmounts(entry ledger.Entry) map[uuid.UUID]string {
	tc, lc := map[uuid.UUID]money.Minor{}, map[uuid.UUID]money.Minor{}
	for _, l := range entry.Lines {
		tc[l.AccountID] += l.TC
		lc[l.AccountID] += l.LC
	}
	out := map[uuid.UUID]string{}
	for a := range tc {
		out[a] = fmt.Sprintf("%d/%d", tc[a], lc[a])
	}
	return out
}

// TestSettleTakesTheDiscount: a settlement takes each item's cash discount
// (T36c): the bank is credited with the items less the discounts and the
// discounts are credited to the cash_discount account, both converted at
// the posting date's rates, so the items still clear at their original
// amounts and what is left in local currency is the exchange difference.
// A discount of the other sign, of the whole amount or not matched to an
// item is refused, and so is one where no cash discount account is
// determined.
func TestSettleTakesTheDiscount(t *testing.T) {
	e := setup(t)
	settleRate(t, e, "USD", "2026-03-01", "0.90")
	settleRate(t, e, "USD", "2026-04-01", "0.80")
	cash, gain := settleAccount(t, e, "2000", "400000"), settleAccount(t, e, "2000", "510000")
	bank, ap := settleAccount(t, e, "2000", "100000"), settleAccount(t, e, "2000", "211000")
	determine(t, e, e.de, "cash_discount", cash)
	determine(t, e, e.de, "fx_gain", gain)
	eur := settlePost(t, e, e.de, "2000", "EUR", 10000, "INV-D1")
	usd := settlePost(t, e, e.de, "2000", "USD", 10000, "INV-D2") // 90.00 EUR at 0.90
	settle := func(currency, day string, discounts []money.Minor, items ...ledger.ItemRef) (ledger.Entry, error) {
		var entry ledger.Entry
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			in := settlementOf(t, e, e.de, "2000", currency, day, items...)
			in.Discounts = discounts
			entry, err = postings().Settle(ctx, in)
			return err
		})
		return entry, err
	}
	for name, c := range map[string]struct {
		currency, day string
		item          ledger.ItemRef
		discount      money.Minor
		want          map[uuid.UUID]string
	}{
		"in the local currency": {"EUR", "2026-03-10", eur, 200, map[uuid.UUID]string{ap: "10000/10000", bank: "-9800/-9800", cash: "-200/-200"}},
		// 97.00 USD paid at 0.80 is 77.60 EUR, the 3.00 USD discount 2.40
		// EUR; of the 90.00 EUR booked, 10.00 EUR is a gain.
		"in a foreign currency at another rate": {"USD", "2026-04-15", usd, 300,
			map[uuid.UUID]string{ap: "10000/9000", bank: "-9700/-7760", cash: "-300/-240", gain: "0/-1000"}},
	} {
		t.Run(name, func(t *testing.T) {
			entry, err := settle(c.currency, c.day, []money.Minor{c.discount}, c.item)
			if err != nil {
				t.Fatal(err)
			}
			if got := lineAmounts(entry); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("lines: got %v, want %v", got, c.want)
			}
		})
	}
	other := settlePost(t, e, e.de, "2000", "EUR", 10000, "INV-D3")
	for name, c := range map[string]struct {
		discounts []money.Minor
		code      string
	}{
		"a discount of the other sign":   {[]money.Minor{-100}, "ledger.invalid_discount"},
		"a discount of the whole amount": {[]money.Minor{10000}, "ledger.invalid_discount"},
		"discounts not matched to items": {[]money.Minor{100, 100}, "ledger.invalid_discount"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := settle("EUR", "2026-03-10", c.discounts, other)
			pgtest.WantErr(t, err, c.code)
		})
	}
	// Amounts at the edge of an amount's range: discounts each in range
	// whose total is not, and nets whose total is not.
	big := []ledger.ItemRef{settlePost(t, e, e.de, "2000", "EUR", 6_000_000_000_000_000_000, "INV-B1"),
		settlePost(t, e, e.de, "2000", "EUR", 6_000_000_000_000_000_000, "INV-B2"), settlePost(t, e, e.de, "2000", "EUR", -9_000_000_000_000_000_000, "INV-B3")}
	for name, discounts := range map[string][]money.Minor{
		"discounts adding up beyond an amount": {5_900_000_000_000_000_000, 5_900_000_000_000_000_000, -1},
		"payments adding up beyond an amount":  {1, 1, -8_999_999_999_999_999_999},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := settle("EUR", "2026-03-10", discounts, big...)
			pgtest.WantErr(t, err, "ledger.amount_too_large")
		})
	}
	// At rates that take local amounts out of range: a discount whose
	// local amount is not in range, and a bank line and discount whose
	// local amounts each are, but not their total.
	settleRate(t, e, "USD", "2026-07-01", "950000")
	settleRate(t, e, "USD", "2026-08-01", "1000000")
	for name, c := range map[string]struct {
		day      string
		discount money.Minor
	}{
		"a discount out of range in local currency":        {"2026-08-15", 9_999_999_999_999},
		"bank and discount out of range in local currency": {"2026-07-15", 4_000_000_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			item := settlePost(t, e, e.de, "2000", "USD", 10_000_000_000_000, "INV-"+c.day)
			_, err := settle("USD", c.day, []money.Minor{c.discount}, item)
			pgtest.WantErr(t, err, "ledger.amount_too_large")
		})
	}
	t.Run("no cash discount account", func(t *testing.T) {
		usItem := settlePost(t, e, e.us, "1000", "USD", 10000, "INV-D4")
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			in := settlementOf(t, e, e.us, "1000", "USD", "2026-03-10", usItem)
			in.Discounts = []money.Minor{100}
			_, err := postings().Settle(ctx, in)
			return err
		})
		pgtest.WantErr(t, err, "ledger.no_cash_discount_account")
	})
	apitest.EveryFailure(t, e.ten, e.ten.Human, "settle with a discount", func(ctx context.Context) error {
		in := settlementOf(t, e, e.de, "2000", "EUR", "2026-03-10", other)
		in.Discounts = []money.Minor{100}
		_, err := postings().Settle(ctx, in)
		return err
	})
}
