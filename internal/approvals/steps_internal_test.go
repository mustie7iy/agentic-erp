package approvals

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// inCompany is a company whose currency has exp digits and is the
// requisition's own: the ledger's conversion is the identity. Conversion
// between currencies is tested end to end in procurement
// (TestThresholdInTheCompanysCurrency).
type inCompany struct{ exp int }

func (c inCompany) ExactToCompany(_ context.Context, _ uuid.UUID, _ string, _ time.Time, amount money.Minor) (*big.Rat, error) {
	return money.Rat(amount, c.exp), nil
}

// TestRequisitionTierAtTheThresholdsDigits: a threshold is validated with
// the digits of each company currency, so it can hold more decimals than
// this company's currency (1000.125 for a BHD company, a USD company). The
// tier is chosen by comparing the two exactly, never by reading the
// threshold with the company's digits and taking a failed read as 0.
func TestRequisitionTierAtTheThresholdsDigits(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.RequisitionApproval = []config.ApprovalTier{
		{UpTo: " 1000.125 ", Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}}, // as validation accepts it
		{Steps: []config.ApprovalStep{{Role: "cfo", Approvals: 1}}},
	}
	usd := "USD"
	for name, c := range map[string]struct {
		amount money.Minor
		exp    int
		want   string
	}{
		"under the threshold": {100012, 2, "team_lead"},  // 1000.12 USD
		"over the threshold":  {100013, 2, "cfo"},        // 1000.13 USD
		"at the threshold":    {1000125, 3, "team_lead"}, // 1000.125 BHD: up to is inclusive
	} {
		t.Run(name, func(t *testing.T) {
			got, err := steps(context.Background(), inCompany{c.exp}, doc, NewRequest{ObjectType: Requisition, Amount: &c.amount, Currency: &usd})
			if err != nil {
				t.Fatalf("steps: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("steps for %d minor: got %v, want one step", c.amount, got)
			}
			if got[0].role != c.want {
				t.Errorf("steps for %d minor: got %s, want %s", c.amount, got[0].role, c.want)
			}
		})
	}
}

// TestRequisitionCurrencyWithMoreDigits: a threshold with fewer decimals
// than the company's currency (1000.12, a BHD company) is compared at the
// company's digits: 1000.120 is within it, 1000.121 is not.
func TestRequisitionCurrencyWithMoreDigits(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.RequisitionApproval = []config.ApprovalTier{
		{UpTo: "1000.12", Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: "cfo", Approvals: 1}}},
	}
	bhd := "BHD"
	for amount, want := range map[money.Minor]string{1000120: "team_lead", 1000121: "cfo"} {
		t.Run(fmt.Sprint(amount), func(t *testing.T) {
			got, err := steps(context.Background(), inCompany{3}, doc, NewRequest{ObjectType: Requisition, Amount: &amount, Currency: &bhd})
			if err != nil {
				t.Fatalf("steps: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("steps for %d minor BHD: got %v, want one step", amount, got)
			}
			if got[0].role != want {
				t.Errorf("steps for %d minor BHD: got %s, want %s", amount, got[0].role, want)
			}
		})
	}
}

// TestRequisitionThresholdNotADecimal: a threshold the document validation
// would have refused is reported, not taken as 0.
func TestRequisitionThresholdNotADecimal(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.RequisitionApproval = []config.ApprovalTier{{UpTo: "x", Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}}}
	amount, usd := money.Minor(100), "USD"
	_, err := steps(context.Background(), inCompany{2}, doc, NewRequest{ObjectType: Requisition, Amount: &amount, Currency: &usd})
	pgtest.WantErr(t, err, `invalid decimal "x"`)
}

// noRate is a ledger without the rate a requisition needs.
type noRate struct{ err error }

func (r noRate) ExactToCompany(context.Context, uuid.UUID, string, time.Time, money.Minor) (*big.Rat, error) {
	return nil, r.err
}

// TestRequisitionWithoutARate: a requisition the ledger cannot convert is
// refused with the ledger's reason; its amount is never compared with the
// thresholds unconverted.
func TestRequisitionWithoutARate(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.RequisitionApproval = []config.ApprovalTier{{Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}}}
	amount, gbp := money.Minor(100), "GBP"
	missing := errors.New("no rate")
	if _, err := steps(context.Background(), noRate{missing}, doc, NewRequest{ObjectType: Requisition, Amount: &amount, Currency: &gbp}); !errors.Is(err, missing) {
		t.Errorf("steps: got %v, want %v", err, missing)
	}
}

// TestPeriodUnlockSteps: unlocking a locked period needs one approval by
// the controller with the period unlock permission (T32), whatever the
// configuration says.
func TestPeriodUnlockSteps(t *testing.T) {
	got, err := steps(context.Background(), nil, &config.Config{}, NewRequest{ObjectType: PeriodUnlock})
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if want := []policyStep{{"controller", 1}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("steps: got %v, want %v", got, want)
	}
	if got, want := ObjectPermissions[PeriodUnlock], "ledger.period.unlock.approve"; got != want {
		t.Errorf("permission: got %q, want %q", got, want)
	}
}

// scaled is a ledger converting at a fixed rate into the company's
// currency (two digits), recording what it was asked to convert.
type scaled struct {
	rate  *big.Rat
	asked *NewRequest
}

func (s scaled) ExactToCompany(_ context.Context, le uuid.UUID, from string, date time.Time, amount money.Minor) (*big.Rat, error) {
	*s.asked = NewRequest{LegalEntityID: le, Currency: &from, Date: date, Amount: &amount}
	return new(big.Rat).Mul(money.Rat(amount, 2), s.rate), nil
}

// TestManualEntrySteps: a manual entry whose debits, converted exactly into
// its company's currency on its posting date, are within the company's
// manual_entry_approval_limit needs no approval; above it the controller
// approves it (T35). A company without a limit fails closed.
func TestManualEntrySteps(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000", ManualEntryApprovalLimit: " 10000.00 "}, {Code: "2000"}}}
	controller := []policyStep{{"controller", 1}}
	le, posted := uuid.New(), time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		company string
		amount  money.Minor // EUR cents
		rate    *big.Rat
		want    []policyStep
	}{
		{"within the limit", "1000", 400000, big.NewRat(2, 1), nil},
		{"at the limit", "1000", 500000, big.NewRat(2, 1), nil},
		{"above the limit", "1000", 500001, big.NewRat(2, 1), controller},
		{"at the limit at a third", "1000", 3000000, big.NewRat(1, 3), nil},
		// 10000.00333...: within the limit only if rounded to cents first
		{"above the limit by less than a cent", "1000", 3000001, big.NewRat(1, 3), controller},
		{"a company without a limit", "2000", 1, big.NewRat(2, 1), controller},
		{"a company the configuration lacks", "9999", 1, big.NewRat(2, 1), controller},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked NewRequest
			eur := "EUR"
			got, err := steps(context.Background(), scaled{c.rate, &asked}, doc, NewRequest{ObjectType: ManualEntry, Company: c.company,
				LegalEntityID: le, Date: posted, Amount: &c.amount, Currency: &eur})
			if err != nil {
				t.Fatalf("steps: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("steps: got %v, want %v", got, c.want)
			}
			if asked.LegalEntityID != le {
				t.Errorf("converted for: got %v, want %v", asked.LegalEntityID, le)
			}
			if !asked.Date.Equal(posted) {
				t.Errorf("converted on: got %v, want %v", asked.Date, posted)
			}
			if *asked.Currency != eur {
				t.Errorf("converted from: got %s, want %s", *asked.Currency, eur)
			}
			if *asked.Amount != c.amount {
				t.Errorf("converted amount: got %d, want %d", *asked.Amount, c.amount)
			}
		})
	}
	if got, want := ObjectPermissions[ManualEntry], "ledger.entry.approve"; got != want {
		t.Errorf("permission: got %q, want %q", got, want)
	}
}

// TestManualEntryLimitNotADecimal: a limit validation would have refused is
// reported, not taken as 0 or as no limit.
func TestManualEntryLimitNotADecimal(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000", ManualEntryApprovalLimit: "lots"}}}
	amount, usd := money.Minor(100), "USD"
	_, err := steps(context.Background(), inCompany{2}, doc, NewRequest{ObjectType: ManualEntry, Company: "1000", Amount: &amount, Currency: &usd})
	pgtest.WantErr(t, err, `invalid decimal "lots"`)
}

// TestManualEntryWithoutARate: an entry the ledger cannot convert is
// refused with the ledger's reason.
func TestManualEntryWithoutARate(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000", ManualEntryApprovalLimit: "10.00"}}}
	amount, gbp := money.Minor(100), "GBP"
	missing := errors.New("no rate")
	if _, err := steps(context.Background(), noRate{missing}, doc, NewRequest{ObjectType: ManualEntry, Company: "1000", Amount: &amount, Currency: &gbp}); !errors.Is(err, missing) {
		t.Errorf("steps: got %v, want %v", err, missing)
	}
}

// document is a configuration source answering doc or err.
type document struct {
	doc *config.Config
	err error
}

func (d document) Document(context.Context) (*config.Config, int32, error) { return d.doc, 1, d.err }

// TestRequired: Required answers whether the active configuration requires
// approval, without opening a request (the ledger refuses a direct manual
// entry above the limit, T35).
func TestRequired(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000", ManualEntryApprovalLimit: "10.00"}}}
	usd := "USD"
	for name, c := range map[string]struct {
		amount money.Minor
		want   bool
	}{"within the limit": {1000, false}, "above it": {1001, true}} {
		t.Run(name, func(t *testing.T) {
			a := Approvals{Settings: document{doc: doc}, Rates: inCompany{2}}
			got, err := a.Required(context.Background(), NewRequest{ObjectType: ManualEntry, Company: "1000", Amount: &c.amount, Currency: &usd})
			if err != nil {
				t.Fatalf("Required: %v", err)
			}
			if got != c.want {
				t.Errorf("Required: got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("without the configuration", func(t *testing.T) {
		unread := errors.New("no document")
		amount := money.Minor(1)
		if _, err := (Approvals{Settings: document{err: unread}}).Required(context.Background(), NewRequest{ObjectType: ManualEntry, Amount: &amount, Currency: &usd}); !errors.Is(err, unread) {
			t.Errorf("Required: got %v, want %v", err, unread)
		}
	})
	t.Run("an unknown object type", func(t *testing.T) {
		if _, err := (Approvals{Settings: document{doc: doc}}).Required(context.Background(), NewRequest{ObjectType: "nothing"}); err == nil {
			t.Error("Required: got no error, want the unknown object type refused")
		}
	})
}

// TestOrderSteps: the release of a purchase order without an approved
// requisition, and a change that raises a released order's total, are
// routed by the order's total through purchasing.order_release_approval,
// in the company's currency (T47c).
func TestOrderSteps(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.OrderReleaseApproval = []config.ApprovalTier{
		{UpTo: "5000.00", Steps: []config.ApprovalStep{{Role: "buyer_lead", Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: "buyer_lead", Approvals: 1}, {Role: "cfo", Approvals: 1}}},
	}
	eur := "EUR"
	for _, c := range []struct {
		name   string
		object string
		amount money.Minor
		want   []policyStep
	}{
		{"an order at the threshold", PurchaseOrder, 500000, []policyStep{{"buyer_lead", 1}}},
		{"an order above it", PurchaseOrder, 500001, []policyStep{{"buyer_lead", 1}, {"cfo", 1}}},
		{"a change to a total at the threshold", OrderChange, 500000, []policyStep{{"buyer_lead", 1}}},
		{"a change to a total above it", OrderChange, 500001, []policyStep{{"buyer_lead", 1}, {"cfo", 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := steps(context.Background(), inCompany{2}, doc, NewRequest{ObjectType: c.object, Amount: &c.amount, Currency: &eur})
			if err != nil {
				t.Fatalf("steps: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("steps: got %v, want %v", got, c.want)
			}
		})
	}
	for _, object := range []string{PurchaseOrder, OrderChange} {
		t.Run(object+" permission", func(t *testing.T) {
			if got, want := ObjectPermissions[object], "procurement.order.approve"; got != want {
				t.Errorf("permission: got %q, want %q", got, want)
			}
		})
	}
	// A document from before T47c has no order tiers: orders are refused
	// rather than released unapproved.
	t.Run("without order tiers", func(t *testing.T) {
		amount := money.Minor(1)
		_, err := steps(context.Background(), inCompany{2}, &config.Config{}, NewRequest{ObjectType: PurchaseOrder, Amount: &amount, Currency: &eur})
		pgtest.WantErr(t, err, "purchasing.order_release_approval")
	})
}
