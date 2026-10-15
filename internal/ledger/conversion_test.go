package ledger_test

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ratOf reads a decimal or a fraction a/b.
func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a rational: %s", s)
	}
	return r
}

// TestRatesPort converts document amounts into a company's currency the
// way posting does: the average rate valid on the date, inverted when only
// the other direction is maintained, and unrounded, as the company's
// configured amounts are compared with it (F13); a missing rate is
// refused, never compared unconverted (F12).
func TestRatesPort(t *testing.T) {
	e := setup(t)
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	e.rate(t, "EUR", "JPY", "2026-01-01", "160")
	e.rate(t, "EUR", "CHF", "2026-01-01", "0.96") // only EUR into CHF: CHF converts at 1/0.96
	// Only EUR into KRW and SEK: their inverses, 1/3e12 and 1/3, have no
	// 12-decimal form, and 1/3e12 rounded to 12 decimals is zero (F19).
	e.rate(t, "EUR", "KRW", "2026-01-01", "3000000000000")
	e.rate(t, "EUR", "SEK", "2026-01-01", "3")
	rates := ledger.Rates{LegalEntities: tenancy.Directory{}}
	toCompany := func(le uuid.UUID, currency, day string, amount money.Minor) (*big.Rat, error) {
		var v *big.Rat
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			v, err = rates.ExactToCompany(ctx, le, currency, date(day), amount)
			return err
		})
		return v, err
	}
	for _, c := range []struct {
		name, currency string
		le             uuid.UUID
		amount         money.Minor
		want           string // major units of the company's currency, as big.Rat reads it
	}{
		{"same currency", "EUR", e.de, 12345, "123.45"},
		{"rate", "USD", e.de, 1111111, "9999.999"},                 // 11,111.11 USD at 0.9, not rounded to 10,000.00
		{"inverted rate", "JPY", e.de, 1000, "6.25"},               // 1,000 JPY at 1/160
		{"inverted repeating rate", "CHF", e.de, 100000, "3125/3"}, // 1,000.00 CHF at 1/0.96, not at 1.041666666667
		// Divided by the recorded rate, exactly (F19).
		{"inverse below 12 decimals", "KRW", e.de, 3_000_000_000_000, "1"},                      // 3e12 KRW at 1/3e12, not 0
		{"inverse of 3", "SEK", e.de, 100, "1/3"},                                               // 1.00 SEK / 3
		{"inverse of 3, large", "SEK", e.de, 300_000_000_000_002, "150000000000001/150"},        // 3,000,000,000,000.02 / 3
		{"beyond the range of an amount", "EUR", e.us, math.MaxInt64, "9223372036854775807/90"}, // 92,233,720,368,547,758.07 EUR at 1/0.9, never wrapped
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := toCompany(c.le, c.currency, "2026-03-10", c.amount)
			if err != nil {
				t.Fatal(err)
			}
			want := ratOf(t, c.want)
			if got.Cmp(want) != 0 {
				t.Errorf("amount: got %s, want %s", got.RatString(), want.RatString())
			}
		})
	}
	refusals := []struct {
		name, currency, day string
	}{
		{"before the rate", "USD", "2025-12-31"},
		{"no rate", "GBP", "2026-03-10"},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			_, err := toCompany(e.de, c.currency, c.day, 100)
			if got := problem.From(err).Code; got != "ledger.no_exchange_rate" {
				t.Errorf("problem: got %s (%v), want ledger.no_exchange_rate", got, err)
			}
		})
	}
	// Callers pass the legal entity of a stored document, so this is a
	// refusal of the request rather than a server error should it happen.
	if _, err := toCompany(uuid.New(), "EUR", "2026-03-10", 1); problem.From(err).Code != "request.reference_not_found" {
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
		_, err := rates.ExactToCompany(ctx, e.de, "USD", date("2026-03-10"), 100)
		return err
	})
	// An invalid rate cannot come from the database; the conversion still
	// reports it rather than returning zero.
	if _, err := (ledger.Conversion{From: "USD", To: "EUR", Rate: "x", FromExp: 2, ToExp: 2}).Convert(1); err == nil || errors.Is(err, money.ErrOverflow) {
		t.Errorf("invalid rate: got %v, want an error", err)
	}
	if _, err := (ledger.Conversion{From: "USD", To: "EUR", Rate: "1", FromExp: -1, ToExp: 2}).Convert(1); err == nil || errors.Is(err, money.ErrOverflow) {
		t.Errorf("invalid exponent: got %v, want an error", err)
	}
	// Convert rounds once, half away from zero: 11,111.11 USD at 0.9 is
	// 9,999.999 EUR, 10,000.00 rounded.
	if got, err := (ledger.Conversion{From: "USD", To: "EUR", Rate: "0.9", FromExp: 2, ToExp: 2}).Convert(1111111); err != nil || got != 1000000 {
		t.Errorf("converted: got %d (%v), want 1000000", got, err)
	}
	// Rounded into the other currency's minor units, an amount can leave the
	// range; Convert reports it rather than wrapping.
	if _, err := (ledger.Conversion{From: "EUR", To: "USD", Rate: "2", FromExp: 2, ToExp: 2}).Convert(math.MaxInt64); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("out of range: got %v, want money.ErrOverflow", err)
	}
}

// TestUnreadableStoredRate: a rate that is not a plain positive decimal,
// which CreateRate cannot store but a direct write to the table could
// ('NaN' passes the column's rate > 0 check), is refused with an error,
// never read as nil and dereferenced.
func TestUnreadableStoredRate(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'EUR', 'GBP', '2026-01-01', 'NaN', 'test', $2)`, e.ten.ID, e.ten.Human.ID)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := ledger.Rates{LegalEntities: tenancy.Directory{}}.ExactToCompany(ctx, e.de, "GBP", date("2026-03-10"), 100)
		return err
	})
	pgtest.WantErr(t, err, `stored rate "NaN"`)
}
