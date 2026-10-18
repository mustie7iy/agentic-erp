package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRecordPrices: an operator records published prices per million
// tokens in the currency's major units; they are kept in millionths of the
// minor unit with who recorded them, and a price recorded once stands
// (ADR-0036).
func TestRecordPrices(t *testing.T) {
	ctx := context.Background()
	admin := pgtest.Admin(t)
	in := func(prices ...agents.Price) error {
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) // committed below when the prices are recorded
		if err := agents.RecordPrices(ctx, tx, "ops|alice", prices); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	model := "claude-test-" + uuid.NewString()[:8] // prices are global; each run records a model of its own
	price := agents.Price{Model: model, EffectiveFrom: "2026-11-01", Currency: "USD", Input: "4.00", Output: "20", CacheWrite: "5.00", CacheRead: "0.125"}
	if err := in(price); err != nil {
		t.Fatal(err)
	}
	var input, read int64
	var by string
	pgtest.Scan(t, `SELECT input, cache_read, recorded_by FROM ref.model_price WHERE model = $1`, []any{model}, &input, &read, &by)
	if input != 400_000_000 || read != 12_500_000 || by != "ops|alice" {
		t.Errorf("recorded: got %d, %d by %s, want 400000000, 12500000 by ops|alice", input, read, by)
	}
	for _, c := range []struct {
		name string
		edit func(p *agents.Price)
		want string
	}{
		{"recorded already", func(*agents.Price) {}, "already recorded"},
		{"another currency", func(p *agents.Price) { p.EffectiveFrom, p.Currency = "2026-12-01", "EUR" }, "prices are in USD"},
		{"not a date", func(p *agents.Price) { p.EffectiveFrom = "soon" }, "effective_from"},
		{"too fine a price", func(p *agents.Price) { p.EffectiveFrom, p.Input = "2026-12-01", "0.000000001" }, "input"},
		{"a negative price", func(p *agents.Price) { p.EffectiveFrom, p.Output = "2026-12-01", "-1" }, "output"},
		{"not a model id", func(p *agents.Price) { p.Model = "Claude Test" }, "model"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := price
			c.edit(&p)
			if err := in(p); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	t.Run("the database failing", func(t *testing.T) {
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) // nothing is kept
		p := price
		p.EffectiveFrom = "2027-01-01"
		for at := 1; at <= 2; at++ {
			if err := agents.RecordPrices(ctx, &pgtest.FailingTx{Tx: tx, FailAt: at}, "ops", []agents.Price{p}); err == nil {
				t.Errorf("statement %d failing: got recorded, want the failure", at)
			}
		}
	})
}
