package consolidation

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestTranslateRefusals: a balance of an account that is not the
// company's is an error; one of an account without a group account fails
// the run (consolidation.account_unmapped, naming the account and the
// company); a balance too large to translate is an error; a trading
// partner tenancy no longer lists is ordered by its id; a chart without
// the role gives the computed row no group account.
func TestTranslateRefusals(t *testing.T) {
	bank := Account{ID: uuid.New(), Number: "100000", Name: "Bank", Type: config.Asset}
	base := func() figures {
		return figures{company: Company{Code: "1000"}, year: 2026, accounts: map[uuid.UUID]Account{bank.ID: bank},
			mapping: map[string]string{"100000": "G1000"}, partners: map[uuid.UUID]string{}, rate: big.NewRat(2, 1), expLC: 2, expGC: 2}
	}
	f := base()
	f.balances = []Balance{{AccountID: uuid.New(), LC: 100}}
	if _, err := translate(f); err == nil || !strings.Contains(err.Error(), "is not the company's") {
		t.Errorf("an account not the company's: got %v, want an error", err)
	}
	f = base()
	f.mapping = map[string]string{}
	f.balances = []Balance{{AccountID: bank.ID, LC: 100}}
	var p *problem.Problem
	if _, err := translate(f); !errors.As(err, &p) || p.Code != "consolidation.account_unmapped" || !strings.Contains(p.Detail, "100000 Bank of company 1000") {
		t.Errorf("an unmapped account: got %v, want consolidation.account_unmapped naming it", err)
	}
	f = base()
	f.balances = []Balance{{AccountID: bank.ID, LC: money.Minor(1 << 62)}}
	if _, err := translate(f); err == nil || !strings.Contains(err.Error(), "translate account 100000 of company 1000") {
		t.Errorf("a balance too large: got %v, want an error", err)
	}
	f = base()
	gone := uuid.New()
	f.balances = []Balance{{AccountID: bank.ID, TradingPartnerID: &gone, LC: 100, GC: 150}}
	rows, err := translate(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rows); got != 2 || rows[0].groupAccount != "" || rows[0].kind != kindCTA || rows[1].partnerCode != gone.String() || rows[1].gc != 200 {
		t.Errorf("a partner unknown and no cta account: got %+v, want the cta row without an account first, the balance at 2.00 with the partner's id", rows)
	}
}
