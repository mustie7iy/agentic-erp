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
		{UpTo: "1000.125", Steps: []config.ApprovalStep{{Role: "team_lead", Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: "cfo", Approvals: 1}}},
	}
	usd := "USD"
	for name, c := range map[string]struct {
		amount money.Minor
		want   string
	}{
		"under the threshold": {100012, "team_lead"}, // 1000.12 USD
		"over the threshold":  {100013, "cfo"},       // 1000.13 USD
	} {
		t.Run(name, func(t *testing.T) {
			var got []policyStep
			if err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
				var err error
				got, err = steps(context.Background(), tx, doc, NewRequest{ObjectType: Requisition, Amount: &c.amount, Currency: &usd})
				return err
			}); err != nil {
				t.Fatalf("steps: %v", err)
			}
			if len(got) != 1 || got[0].role != c.want {
				t.Errorf("steps for %d minor USD: got %v, want one step of %s", c.amount, got, c.want)
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
	pgtest.WantErr(t, err, `threshold "x" is not a decimal`)
}
