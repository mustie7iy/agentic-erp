package ledger_test

import (
	"context"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestOpenPayablesByTradingPartner: a migration run's reconciliation reads
// the open supplier items that carry a trading partner, summed per trading
// partner (ADR-0059 As built, the migration leaf): an ordinary item and a
// down payment of company 2000 naming company 1000 count, an item naming
// no trading partner and a cleared one do not.
func TestOpenPayablesByTradingPartner(t *testing.T) {
	e := setup(t)
	downPayments(t, e)
	expense, ap, bank := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000")
	dp, a := settleAccount(t, e, "2000", "159000"), "A"
	revaluationPost(t, e, e.de, "2026-03-02", "EUR", ledger.PostingLine{AccountID: expense, Amount: 4000},
		ledger.PostingLine{AccountID: ap, Amount: -4000, PartnerID: &e.supplier, TradingPartner: &e.us})
	revaluationPost(t, e, e.de, "2026-03-03", "EUR", ledger.PostingLine{AccountID: dp, Amount: 1000, PartnerID: &e.supplier, SpecialGL: &a, TradingPartner: &e.us},
		ledger.PostingLine{AccountID: bank, Amount: -1000})
	revaluationPost(t, e, e.de, "2026-03-04", "EUR", ledger.PostingLine{AccountID: expense, Amount: 700},
		ledger.PostingLine{AccountID: ap, Amount: -700, PartnerID: &e.supplier})
	invoice := revaluationPost(t, e, e.de, "2026-03-05", "EUR", ledger.PostingLine{AccountID: expense, Amount: 250},
		ledger.PostingLine{AccountID: ap, Amount: -250, PartnerID: &e.supplier, TradingPartner: &e.us})
	payment := revaluationPost(t, e, e.de, "2026-03-06", "EUR", ledger.PostingLine{AccountID: bank, Amount: -250},
		ledger.PostingLine{AccountID: ap, Amount: 250, PartnerID: &e.supplier, TradingPartner: &e.us})
	item := func(en ledger.Entry) ledger.ItemRef {
		return ledger.ItemRef{FiscalYear: en.FiscalYear, EntryID: en.ID, LineNo: 2}
	}
	var got, other map[uuid.UUID]money.Minor
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		if _, err = postings().Clear(ctx, e.de, date("2026-03-06"), item(payment), []ledger.ItemRef{item(invoice), item(payment)}); err != nil {
			return err
		}
		if got, err = (ledger.Reports{}).OpenPayablesByTradingPartner(ctx, e.de, 2026); err != nil {
			return err
		}
		other, err = ledger.Reports{}.OpenPayablesByTradingPartner(ctx, e.de, 2025)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := map[uuid.UUID]money.Minor{e.us: -3000}; !maps.Equal(got, want) {
		t.Errorf("open payables by trading partner: got %v, want %v", got, want)
	}
	if len(other) != 0 {
		t.Errorf("open payables by trading partner of another year: got %v, want none", other)
	}
}
