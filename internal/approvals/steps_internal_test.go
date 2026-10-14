package approvals

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRequisitionTierAtTheThresholdsDigits: a threshold is validated with
// the digits of each company currency, so it can hold more decimals than
// the requisition's currency (1000.125 for a BHD company, a USD
// requisition). The tier is chosen by comparing the two exactly, never by
// reading the threshold with the requisition's digits and taking a failed
// read as 0.
func TestRequisitionTierAtTheThresholdsDigits(t *testing.T) {
	doc := &config.Config{}
	doc.Purchasing.RequisitionApproval = []config.ApprovalTier{
		{UpTo: " 1000.125 ", Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}}, // as validation accepts it
		{Steps: []config.ApprovalStep{{Role: "cfo", Approvals: 1}}},
	}
	for name, c := range map[string]struct {
		amount   money.Minor
		currency string
		want     string
	}{
		"under the threshold": {100012, "USD", "team_lead"},  // 1000.12 USD
		"over the threshold":  {100013, "USD", "cfo"},        // 1000.13 USD
		"at the threshold":    {1000125, "BHD", "team_lead"}, // 1000.125 BHD: up to is inclusive
		"more digits than it": {1000126, "BHD", "cfo"},       // 1000.126 BHD
	} {
		t.Run(name, func(t *testing.T) {
			var got []policyStep
			if err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
				var err error
				got, err = steps(context.Background(), tx, doc, NewRequest{ObjectType: Requisition, Amount: &c.amount, Currency: &c.currency})
				return err
			}); err != nil {
				t.Fatalf("steps: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("steps for %d minor %s: got %v, want one step", c.amount, c.currency, got)
			}
			if got[0].role != c.want {
				t.Errorf("steps for %d minor %s: got %s, want %s", c.amount, c.currency, got[0].role, c.want)
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
	err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
		_, err := steps(context.Background(), tx, doc, NewRequest{ObjectType: Requisition, Amount: &amount, Currency: &usd})
		return err
	})
	pgtest.WantErr(t, err, `invalid decimal "x"`)
}
