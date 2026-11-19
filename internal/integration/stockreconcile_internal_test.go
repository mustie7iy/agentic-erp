package integration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
