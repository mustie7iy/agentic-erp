package payments

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestRequestsTooLarge: requests whose amounts add up to more than an
// amount holds fail the proposal (payments.amount_too_large), whether they
// are paid or added to their supplier's exclusion.
func TestRequestsTooLarge(t *testing.T) {
	bank := uuid.New()
	paid, blocked := uuid.New(), uuid.New()
	payees := map[uuid.UUID]partner.Payee{paid: {Status: partner.Active, BankAccountID: &bank}}
	huge := func(supplier uuid.UUID) DownPayment {
		return DownPayment{ID: uuid.New(), SupplierID: supplier, Amount: money.Minor(math.MaxInt64 - 1)}
	}
	for name, requests := range map[string][]DownPayment{
		"paid":     {huge(paid), huge(paid)},
		"excluded": {huge(blocked), huge(blocked)},
	} {
		t.Run(name, func(t *testing.T) {
			var pr proposal
			err := pr.payRequests(requests, payees, Run{DocumentNumber: "1"}, "2000", 5)
			if got := problem.From(err).Code; got != "payments.amount_too_large" {
				t.Errorf("got %s (%v), want payments.amount_too_large", got, err)
			}
		})
	}
}

// TestRequestTaxCodeUnread: a run that pays a taxed request does not
// propose, nor release, when the tax code cannot be read.
func TestRequestTaxCodeUnread(t *testing.T) {
	e := &Endpoints{Accounts: failingAccounts{}}
	v1 := "V1"
	err := e.requireRequestTaxCodes(context.Background(), Run{}, []DownPayment{{TaxCode: &v1, Tax: 1}}, nil)
	if err == nil || err.Error() != "ledger unavailable" {
		t.Errorf("got %v, want the ledger's error", err)
	}
	if err := e.requireDownPaymentAccount(context.Background(), Run{}, proposal{kinds: []string{KindDownPayment}}); err == nil {
		t.Error("down payment account unread: got nil, want the ledger's error")
	}
}
