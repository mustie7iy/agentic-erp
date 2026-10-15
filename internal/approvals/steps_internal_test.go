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
