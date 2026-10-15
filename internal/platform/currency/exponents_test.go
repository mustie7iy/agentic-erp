package currency_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The offline exponents read from the migration seed are exactly what a
// migrated database holds in ref.currency, so validating offline and
// against the database cannot disagree.
func TestSeededMatchesTheDatabase(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only; nothing to keep
	loaded, err := currency.Load(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	seeded := currency.Seeded()
	if !maps.Equal(seeded, loaded) {
		t.Errorf("seeded exponents: got %v, want ref.currency's %v", seeded, loaded)
	}
	for code, want := range map[string]int{"BHD": 3, "JPY": 0, "EUR": 2} {
		if got, ok := loaded[code]; !ok || got != want {
			t.Errorf("exponent of %s: got %d (%v), want %d", code, got, ok, want)
		}
	}
	t.Run("a failed read", func(t *testing.T) {
		_, err := currency.Load(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1})
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
	t.Run("a copy each time", func(t *testing.T) {
		currency.Seeded()["BHD"] = 9
		if got := currency.Seeded()["BHD"]; got != 3 {
			t.Errorf("after changing a copy: got %d, want 3", got)
		}
	})
}
