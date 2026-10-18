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
// amount holds fail the proposal (payments.amount_too_large).
func TestRequestsTooLarge(t *testing.T) {
	bank := uuid.New()
	paid := uuid.New()
	payees := map[uuid.UUID]partner.Payee{paid: {Status: partner.Active, BankAccountID: &bank}}
	huge := func(supplier uuid.UUID) DownPayment {
		return DownPayment{ID: uuid.New(), SupplierID: supplier, Amount: money.Minor(math.MaxInt64 - 1)}
	}
	var pr proposal
	err := pr.payRequests([]DownPayment{huge(paid), huge(paid)}, nil, payees, Run{DocumentNumber: "1"}, "2000", 5)
	if got := problem.From(err).Code; got != "payments.amount_too_large" {
		t.Errorf("got %s (%v), want payments.amount_too_large", got, err)
	}
}

// TestRequestTaxCodeUnread: a run that pays a taxed request does not
// propose, nor release, when the tax code cannot be read.
func TestRequestTaxCodeUnread(t *testing.T) {
	e := &Endpoints{Accounts: failingAccounts{}}
	v1 := "V1"
	_, err := e.invalidTaxCodes(context.Background(), Run{}, []DownPayment{{TaxCode: &v1, Tax: 1}})
	if err == nil || err.Error() != "ledger unavailable" {
		t.Errorf("proposal: got %v, want the ledger's error", err)
	}
	e.DownPayments = taxedRequest{}
	request := uuid.New()
	err = e.recheckRequests(context.Background(), Run{Status: Proposed}, []Payment{{Kind: KindDownPayment, RequestID: &request}})
	if err == nil || err.Error() != "ledger unavailable" {
		t.Errorf("release: got %v, want the ledger's error", err)
	}
	if err := e.requireDownPaymentAccount(context.Background(), Run{}, proposal{kinds: []string{KindDownPayment}}); err == nil {
		t.Error("down payment account unread: got nil, want the ledger's error")
	}
}

// taxedRequest is a payables context whose requests have tax code V1.
type taxedRequest struct{ DownPayments }

func (taxedRequest) Request(context.Context, uuid.UUID) (DownPayment, error) {
	v1 := "V1"
	return DownPayment{TaxCode: &v1, Tax: 1}, nil
}
