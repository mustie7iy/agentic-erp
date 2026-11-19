package receivables_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// TestReverseSourced (T76i): the context that entered a debit memo from
// its document reverses it, naming that document, on the memo's posting
// date, the memo then reversed with the reason, once; another document
// named, a credit memo so entered or a memo a person entered is an
// error, not a refusal; one that does not exist is not found; outside a
// unit of work it fails.
func TestReverseSourced(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	notice := uuid.Must(uuid.NewV7())
	memo := e.sourced(t, receivables.KindDebitMemo, notice, notice.String())
	fromSales := uuid.MustParse(e.salesMemo(t))
	byHand := uuid.MustParse(e.post(t, e.entered(t, e.debitMemo(e.customer, "2026-03-10", e.item("10.00", ""))), "2026-03-12")["id"].(string))
	reverse := func(id, source uuid.UUID) (receivables.Invoice, error) {
		var v receivables.Invoice
		err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			v, err = h.ReverseSourced(ctx, id, source, receivables.Reversal{Reason: "Run in error"})
			return err
		})
		return v, err
	}
	for _, c := range []struct {
		name       string
		id, source uuid.UUID
	}{
		{"another document named", memo, uuid.Must(uuid.NewV7())},
		{"a credit memo another context entered", fromSales, fromSales},
		{"a memo a person entered", byHand, byHand},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p *problem.Problem
			if _, err := reverse(c.id, c.source); err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), "was not entered from document") {
				t.Errorf("got %v, want an error that is no refusal", err)
			}
		})
	}
	if _, err := reverse(uuid.Must(uuid.NewV7()), notice); problem.From(err).Status != 404 {
		t.Errorf("a memo that does not exist: got %v, want 404", err)
	}
	if _, err := h.ReverseSourced(context.Background(), memo, notice, receivables.Reversal{Reason: "x"}); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	v, err := reverse(memo, notice)
	if err != nil {
		t.Fatalf("the dunning notice's memo: got %v, want it reversed", err)
	}
	if v.Status != receivables.Reversed || v.Reversal == nil || !v.Reversal.PostingDate.Equal(*v.PostingDate) || v.Reversal.Reason != "Run in error" {
		t.Errorf("the memo: got %s with %+v, want it reversed on its posting date with the reason", v.Status, v.Reversal)
	}
	if _, err := reverse(memo, notice); problem.From(err).Code != "receivables.invalid_status" {
		t.Errorf("reversed again: got %v, want receivables.invalid_status", err)
	}
}
