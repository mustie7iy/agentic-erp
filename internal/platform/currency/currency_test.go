package currency_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestExponent(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only; nothing to keep
	errUnknown := errors.New("unknown")
	for name, c := range map[string]struct {
		q       currency.Querier
		code    string
		unknown error
		want    int
		wantErr error
	}{
		"EUR":                   {tx, "EUR", errUnknown, 2, nil},
		"JPY":                   {tx, "JPY", errUnknown, 0, nil},
		"BHD":                   {tx, "BHD", nil, 3, nil},
		"unknown, with problem": {tx, "XXQ", errUnknown, 0, errUnknown},
		"unknown, without":      {tx, "XXQ", nil, 0, pgx.ErrNoRows},
		"failed read":           {&pgtest.FailingTx{Tx: tx, FailAt: 1}, "EUR", errUnknown, 0, pgtest.ErrInjected},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := currency.Exponent(ctx, c.q, c.code, c.unknown)
			if c.wantErr == nil && err != nil {
				t.Fatalf("Exponent(%s): got error %v, want none", c.code, err)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Exponent(%s): got error %v, want %v", c.code, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("Exponent(%s): got %d, want %d", c.code, got, c.want)
			}
		})
	}
}

func TestLoadCodes(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only; nothing to keep
	t.Run("the named codes only", func(t *testing.T) {
		got, err := currency.LoadCodes(ctx, tx, "JPY", "BHD", "XXQ")
		if err != nil {
			t.Fatal(err)
		}
		want := currency.Exponents{"JPY": 0, "BHD": 3}
		if !maps.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("a failed read", func(t *testing.T) {
		_, err := currency.LoadCodes(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, "EUR")
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
}

// TestParseAmount: an amount with exactly the currency's minor-unit digits
// is read into minor units; another form is 400 request.invalid_amount
// naming the request's field.
func TestParseAmount(t *testing.T) {
	for name, c := range map[string]struct {
		value   string
		exp     int
		want    money.Minor
		refused bool
	}{
		"two decimals":        {"1080.50", 2, 108050, false},
		"none":                {"1080", 0, 1080, false},
		"negative":            {"-5.00", 2, -500, false},
		"too many decimals":   {"1.001", 2, 0, true},
		"too few decimals":    {"1.0", 2, 0, true},
		"decimals where none": {"1.00", 0, 0, true},
		"not a number":        {"ten", 2, 0, true},
		"out of range":        {"99999999999999999999.00", 2, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := currency.ParseAmount(c.value, "EUR", c.exp, "items[0].amount.amount")
			if !c.refused {
				if err != nil || got != c.want {
					t.Errorf("got %d (%v), want %d", got, err, c.want)
				}
				return
			}
			p := problem.From(err)
			if p.Status != 400 || p.Code != "request.invalid_amount" {
				t.Fatalf("got %v, want 400 request.invalid_amount", err)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != "items[0].amount.amount" || p.Errors[0].Code != "invalid" {
				t.Errorf("field errors: got %+v, want one naming items[0].amount.amount", p.Errors)
			}
			if !strings.Contains(p.Detail, "EUR") {
				t.Errorf("detail: got %q, want it to name the currency", p.Detail)
			}
		})
	}
}
