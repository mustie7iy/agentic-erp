package inventory_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// takeOver is a take-over of material at plant P1 under class 3000.
func (e env) takeOver(material, control, price string, items ...inventory.TakeOverItem) inventory.StockTakeOver {
	return inventory.StockTakeOver{Valuation: inventory.ValuationInput{MaterialID: uuid.MustParse(material), PlantID: e.p1, ValuationClass: "3000",
		PriceControl: control, Price: price, Currency: "EUR", PriceUnit: 1}, HeaderText: "Migration run: SAP stock", Items: items}
}

// TestTakeOver (S2p): a take-over values the material in the plant and
// posts its stock as one 561, a moving-priced item at what it is worth
// and a standard-priced one at its price, the header text naming where
// it comes from; without items it only values. TakenOver reads the
// valuation back with its stock by storage location and the entry of the
// document.
func TestTakeOver(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	moving := e.material(t, "Moving bracket", "trading_good", true)
	standard := e.material(t, "Standard bracket", "trading_good", true)
	empty := e.material(t, "Empty bracket", "trading_good", true)
	var got, std, none inventory.TakenOverStock
	var held map[uuid.UUID]inventory.HeldStock
	var entries map[uuid.UUID]uuid.UUID
	err := e.inRequest(t, func(ctx context.Context) error {
		var err error
		if got, err = h.TakeOver(ctx, e.takeOver(moving, inventory.MovingAverage, "2.50",
			inventory.TakeOverItem{StorageLocationID: e.s1, Quantity: "60", Amount: &inventory.Amount{Value: "150.01", Currency: "EUR"}},
			inventory.TakeOverItem{StorageLocationID: e.s2, Quantity: "40", Amount: &inventory.Amount{Value: "100.00", Currency: "EUR"}})); err != nil {
			return err
		}
		if std, err = h.TakeOver(ctx, e.takeOver(standard, inventory.Standard, "10.00", inventory.TakeOverItem{StorageLocationID: e.s1, Quantity: "4"})); err != nil {
			return err
		}
		if none, err = h.TakeOver(ctx, e.takeOver(empty, inventory.MovingAverage, "1.00")); err != nil {
			return err
		}
		held, entries, err = h.TakenOver(ctx, []uuid.UUID{got.Valuation.ID, none.Valuation.ID, uuid.New()}, []uuid.UUID{got.Document.ID, uuid.New()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, got, want string }{
		{"the moving valuation", got.Valuation.Quantity + " " + got.Valuation.Value.Plain(2), "100 250.01"},
		{"the moving document", got.Document.MovementType + " " + *got.Document.HeaderText, "561 Migration run: SAP stock"},
		{"the standard valuation", std.Valuation.Quantity + " " + std.Valuation.Value.Plain(2), "4 40.00"},
		{"the valuation without stock", none.Valuation.Quantity + " " + none.Valuation.Value.Plain(2), "0 0.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	if none.Document != nil {
		t.Errorf("the take-over without stock: got document %v, want none", none.Document.Number)
	}
	if len(held) != 2 {
		t.Errorf("held: got %d valuations, want two", len(held))
	}
	g := held[got.Valuation.ID].Locations
	if g[e.s1] != "60" {
		t.Errorf("held at S1: got %q, want 60", g[e.s1])
	}
	if g[e.s2] != "40" {
		t.Errorf("held at S2: got %q, want 40", g[e.s2])
	}
	if len(entries) != 1 {
		t.Errorf("entries: got %d, want the moving document's alone", len(entries))
	}
	if entries[got.Document.ID] != *got.Document.EntryID {
		t.Errorf("the moving document's entry: got %v, want %v", entries[got.Document.ID], *got.Document.EntryID)
	}
}

// TestTakeOverRefused: an item the material is not kept at refuses the
// take-over, as a goods movement's does, and posts nothing.
func TestTakeOverRefused(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	m := e.material(t, "Refused bracket", "trading_good", true)
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.TakeOver(ctx, e.takeOver(m, inventory.MovingAverage, "1.00", inventory.TakeOverItem{StorageLocationID: e.s3, Quantity: "1"}))
		return err
	})
	if p := problem.From(err); p.Code != "inventory.storage_location_invalid" {
		t.Errorf("got %v, want inventory.storage_location_invalid", err)
	}
}

// TestTakeOverDatabaseFailures: every database error of a take-over and
// of its read is answered as an error, none outside a unit of work.
func TestTakeOverDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	var taken inventory.TakenOverStock
	err := e.inRequest(t, func(ctx context.Context) error {
		var err error
		taken, err = h.TakeOver(ctx, e.takeOver(e.material(t, "Read bracket", "trading_good", true), inventory.MovingAverage, "1.00",
			inventory.TakeOverItem{StorageLocationID: e.s1, Quantity: "1", Amount: &inventory.Amount{Value: "1.00", Currency: "EUR"}}))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(ctx context.Context) error{
		"take over": func(ctx context.Context) error {
			m := e.material(t, "Failing bracket "+uuid.NewString()[:8], "trading_good", true)
			_, err := h.TakeOver(ctx, e.takeOver(m, inventory.MovingAverage, "1.00",
				inventory.TakeOverItem{StorageLocationID: e.s1, Quantity: "1", Amount: &inventory.Amount{Value: "1.00", Currency: "EUR"}}))
			return err
		},
		"taken over": func(ctx context.Context) error {
			_, _, err := h.TakenOver(ctx, []uuid.UUID{taken.Valuation.ID}, []uuid.UUID{taken.Document.ID})
			return err
		},
		"lock numbers": func(ctx context.Context) error { _, err := h.LockNumbers(ctx, e.DE); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}

// TestLockNumbers: LockNumbers takes the company's material_document
// range of its year today, answers that day and draws no number from the
// range of that day's year; a company that is none is refused.
func TestLockNumbers(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	next := func(year int) (n int64) {
		pgtest.Scan(t, `SELECT next_value FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = 'material_document'
			AND fiscal_year = $3`, []any{e.Tenant.ID, e.DE, year}, &n)
		return n
	}
	var day time.Time
	if err := e.inRequest(t, func(ctx context.Context) (err error) { day, err = h.LockNumbers(ctx, e.DE); return err }); err != nil {
		t.Fatalf("lock the range: %v", err)
	}
	if day.IsZero() {
		t.Fatal("the locked day: got none, want the company's date today")
	}
	before := next(day.Year()) // the fixture's fiscal years are calendar years
	if err := e.inRequest(t, func(ctx context.Context) error { _, err := h.LockNumbers(ctx, e.DE); return err }); err != nil {
		t.Fatalf("lock the range again: %v", err)
	}
	if after := next(day.Year()); after != before {
		t.Errorf("next number of %d after the lock: got %d, want %d", day.Year(), after, before)
	}
	if err := e.inRequest(t, func(ctx context.Context) error { _, err := h.LockNumbers(ctx, uuid.New()); return err }); err == nil {
		t.Error("a company that is none: got nil error, want an error")
	}
}

// TestTakeOverOnTheLockedDay (batch 84 integration): a run locks the
// range on the last day of a fiscal year and takes the stock over after
// the company's midnight; given the day LockNumbers answered, the initial
// entry is dated it and numbered from the range of the year it locked.
func TestTakeOverOnTheLockedDay(t *testing.T) {
	e := setup(t)
	at := func(now time.Time) *inventory.Endpoints {
		h := endpoints(t)
		h.Now = func() time.Time { return now }
		return h
	}
	m := e.material(t, "Year-end bracket", "trading_good", true)
	var taken inventory.TakenOverStock
	err := e.inRequest(t, func(ctx context.Context) error {
		day, err := at(time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)).LockNumbers(ctx, e.DE)
		if err != nil {
			return err
		}
		in := e.takeOver(m, inventory.MovingAverage, "1.00",
			inventory.TakeOverItem{StorageLocationID: e.s1, Quantity: "1", Amount: &inventory.Amount{Value: "1.00", Currency: "EUR"}})
		in.Day = day
		taken, err = at(time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)).TakeOver(ctx, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := taken.Document.PostingDate.Format(time.DateOnly); got != "2026-12-31" {
		t.Errorf("the entry's date: got %s, want 2026-12-31", got)
	}
	if got := taken.Document.FiscalYear; got != 2026 {
		t.Errorf("the document's number range year: got %d, want 2026", got)
	}
}
