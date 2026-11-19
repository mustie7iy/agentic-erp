package integration

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// offReports answers the take-over's entry totals.
type offReports struct {
	Reports
	off ledger.Totals
	err error
}

func (r offReports) EntryTotals(context.Context, uuid.UUID, int16, []uuid.UUID) (ledger.Totals, error) {
	return r.off, r.err
}

// TestWithoutTakeOverOutOfRange: totals that the take-over's cannot be
// taken off within an amount's range, or that cannot be read, fail the
// trial balance rather than wrap.
func TestWithoutTakeOverOutOfRange(t *testing.T) {
	read := errors.New("read")
	cases := []struct {
		name         string
		totals, off  ledger.Totals
		err, wantErr error
	}{
		{"an account", ledger.Totals{ByAccount: map[string]money.Minor{"150000": math.MinInt64}}, ledger.Totals{ByAccount: map[string]money.Minor{"150000": 1}},
			nil, money.ErrOverflow},
		{"the debits", ledger.Totals{ByAccount: map[string]money.Minor{}, Debits: math.MinInt64}, ledger.Totals{Debits: 1}, nil, money.ErrOverflow},
		{"the entries", ledger.Totals{}, ledger.Totals{}, read, read},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := &reconciler{e: &Endpoints{Reports: offReports{off: c.off, err: c.err}}, stock: &stockStates{entries: []uuid.UUID{uuid.New()}}}
			if _, err := rc.withoutTakeOver(context.Background(), companyScope{cc: "2000"}, c.totals); !errors.Is(err, c.wantErr) {
				t.Errorf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// totalsReports answers the run's fiscal year's totals.
type totalsReports struct {
	Reports
	totals ledger.Totals
}

func (r totalsReports) Totals(context.Context, uuid.UUID, int16) (ledger.Totals, error) {
	return r.totals, nil
}

// TestStockControlsOfAClassNoLongerConfigured: a valuation taken over in a
// class the active configuration no longer has is a difference of its
// own, and adds no stock account without a number to the account check.
func TestStockControlsOfAClassNoLongerConfigured(t *testing.T) {
	le := uuid.New()
	held := inventory.HeldStock{Valuation: inventory.Valuation{LegalEntityID: le, ValuationClass: "3000", PriceControl: "moving_average", Quantity: "1", Value: 100}}
	rc := &reconciler{e: &Endpoints{Reports: totalsReports{totals: ledger.Totals{ByAccount: map[string]money.Minor{}}}},
		t:         target{classes: map[string]map[string]config.InventoryAccounts{"2000": {}}},
		stock:     &stockStates{held: map[string]inventory.HeldStock{"P1/M-1": held}},
		materials: &materialStates{}}
	format := func(m money.Minor) string { return strconv.FormatInt(int64(m), 10) }
	if err := rc.stockControls(context.Background(), companyScope{cc: "2000", le: le, format: format}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rc.rec.Checks {
		if c.Name == "Stock accounts: accounts with differences" && c.Platform != "0" {
			t.Errorf("stock accounts with differences: got %s, want 0", c.Platform)
		}
	}
	for _, d := range rc.rec.Diffs {
		if d.Key == "stock account " {
			t.Errorf("differences: got a stock account without a number")
		}
	}
	if len(rc.rec.Diffs) == 0 {
		t.Errorf("differences: got none, want the valuation's")
	}
}
