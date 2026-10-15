package ledger_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestRatesPort converts document amounts into a company's currency the
// way posting does: the average rate valid on the date, inverted when only
// the other direction is maintained; a missing rate or an amount out of
// range is refused, never compared unconverted (F12).
func TestRatesPort(t *testing.T) {
	e := setup(t)
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	e.rate(t, "EUR", "JPY", "2026-01-01", "160")
	e.rate(t, "EUR", "CHF", "2026-01-01", "0.96") // only EUR into CHF: CHF converts at 1/0.96
	rates := ledger.Rates{LegalEntities: tenancy.Directory{}}
	toCompany := func(le uuid.UUID, currency, day string, amount money.Minor) (money.Minor, int, error) {
		var v money.Minor
		var exp int
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			v, exp, err = rates.ToCompany(ctx, le, currency, date(day), amount)
			return err
		})
		return v, exp, err
	}
	for _, c := range []struct {
		name, currency string
		amount, want   money.Minor
	}{
		{"same currency", "EUR", 12345, 12345},
		{"rate", "USD", 1111111, 1000000},                  // 11,111.11 USD at 0.9: 9,999.999 EUR, rounded
		{"inverted rate", "JPY", 1000, 625},                // 1,000 JPY at 1/160: 6.25 EUR
		{"inverted repeating rate", "CHF", 100000, 104167}, // 1,000.00 CHF at 1/0.96 = 1.041666666667: 1,041.67 EUR
	} {
		t.Run(c.name, func(t *testing.T) {
			got, exp, err := toCompany(e.de, c.currency, "2026-03-10", c.amount)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("amount: got %d, want %d", got, c.want)
			}
			if exp != 2 {
				t.Errorf("exponent: got %d, want 2", exp)
			}
		})
	}
	refusals := []struct {
		name, currency, day, code string
		amount                    money.Minor
	}{
		{"before the rate", "USD", "2025-12-31", "ledger.no_exchange_rate", 100},
		{"no rate", "GBP", "2026-03-10", "ledger.no_exchange_rate", 100},
		{"out of range", "EUR", "2026-03-10", "ledger.amount_too_large", math.MaxInt64}, // in USD company 1000, at 1/0.9
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			le := e.de
			if c.code == "ledger.amount_too_large" {
				le = e.us
			}
			_, _, err := toCompany(le, c.currency, c.day, c.amount)
			if got := problem.From(err).Code; got != c.code {
				t.Errorf("problem: got %s (%v), want %s", got, err, c.code)
			}
		})
	}
	// Callers pass the legal entity of a stored document, so this is a
	// refusal of the request rather than a server error should it happen.
	if _, _, err := toCompany(uuid.New(), "EUR", "2026-03-10", 1); problem.From(err).Code != "request.reference_not_found" {
		t.Errorf("unknown legal entity: got %v, want request.reference_not_found", err)
	}
	t.Run("inverse rate", func(t *testing.T) {
		var c ledger.Conversion
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			c, err = rates.Conversion(ctx, "CHF", "EUR", date("2026-03-10"))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		// The inverse of the recorded rate, rounded to 12 places as posting
		// rounds it; never the recorded 0.96 read the wrong way round.
		if c.Rate != "1.041666666667" {
			t.Errorf("rate: got %s, want 1.041666666667", c.Rate)
		}
	})
	if _, err := rates.Conversion(context.Background(), "USD", "EUR", date("2026-03-10")); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "to company", func(ctx context.Context) error {
		_, _, err := rates.ToCompany(ctx, e.de, "USD", date("2026-03-10"), 100)
		return err
	})
	// An invalid rate cannot come from the database; the conversion still
	// reports it rather than returning zero.
	if _, err := (ledger.Conversion{From: "USD", To: "EUR", Rate: "x", FromExp: 2, ToExp: 2}).Convert(1); err == nil || errors.Is(err, money.ErrOverflow) {
		t.Errorf("invalid rate: got %v, want an error", err)
	}
}
