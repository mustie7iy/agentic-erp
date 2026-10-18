package ledger_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestCashDiscountAccountDetermined: a company's cash discount account
// (SAP SKE, T36a) is the account determination's cash_discount, which
// payments post the discounts they take to.
func TestCashDiscountAccountDetermined(t *testing.T) {
	e := setup(t)
	doc := document(t)
	doc.Company("2000").AccountDetermination.CashDiscount = "400000"
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	var number string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT g.number FROM ledger.account_determination d JOIN ledger.gl_account g ON g.id = d.account_id
		WHERE d.tenant_id = $1 AND d.legal_entity_id = $2 AND d.key = 'cash_discount'`, e.ten.ID, e.de).Scan(&number); err != nil {
		t.Fatalf("read the cash discount account: %v", err)
	}
	if number != "400000" {
		t.Errorf("cash discount account: got %s, want 400000", number)
	}
}
