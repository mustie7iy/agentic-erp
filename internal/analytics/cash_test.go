package analytics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// fakeCash answers the cash actuals' shares, the forecast's lines and the
// accuracy's cells of any legal entity, or fails.
type fakeCash struct {
	shares   []CashShare
	forecast []ForecastShare
	cells    []AccuracyCell
	err      error
	asked    []uuid.UUID
}

func (f *fakeCash) ActualShares(_ context.Context, le uuid.UUID) ([]CashShare, error) {
	f.asked = append(f.asked, le)
	return f.shares, f.err
}

func (f *fakeCash) ForecastShares(_ context.Context, le uuid.UUID) ([]ForecastShare, error) {
	f.asked = append(f.asked, le)
	return f.forecast, f.err
}

func (f *fakeCash) AccuracyCells(_ context.Context, le uuid.UUID) ([]AccuracyCell, error) {
	f.asked = append(f.asked, le)
	return f.cells, f.err
}

// cashPages are the API's pages of a legal entity with a uuid, and
// nothing else: the cash actuals alone read no partners, accounts or
// journal.
func cashPages(le uuid.UUID) map[string][]string {
	return map[string][]string{"/v1/legal-entities": {items(map[string]any{"id": le.String(), "code": "3000", "local_currency": "CAD"})}}
}

// TestSourceReadsCashActuals: a source of the cash actuals alone reads the
// legal entities and each one's shares through the cash context, one fact
// per share with its dimensions and value date's fiscal periods; it reads
// no business partners.
func TestSourceReadsCashActuals(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	day := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	cash := &fakeCash{shares: []CashShare{{StatementID: uuid.New(), LineNo: 2, ValueDate: day, HouseBankCode: "MAIN", Currency: "CAD", Exponent: 2,
		Match: "posting_rule", Item: "BANK_CHARGES", ItemName: "Bank charges", Amount: -1500}}}
	f, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_actuals"}, Cash: cash}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	facts := f.Tables["cash_actuals"]
	if len(facts) != 1 || len(cash.asked) != 1 || cash.asked[0] != le {
		t.Fatalf("facts: got %+v asking %v, want one of legal entity %s", facts, cash.asked, le)
	}
	want := map[string]string{"company": "3000", "house_bank": "MAIN", "match": "posting_rule", "liquidity_item": "BANK_CHARGES",
		"liquidity_item_name": "Bank charges", "value_date": "2026-04-02", "fiscal_year": "2026", "fiscal_quarter": "2", "fiscal_month": "4"}
	for dim, v := range want {
		if got := facts[0].Dims[dim]; got != v {
			t.Errorf("%s: got %q, want %q", dim, got, v)
		}
	}
	if fc := facts[0]; fc.Amount != -1500 || fc.Currency != "CAD" || f.Exponents["CAD"] != 2 || f.DataThrough != "2026-04-02" {
		t.Errorf("amount: got %d %s exponent %d through %s, want -1500 CAD 2 through 2026-04-02", fc.Amount, fc.Currency, f.Exponents["CAD"], f.DataThrough)
	}
	if _, posting := facts[0].Dims["posting_date"]; posting {
		t.Error("posting_date: got one, want the value date alone")
	}
	t.Run("without the port", func(t *testing.T) {
		f, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_actuals"}}.Facts(context.Background(), m, time.Now())
		if err != nil || len(f.Tables["cash_actuals"]) != 0 {
			t.Errorf("got %+v, %v; want no cash actuals", f, err)
		}
	})
}

// TestSourceCashErrors: a legal entity whose id is not one, the cash
// context failing, or a currency of two exponents fails the facts.
func TestSourceCashErrors(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	share := CashShare{StatementID: uuid.New(), LineNo: 1, ValueDate: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), Currency: "CAD", Exponent: 2, Amount: 1}
	other := share
	other.Exponent = 3
	for name, c := range map[string]struct {
		pages map[string][]string
		cash  *fakeCash
		want  string
	}{
		"legal entity id": {map[string][]string{"/v1/legal-entities": {items(map[string]any{"id": "le", "code": "3000"})}}, &fakeCash{}, `legal entity "le"`},
		"cash context":    {cashPages(le), &fakeCash{err: errors.New("cash failed")}, "cash actuals of 3000: cash failed"},
		"two exponents":   {cashPages(le), &fakeCash{shares: []CashShare{share, other}}, "CAD has exponents 2 and 3"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Source{API: fakeAPI(t, c.pages), Datasets: []string{"cash_actuals"}, Cash: c.cash}.Facts(context.Background(), m, time.Now())
			pgtest.CheckErr(t, err, c.want)
		})
	}
}

// TestSourceReadsCashForecast: a source of the forecast alone reads each
// legal entity's snapshot lines through the cash context, one fact per
// line with its dimensions, lag and value date's fiscal periods; it reads
// no actuals and no business partners.
func TestSourceReadsCashForecast(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	day := time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)
	cash := &fakeCash{shares: []CashShare{{Currency: "CAD", Exponent: 2}}, forecast: []ForecastShare{{SnapshotID: uuid.New(), LineNo: 3,
		SnapshotDate: day.AddDate(0, 0, -7), ValueDate: day, LagDays: 7, Currency: "CAD", Exponent: 2, Source: "payable_blocked", Certainty: "expected",
		Blocked: true, Item: "OP_SUPPLIERS", ItemName: "Supplier payments", Amount: -300}}}
	f, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_snapshots"}, Cash: cash}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	facts := f.Tables["cash_snapshots"]
	if len(facts) != 1 || len(f.Tables["cash_actuals"]) != 0 || len(cash.asked) != 1 {
		t.Fatalf("facts: got %+v and %d actuals asking %v, want one forecast line asked once", facts, len(f.Tables["cash_actuals"]), cash.asked)
	}
	want := map[string]string{"company": "3000", "currency": "CAD", "lag_days": "7", "house_bank": "", "flow_source": "payable_blocked", "certainty": "expected",
		"blocked": "true", "liquidity_item": "OP_SUPPLIERS", "liquidity_item_name": "Supplier payments", "value_date": "2026-04-09", "fiscal_month": "4"}
	for dim, v := range want {
		if got := facts[0].Dims[dim]; got != v {
			t.Errorf("%s: got %q, want %q", dim, got, v)
		}
	}
	if fc := facts[0]; fc.Amount != -300 || fc.Currency != "CAD" || f.Exponents["CAD"] != 2 || f.DataThrough != "2026-04-09" {
		t.Errorf("amount: got %d %s exponent %d through %s, want -300 CAD 2 through 2026-04-09", fc.Amount, fc.Currency, f.Exponents["CAD"], f.DataThrough)
	}
}

// TestSourceForecastErrors: the cash context failing on the forecast, or
// a currency of two exponents, fails the facts.
func TestSourceForecastErrors(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	line := ForecastShare{SnapshotID: uuid.New(), LineNo: 1, ValueDate: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), Currency: "CAD", Exponent: 2, Amount: 1}
	other := line
	other.Exponent = 3
	for name, c := range map[string]struct {
		cash *fakeCash
		want string
	}{
		"cash context":  {&fakeCash{err: errors.New("cash failed")}, "cash forecast of 3000: cash failed"},
		"two exponents": {&fakeCash{forecast: []ForecastShare{line, other}}, "cash forecast of 3000: CAD has exponents 2 and 3"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_snapshots"}, Cash: c.cash}.Facts(context.Background(), m, time.Now())
			pgtest.CheckErr(t, err, c.want)
		})
	}
}

// TestSourceReadsCashAccuracy: a source of the accuracy alone reads each
// legal entity's cells through the cash context, three facts per cell (the
// variance, signed, and the sizes of the variance and the forecast) with
// its dimensions, lag and value date's fiscal periods; it reads no actuals
// or forecast lines and no business partners.
func TestSourceReadsCashAccuracy(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	day := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
	cash := &fakeCash{shares: []CashShare{{Currency: "CAD", Exponent: 2}}, forecast: []ForecastShare{{Currency: "CAD", Exponent: 2}},
		cells: []AccuracyCell{{ID: uuid.New(), ValueDate: day, LagDays: 7, Currency: "CAD", Exponent: 2, Item: "OP_SUPPLIERS", ItemName: "Supplier payments",
			Actual: 300, Forecast: -200, Variance: 500}}}
	f, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_accuracy"}, Cash: cash}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	facts := f.Tables["cash_accuracy"]
	if len(facts) != 3 || len(f.Tables["cash_actuals"]) != 0 || len(f.Tables["cash_snapshots"]) != 0 || len(cash.asked) != 1 {
		t.Fatalf("facts: got %+v and %d actuals asking %v, want three of one cell asked once", facts, len(f.Tables["cash_actuals"]), cash.asked)
	}
	for i, want := range []struct {
		figure string
		amount int64
	}{{"variance", 500}, {"absolute_variance", 500}, {"absolute_forecast", 200}} {
		fc := facts[i]
		if fc.Dims["figure"] != want.figure || int64(fc.Amount) != want.amount || fc.Currency != "CAD" || fc.ID != cash.cells[0].ID.String() {
			t.Errorf("fact %d: got %s %d %s of %s, want %s %d CAD of the cell", i, fc.Dims["figure"], fc.Amount, fc.Currency, fc.ID, want.figure, want.amount)
		}
	}
	want := map[string]string{"company": "3000", "currency": "CAD", "lag_days": "7", "liquidity_item": "OP_SUPPLIERS", "liquidity_item_name": "Supplier payments",
		"value_date": "2026-04-16", "fiscal_month": "4"}
	for dim, v := range want {
		if got := facts[0].Dims[dim]; got != v {
			t.Errorf("%s: got %q, want %q", dim, got, v)
		}
	}
	if f.Exponents["CAD"] != 2 || f.DataThrough != "2026-04-16" {
		t.Errorf("exponent %d through %s, want 2 through 2026-04-16", f.Exponents["CAD"], f.DataThrough)
	}
}

// TestSourceAccuracyErrors: the cash context failing on the accuracy, or a
// currency of two exponents, fails the facts.
func TestSourceAccuracyErrors(t *testing.T) {
	m, _ := DefaultModel()
	le := uuid.New()
	cell := AccuracyCell{ID: uuid.New(), ValueDate: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), Currency: "CAD", Exponent: 2, Variance: 1}
	other := cell
	other.Exponent = 3
	for name, c := range map[string]struct {
		cash *fakeCash
		want string
	}{
		"cash context":  {&fakeCash{err: errors.New("cash failed")}, "forecast accuracy of 3000: cash failed"},
		"two exponents": {&fakeCash{cells: []AccuracyCell{cell, other}}, "forecast accuracy of 3000: CAD has exponents 2 and 3"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_accuracy"}, Cash: c.cash}.Facts(context.Background(), m, time.Now())
			pgtest.CheckErr(t, err, c.want)
		})
	}
}
