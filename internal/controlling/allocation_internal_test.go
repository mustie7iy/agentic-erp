package controlling

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestBasisPoints: a percentage is read exactly, above 0 and at most 100
// with up to two decimals, and shown with two.
func TestBasisPoints(t *testing.T) {
	for in, want := range map[string]int32{"60": 6000, "40.0": 4000, "33.33": 3333, "100": 10000, "100.00": 10000, "0.01": 1,
		"0": 0, "0.00": 0, "100.01": 0, "33.333": 0, "-5": 0, "abc": 0, "": 0} {
		t.Run(in, func(t *testing.T) {
			got, ok := basisPoints(in)
			if got != want || ok != (want != 0) {
				t.Errorf("basis points of %q: got %d %v, want %d %v", in, got, ok, want, want != 0)
			}
		})
	}
	if got := percent(3305); got != "33.05" {
		t.Errorf("percent of 3305 basis points: got %s, want 33.05", got)
	}
}

// lineOf is a posting line as "account amount center", by the names given.
func lineOf(l ledger.PostingLine, names map[uuid.UUID]string) string {
	return fmt.Sprintf("%s %d %s", names[l.AccountID], l.Amount, names[*l.CostCenterID])
}

// TestDistributionLines: per account the sender is credited with its cost
// and the receivers are debited with shares that add up to it to the
// minor unit; a share that rounds to nothing has no line; a credit
// balance moves as one; a total beyond an amount is an error.
func TestDistributionLines(t *testing.T) {
	s, a, b, c := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	x, y := uuid.New(), uuid.New()
	names := map[uuid.UUID]string{s: "S", a: "A", b: "B", c: "C", x: "x", y: "y"}
	thirds := cycle{SenderID: s, Receivers: []cycleReceiver{{CostCenterID: a, ShareBP: 3333}, {CostCenterID: b, ShareBP: 3333}, {CostCenterID: c, ShareBP: 3334}}}
	sixty := cycle{SenderID: s, Receivers: []cycleReceiver{{CostCenterID: a, ShareBP: 6000}, {CostCenterID: b, ShareBP: 4000}}}
	for name, tc := range map[string]struct {
		cycle cycle
		costs []accountCost
		want  string
		total money.Minor
	}{
		"sixty and forty of an odd cent": {sixty, []accountCost{{AccountID: x, Amount: 10001}}, "x -10001 S; x 6001 A; x 4000 B", 10001},
		"thirds of a hundred and one":    {thirds, []accountCost{{AccountID: x, Amount: 10001}}, "x -10001 S; x 3333 A; x 3333 B; x 3335 C", 10001},
		"one cent goes to one receiver":  {sixty, []accountCost{{AccountID: x, Amount: 1}}, "x -1 S; x 1 A", 1},
		"a credit balance moves as one":  {sixty, []accountCost{{AccountID: x, Amount: -10001}}, "x 10001 S; x -6001 A; x -4000 B", -10001},
		"each account on its own": {sixty, []accountCost{{AccountID: x, Amount: 500}, {AccountID: y, Amount: -100}},
			"x -500 S; x 300 A; x 200 B; y 100 S; y -60 A; y -40 B", 400},
	} {
		t.Run(name, func(t *testing.T) {
			lines, total, err := distributionLines(tc.cycle, tc.costs)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(lines))
			perAccount := map[uuid.UUID]money.Minor{}
			for i, l := range lines {
				got[i] = lineOf(l, names)
				perAccount[l.AccountID] += l.Amount
			}
			if s := strings.Join(got, "; "); s != tc.want {
				t.Errorf("lines: got %s, want %s", s, tc.want)
			}
			if total != tc.total {
				t.Errorf("total: got %d, want %d", total, tc.total)
			}
			for account, sum := range perAccount {
				if sum != 0 {
					t.Errorf("account %s: got a balance of %d, want 0", names[account], sum)
				}
			}
		})
	}
	_, _, err := distributionLines(sixty, []accountCost{{AccountID: x, Amount: math.MaxInt64}, {AccountID: y, Amount: 1}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("a total beyond an amount: got %v, want an overflow", err)
	}
}

// TestDistributionBeyondAnAmount: a run whose total does not fit in an
// amount ends before anything is posted.
func TestDistributionBeyondAnAmount(t *testing.T) {
	e := &Endpoints{Ledger: validCenters{}}
	c := cycle{ID: uuid.New(), SenderID: uuid.New(), Receivers: []cycleReceiver{{CostCenterID: uuid.New(), ShareBP: wholeBP}}}
	_, err := e.distribute(context.Background(), nil, reqctx.Principal{}, c, tenancy.Entity{}, ledger.FiscalPeriod{},
		[]accountCost{{AccountID: uuid.New(), Amount: math.MaxInt64}, {AccountID: uuid.New(), Amount: 1}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("got %v, want an overflow and nothing posted", err)
	}
}

// pagedLedger is the ledger's port answering a sender's actuals in pages
// and naming every account a primary cost account but the ones in other.
type pagedLedger struct {
	Ledger
	rows  []ledger.AssignmentActual
	other map[uuid.UUID]bool
	asked []*ledger.CenterAccount
}

func (l *pagedLedger) CostCenterActuals(_ context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error) {
	l.asked = append(l.asked, q.After)
	rows := l.rows
	if q.After != nil {
		for i, r := range rows {
			if r.CenterAccount == *q.After {
				rows = rows[i+1:]
				break
			}
		}
	}
	return rows[:min(len(rows), q.Limit)], nil
}

func (l *pagedLedger) CostAccounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CostAccount, error) {
	out := map[uuid.UUID]ledger.CostAccount{}
	for i, id := range ids {
		out[id] = ledger.CostAccount{Number: fmt.Sprintf("%06d", len(ids)-i), PrimaryCost: !l.other[id]}
	}
	return out, nil
}

// TestSenderCostsReadsEveryPage: a sender with more accounts than one
// read takes has them all read, page after page; the costs come by
// account number, without the accounts that are no primary cost accounts
// and those whose cost is zero.
func TestSenderCostsReadsEveryPage(t *testing.T) {
	sender := uuid.New()
	l := &pagedLedger{other: map[uuid.UUID]bool{}}
	for i := range costsPage + 3 {
		l.rows = append(l.rows, ledger.AssignmentActual{CenterAccount: ledger.CenterAccount{CenterID: sender, AccountID: uuid.New()}, Amount: money.Minor(i + 1)})
	}
	l.rows[0].Amount = 0
	l.other[l.rows[1].AccountID] = true
	e := &Endpoints{Ledger: l}
	costs, err := e.senderCosts(context.Background(), uuid.New(), sender, ledger.FiscalPeriod{FiscalYear: 2026, Number: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != costsPage+1 {
		t.Fatalf("costs: got %d, want %d (every account but the one without a cost and the one that is no cost account)", len(costs), costsPage+1)
	}
	if len(l.asked) != 2 || l.asked[0] != nil || l.asked[1] == nil || *l.asked[1] != l.rows[costsPage-1].CenterAccount {
		t.Errorf("reads: got %d starting after %v, want two, the second after the first page's last row", len(l.asked), l.asked)
	}
	for i := 1; i < len(costs); i++ {
		if costs[i-1].Number >= costs[i].Number {
			t.Fatalf("costs %d and %d: got account %s before %s, want them by account number", i-1, i, costs[i-1].Number, costs[i].Number)
		}
	}
	// The numbers fall as the rows rise, so the first by number is the last row read.
	if costs[0].Amount != money.Minor(costsPage+3) {
		t.Errorf("first cost by account number: got %d, want %d", costs[0].Amount, costsPage+3)
	}
}
