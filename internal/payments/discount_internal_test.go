package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // constant test dates
	return d
}

// TestDiscountRounding: a discount is the tier's percent of the item,
// rounded half away from zero, and an item always pays at least a minor
// unit.
func TestDiscountRounding(t *testing.T) {
	terms := func(pct string) *ledger.ItemTerms {
		return &ledger.ItemTerms{BaselineDate: day("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: pct}}}
	}
	for name, c := range map[string]struct {
		pct        string
		paid, want money.Minor
	}{
		"half away from zero":           {"2.5", 1, 0},
		"half away from zero, up":       {"2.5", 20, 1},
		"a credit, half away from zero": {"2.5", -20, -1},
		"the whole amount, less a unit": {"99.9", 1, 0},
		"a credit, less a unit":         {"99.9", -2, -1},
		"three decimals":                {"2.125", 10000, 213},
		"a zero item":                   {"3", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := discountOn(terms(c.pct), c.paid, day("2026-03-05"))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("discount of %s%% on %d: got %d, want %d", c.pct, c.paid, got, c.want)
			}
		})
	}
	two := &ledger.ItemTerms{BaselineDate: day("2026-03-01"), NetDays: 45, Discounts: []ledger.Discount{{Days: 14, Percent: "3"}, {Days: 30, Percent: "2"}}}
	for name, c := range map[string]struct {
		run, dueBy string
		want       bool
	}{
		"a tier lapsing before the due-by date": {"2026-03-10", "2026-03-20", true},
		"the same tier on the due-by date":      {"2026-03-10", "2026-03-12", false},
		"a due-by date before the run date":     {"2026-03-20", "2026-03-10", false},
		"no tier on the run date":               {"2026-04-10", "2026-04-12", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := losesDiscount(two, day(c.run), day(c.dueBy)); got != c.want {
				t.Errorf("loses discount paying on %s rather than %s: got %v, want %v", c.dueBy, c.run, got, c.want)
			}
		})
	}
	if _, err := discountOn(terms("x"), 100, day("2026-03-05")); err == nil {
		t.Error("discount at a percent that is not one: got none, want an error")
	}
}

// TestProposalDiscountsOutOfRange: a proposal whose discounts are not a
// percent, or add up beyond an amount, fails rather than paying a wrong
// amount.
func TestProposalDiscountsOutOfRange(t *testing.T) {
	supplier, bank := uuid.New(), uuid.New()
	payees := map[uuid.UUID]partner.Payee{supplier: {Status: partner.Active, BankAccountID: &bank}}
	bad := &ledger.ItemTerms{BaselineDate: day("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: "x"}}}
	err := new(proposal).propose([]candidate{{supplier: supplier, items: []Item{{Amount: 100, terms: bad}}}}, payees, Run{RunDate: day("2026-03-05")}, "2000", 0)
	if err == nil || !strings.Contains(err.Error(), "the discount on item") {
		t.Errorf("a percent that is not one: got %v, want the discount refused", err)
	}
	huge := money.Minor(9_000_000_000_000_000_000)
	c := candidate{supplier: supplier, items: []Item{{Amount: huge, Discount: huge - 1}, {Amount: huge, Discount: huge - 1}}}
	var pr proposal
	if err := pr.pay(c, bank, via{}, 1, Run{}, "2000"); err == nil {
		t.Error("a supplier's discounts beyond an amount: got none, want an error")
	}
	pr = proposal{discount: huge}
	if err := pr.pay(candidate{supplier: supplier, items: []Item{{Amount: huge, Discount: huge - 1}}}, bank, via{}, 1, Run{}, "2000"); err == nil {
		t.Error("a run's discounts beyond an amount: got none, want an error")
	}
}

// failingAccounts is a ledger whose account determination fails.
type failingAccounts struct{}

func (failingAccounts) Determination(context.Context, uuid.UUID, string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, errors.New("ledger unavailable")
}

func (failingAccounts) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	return ledger.TaxCode{}, false, errors.New("ledger unavailable")
}

func (failingAccounts) Chart(context.Context, uuid.UUID) (map[string]ledger.ChartAccount, error) {
	return nil, errors.New("ledger unavailable")
}

func (failingAccounts) Balances(context.Context, uuid.UUID, uuid.UUID, string, []time.Time) ([]money.Minor, error) {
	return nil, errors.New("ledger unavailable")
}

// TestCashDiscountAccountUnread: a run that takes a discount does not
// propose when the cash discount account cannot be read.
func TestCashDiscountAccountUnread(t *testing.T) {
	e := &Endpoints{Accounts: failingAccounts{}}
	err := e.requireCashDiscountAccount(context.Background(), Run{}, proposal{itemDiscounts: []money.Minor{0, 3}})
	if err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Errorf("got %v, want the ledger's error", err)
	}
}

// entities is a legal entity directory of one company code.
type entities string

func (code entities) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{ID: id, Code: string(code)}, true, nil
}

// TestCompanyDayUnknownZone: a stored document whose company names a zone
// this build cannot load (validation refuses one, so only a zone database
// that lost it) fails the release rather than judging the discount in UTC,
// and a run's legal entity the directory does not know is refused as
// every company date refuses it (config.EntityDay, review:batch27).
func TestCompanyDayUnknownZone(t *testing.T) {
	at := func() time.Time { return time.Date(2026, 3, 16, 2, 0, 0, 0, time.UTC) }
	doc := &config.Config{Companies: []config.Company{{Code: "2000", TimeZone: "America/Nowhere"}}}
	until := day("2026-03-15")
	for name, tc := range map[string]struct {
		entities LegalEntities
		want     string
	}{
		"a zone that does not load": {entities("2000"), `company 2000: time zone "America/Nowhere"`},
		"an unknown legal entity":   {noEntities{}, "request.reference_not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{LegalEntities: tc.entities, Now: at}
			err := e.recheckDiscounts(context.Background(), Run{ID: uuid.New(), Status: Proposed}, []Payment{{}, {DiscountUntil: &until}}, doc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// noEntities is a legal entity directory that knows none.
type noEntities struct{}

func (noEntities) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, nil
}
