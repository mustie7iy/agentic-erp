package ledger_test

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// recorded is a row of ledger.price_difference_item.
type recorded struct {
	LineNo int32
	Order  uuid.UUID
	Item   int32
	TC, LC int64
}

// recordedOf are the entry's recorded price difference lines, by line.
func recordedOf(t *testing.T, entry ledger.Entry) []recorded {
	t.Helper()
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT line_no, purchase_order_id, po_item_no, tax_borne_tc, tax_borne_lc
		FROM ledger.price_difference_item WHERE fiscal_year = $1 AND journal_entry_id = $2 ORDER BY line_no`, entry.FiscalYear, entry.ID)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[recorded])
	if err != nil {
		t.Fatalf("read the recorded price differences: %v", err)
	}
	return out
}

// lineOn is the number of the entry's nth line on the account.
func lineOn(t *testing.T, entry ledger.Entry, account uuid.UUID, nth int) int32 {
	t.Helper()
	for _, l := range entry.Lines {
		if l.AccountID != account {
			continue
		}
		if nth == 0 {
			return l.LineNo
		}
		nth--
	}
	t.Fatalf("no such line on account %s: %+v", account, entry.Lines)
	return 0
}

// TestPriceDifferencesRecorded: an invoiced order item's price difference
// line is recorded with its order item and the tax it bears, in the
// entry's and in local currency, and nothing that posts changes with the
// tax (B10, po:2026-11-10).
func TestPriceDifferencesRecorded(t *testing.T) {
	g := grirSetup(t)
	settleRate(t, g.env, "USD", "2026-03-20", "0.95")
	p := price{100, 1}
	t.Run("the tax borne moves no amount", func(t *testing.T) {
		without := g.post(t, g.invoicePosting("2026-03-20", "USD", uuid.New(), "50", p, 5600))
		with := g.invoicePosting("2026-03-20", "USD", uuid.New(), "50", p, 5600)
		with.GRIR[0].TaxBorne = 500
		entry := g.post(t, with)
		for _, account := range []uuid.UUID{g.grir, g.diff, g.ap, g.fxGain, g.fxLoss} {
			if got, want := on(entry, account), on(without, account); !reflect.DeepEqual(got, want) {
				t.Errorf("lines on %s: got %v, want %v as without the tax", account, got, want)
			}
		}
		if got, want := len(entry.Lines), len(without.Lines); got != want {
			t.Errorf("lines: got %d, want %d", got, want)
		}
	})
	t.Run("an invoice in a foreign currency", func(t *testing.T) {
		order := uuid.New()
		in := g.invoicePosting("2026-03-20", "USD", order, "50", p, 5600)
		in.GRIR[0].TaxBorne = 500
		entry := g.post(t, in)
		if got, want := one(t, entry, g.diff, "price difference"), [3]money.Minor{600, 570, 570}; got != want {
			t.Fatalf("price difference line: got %v, want %v", got, want)
		}
		want := []recorded{{LineNo: lineOn(t, entry, g.diff, 0), Order: order, Item: 10, TC: 500, LC: 475}}
		if got := recordedOf(t, entry); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded: got %+v, want %+v", got, want)
		}
	})
	t.Run("several items, one without a difference", func(t *testing.T) {
		first, second, third := uuid.New(), uuid.New(), uuid.New()
		in := g.invoicePosting("2026-03-20", "EUR", first, "1", p, 130)
		in.Lines[0].Amount = -130 - 100 - 90
		in.GRIR[0].TaxBorne = 30
		exact, less := money.Minor(100), money.Minor(90)
		in.GRIR = append(in.GRIR,
			ledger.GRIRItem{PurchaseOrderID: second, POItemNo: 20, Quantity: "1", Unit: "EA", UnitPrice: 100, PriceUnit: 1, Invoiced: &exact},
			ledger.GRIRItem{PurchaseOrderID: third, POItemNo: 30, Quantity: "1", Unit: "EA", UnitPrice: 100, PriceUnit: 1, Invoiced: &less})
		entry := g.post(t, in)
		want := []recorded{{LineNo: lineOn(t, entry, g.diff, 0), Order: first, Item: 10, TC: 30, LC: 30},
			{LineNo: lineOn(t, entry, g.diff, 1), Order: third, Item: 30, TC: 0, LC: 0}}
		if got := recordedOf(t, entry); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded: got %+v, want %+v", got, want)
		}
	})
	t.Run("a credit memo", func(t *testing.T) {
		order := uuid.New()
		g.post(t, g.invoicePosting("2026-03-02", "EUR", order, "2", p, 200))
		in := g.invoicePosting("2026-03-03", "EUR", order, "-1", p, -112)
		in.GRIR[0].TaxBorne = -5
		entry := g.post(t, in)
		if got, want := one(t, entry, g.diff, "price difference"), [3]money.Minor{-12, -12, -12}; got != want {
			t.Fatalf("price difference line: got %v, want %v", got, want)
		}
		want := []recorded{{LineNo: lineOn(t, entry, g.diff, 0), Order: order, Item: 10, TC: -5, LC: -5}}
		if got := recordedOf(t, entry); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded: got %+v, want %+v", got, want)
		}
	})
	t.Run("nothing without a price difference line", func(t *testing.T) {
		order := uuid.New()
		for name, in := range map[string]ledger.Posting{
			"an invoice at the order price": g.invoicePosting("2026-03-02", "EUR", order, "1", p, 100),
			"a receipt":                     g.receiptPosting("2026-03-03", "EUR", order, "-1", p),
		} {
			if got := recordedOf(t, g.post(t, in)); len(got) != 0 {
				t.Errorf("%s: got %+v recorded, want none", name, got)
			}
		}
	})
	t.Run("a tax too large for the local currency", func(t *testing.T) {
		settleRate(t, g.env, "USD", "2026-03-25", "1.5")
		in := g.invoicePosting("2026-03-25", "USD", uuid.New(), "1", p, 150)
		in.GRIR[0].TaxBorne = math.MaxInt64
		err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, in)
			return err
		})
		if got := problem.From(err).Code; got != "ledger.amount_too_large" {
			t.Errorf("got %s (%v), want ledger.amount_too_large", got, err)
		}
	})
}

// TestPriceDifferencesReversed: a reversal's price difference lines are
// recorded as their originals' with the tax negated, line by line, and a
// reversal of an entry without recorded lines records none.
func TestPriceDifferencesReversed(t *testing.T) {
	g := grirSetup(t)
	reverse := func(t *testing.T, entry ledger.Entry) ledger.Entry {
		t.Helper()
		var rev ledger.Entry
		if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) (err error) {
			rev, err = postings().Reverse(ctx, entry.ID, nil, nil)
			return err
		}); err != nil {
			t.Fatalf("reverse: %v", err)
		}
		return rev
	}
	p := price{100, 1}
	t.Run("recorded", func(t *testing.T) {
		order := uuid.New()
		in := g.invoicePosting("2026-03-02", "EUR", order, "1", p, 140)
		in.GRIR[0].TaxBorne = 25
		entry := g.post(t, in)
		rev := reverse(t, entry)
		line := lineOn(t, entry, g.diff, 0)
		if got := lineOn(t, rev, g.diff, 0); got != line {
			t.Fatalf("the reversal's price difference line: got line %d, want %d as the original's", got, line)
		}
		want := []recorded{{LineNo: line, Order: order, Item: 10, TC: -25, LC: -25}}
		if got := recordedOf(t, rev); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded for the reversal: got %+v, want %+v", got, want)
		}
		if got := recordedOf(t, entry); len(got) != 1 || got[0].TC != 25 {
			t.Errorf("recorded for the original: got %+v, want its tax of 25 unchanged", got)
		}
	})
	t.Run("not recorded", func(t *testing.T) {
		entry := g.post(t, g.invoicePosting("2026-03-02", "EUR", uuid.New(), "1", p, 100))
		if got := recordedOf(t, reverse(t, entry)); len(got) != 0 {
			t.Errorf("got %+v recorded, want none", got)
		}
	})
}
