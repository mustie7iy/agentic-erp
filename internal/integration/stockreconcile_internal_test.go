package integration

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
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
	var keys []string
	for _, d := range rc.rec.Diffs {
		keys = append(keys, d.Key)
		if strings.HasPrefix(d.Key, "stock account") {
			t.Errorf("differences: got %q, want no stock account's", d.Key)
		}
	}
	if !slices.Contains(keys, "stock P1/M-1") {
		t.Errorf("differences: got %v, want the valuation's, stock P1/M-1", keys)
	}
}

// TestStockControlsOfOneCompany: a company's stock controls leave out
// SAP's valuations of other company codes and the platform's of other
// legal entities, and compare a stock account a taken-over valuation's
// class posts to even when SAP has no valuation of that class left.
func TestStockControlsOfOneCompany(t *testing.T) {
	le := uuid.New()
	other := inventory.HeldStock{Valuation: inventory.Valuation{LegalEntityID: uuid.New(), ValuationClass: "3000", Quantity: "1", Value: 5}}
	gone := inventory.HeldStock{Valuation: inventory.Valuation{LegalEntityID: le, ValuationClass: "3100", Quantity: "2", Value: 20}}
	rc := &reconciler{e: &Endpoints{Reports: totalsReports{totals: ledger.Totals{ByAccount: map[string]money.Minor{"150000": 20}}}},
		t: target{classes: map[string]map[string]config.InventoryAccounts{"2000": {"3100": {Stock: "150000"}}}},
		stock: &stockStates{held: map[string]inventory.HeldStock{"P9/M-9": other, "P1/M-2": gone},
			stocks: []sapStock{{matnr: "M-1", werks: "P1", bwkey: "P1", company: "1000", class: "3000", quantity: money.ParseRat("1"), value: 5}}},
		materials: &materialStates{}}
	format := func(m money.Minor) string { return strconv.FormatInt(int64(m), 10) }
	if err := rc.stockControls(context.Background(), companyScope{cc: "2000", le: le, format: format}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, d := range rc.rec.Diffs {
		keys = append(keys, d.Key)
	}
	slices.Sort(keys)
	if got, want := strings.Join(keys, ", "), "stock P1/M-2, stock account 150000"; got != want {
		t.Errorf("differences: got %s, want %s", got, want)
	}
}

// TestHeldStockTextLeavesOutEmptyLocations: a storage location the
// material was moved at that holds no stock now is left out, as SAP's
// MARD rows without stock are.
func TestHeldStockTextLeavesOutEmptyLocations(t *testing.T) {
	empty, full := uuid.New(), uuid.New()
	h := inventory.HeldStock{Valuation: inventory.Valuation{ValuationClass: "3000", PriceControl: "S", Quantity: "4", Value: 40},
		Locations: map[uuid.UUID]string{empty: "0", full: "4"}}
	ms := &materialStates{units: map[uuid.UUID]tenancy.Unit{full: {Code: "S1"}}}
	if got, want := heldStockText(h, ms, func(m money.Minor) string { return strconv.FormatInt(int64(m), 10) }), "class 3000, S, stock 4 worth 40 (S1 4)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
