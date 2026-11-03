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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // constant test dates
	return d
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
	if err := pr.pay(c, bank, via{}, 1, Run{}, "2000"); !errors.As(err, new(supplierTooLarge)) {
		t.Errorf("a supplier's discounts beyond an amount: got %v, want supplierTooLarge", err)
	}
	pr = proposal{discount: huge}
	err = pr.pay(candidate{supplier: supplier, items: []Item{{Amount: huge, Discount: huge - 1}}}, bank, via{}, 1, Run{}, "2000")
	if got := problem.From(err).Code; got != "payments.amount_too_large" || errors.As(err, new(supplierTooLarge)) {
		t.Errorf("a run's discounts beyond an amount: got %v, want the run's payments.amount_too_large", err)
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
