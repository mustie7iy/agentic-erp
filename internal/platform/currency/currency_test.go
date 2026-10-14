package currency_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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
