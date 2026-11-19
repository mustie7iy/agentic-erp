package inventory_test

import (
	"context"
	"testing"

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
		"lock numbers": func(ctx context.Context) error { return h.LockNumbers(ctx, e.DE) },
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
// range of its year today and draws no number from it; a company that is
// none is refused.
func TestLockNumbers(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	next := func() (n int64) {
		pgtest.Scan(t, `SELECT next_value FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = 'material_document'
			ORDER BY fiscal_year DESC LIMIT 1`, []any{e.Tenant.ID, e.DE}, &n)
		return n
	}
	before := next()
	if err := e.inRequest(t, func(ctx context.Context) error { return h.LockNumbers(ctx, e.DE) }); err != nil {
		t.Fatalf("lock the range: %v", err)
	}
	if after := next(); after != before {
		t.Errorf("next number after the lock: got %d, want %d", after, before)
	}
	if err := e.inRequest(t, func(ctx context.Context) error { return h.LockNumbers(ctx, uuid.New()) }); err == nil {
		t.Error("a company that is none: got nil error, want an error")
	}
}
