package ledger

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestWithholdingAtPayment: a payment withholds the rate of the base less
// the discount's share of it, rounded once, half away from zero, in the
// base's sign; codes deducted at invoice withhold nothing more.
func TestWithholdingAtPayment(t *testing.T) {
	gross := Withholding{Code: "W2", Posting: config.WithholdingAtPayment, RatePct: "15", Base: 10000}
	for _, c := range []struct {
		name                 string
		w                    Withholding
		owed, discount, want money.Minor
	}{
		{"no discount", gross, 10000, 0, 1500},
		{"3% discount", gross, 10000, 300, 1455},
		{"a base other than what is owed", gross, 9000, 270, 1455},
		{"a credit memo", Withholding{Posting: config.WithholdingAtPayment, RatePct: "15", Base: -2000}, -2000, -60, -291},
		{"rounded half away from zero", Withholding{Posting: config.WithholdingAtPayment, RatePct: "12.5", Base: 4}, 4, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.w.AtPayment(c.owed, c.discount)
			if err != nil || got != c.want {
				t.Errorf("AtPayment: got %v, %v, want %v", got, err, c.want)
			}
		})
	}
	atInvoice := Withholding{Posting: config.WithholdingAtInvoice, RatePct: "10", Base: 10000, Amount: 1000}
	if got, err := PaymentWithholding([]Withholding{atInvoice, gross}, 10000, 0); err != nil || got != 1500 {
		t.Errorf("PaymentWithholding with a code at invoice: got %v, %v, want 1500", got, err)
	}
	bad := Withholding{Code: "WX", Posting: config.WithholdingAtPayment, RatePct: "x", Base: 100}
	if _, err := PaymentWithholding([]Withholding{bad}, 100, 0); err == nil {
		t.Error("PaymentWithholding with a rate that is no decimal: got nil error, want an error")
	}
	huge := Withholding{Code: "W9", Posting: config.WithholdingAtPayment, RatePct: "99", Base: math.MaxInt64}
	if _, err := PaymentWithholding([]Withholding{huge, huge}, math.MaxInt64, 0); err == nil {
		t.Error("PaymentWithholding out of range: got nil error, want an error")
	}
}

// TestPaymentWithholdingTotals: a settlement's withholding is per account
// and code, netted across its items; amounts that cancel leave no line; a
// rate that is no decimal and totals out of range are refused.
func TestPaymentWithholdingTotals(t *testing.T) {
	account := uuid.UUID{0x01}
	w := func(base money.Minor, rate string) Withholding {
		return Withholding{Code: "W2", Posting: config.WithholdingAtPayment, RatePct: rate, AccountID: account, Base: base}
	}
	byKey, total, err := paymentWithholding([][]Withholding{{w(10000, "15")}, {w(-2000, "15")}}, []money.Minor{10000, -2000}, []money.Minor{300, 0}, nil)
	if err != nil || total != 1155 || byKey[withholdingKey{account, "W2"}] != 1155 {
		t.Errorf("netted: got %v %v, %v, want 1155 on W2", byKey, total, err)
	}
	byKey, _, err = paymentWithholding([][]Withholding{{w(1000, "10")}, {w(-1000, "10")}}, []money.Minor{1000, -1000}, nil, nil)
	if err != nil || len(byKey) != 0 {
		t.Errorf("cancelling: got %v, %v, want no line", byKey, err)
	}
	if _, _, err := paymentWithholding([][]Withholding{{w(100, "x")}}, []money.Minor{100}, nil, nil); err == nil {
		t.Error("a rate that is no decimal: got nil error, want an error")
	}
	huge := w(math.MaxInt64, "99")
	_, _, err = paymentWithholding([][]Withholding{{huge}, {huge}}, []money.Minor{math.MaxInt64, math.MaxInt64}, nil, nil)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != "ledger.amount_too_large" {
		t.Errorf("out of range: got %v, want ledger.amount_too_large", err)
	}
	w3 := huge
	w3.Code = "W3" // a code of its own: only the items' total is out of range
	_, _, err = paymentWithholding([][]Withholding{{huge}, {w3}}, []money.Minor{math.MaxInt64, math.MaxInt64}, nil, nil)
	if !errors.As(err, &p) || p.Code != "ledger.amount_too_large" {
		t.Errorf("total out of range: got %v, want ledger.amount_too_large", err)
	}
	other := uuid.UUID{0xff}
	lines := withholdingLines(map[withholdingKey]money.Minor{{other, "W1"}: 5, {account, "W2"}: 1155, {account, "W1"}: 10}, Settlement{})
	if len(lines) != 3 {
		t.Fatalf("lines: got %d, want 3", len(lines))
	}
	if got := fmt.Sprint(int64(lines[0].TC), " ", *lines[0].Text, " ", int64(lines[1].TC), " ", lines[2].AccountID == other); got != "-10 Withholding tax W1 -1155 true" {
		t.Errorf("lines: got %s, want W1 then W2 of the account, then the other account's", got)
	}
}

// TestWithheldAsProposed: a settlement that states what each item withholds
// (a payment run's release, T46f) posts only that: a recomputed withholding
// that differs, or a count other than one per item, is refused rather than
// posting an amount other than the one the run proposed and paid out.
func TestWithheldAsProposed(t *testing.T) {
	w := Withholding{Code: "W2", Posting: config.WithholdingAtPayment, RatePct: "15", Base: 10000}
	taxes, owed, discounts := [][]Withholding{{w}, nil}, []money.Minor{10000, 500}, []money.Minor{300, 0}
	for _, c := range []struct {
		name     string
		expected []money.Minor
		code     string
	}{
		{"not stated", nil, ""},
		{"as proposed", []money.Minor{1455, 0}, ""},
		{"another amount", []money.Minor{1500, 0}, "ledger.withholding_changed"},
		{"one item withholding none", []money.Minor{1455, 1}, "ledger.withholding_changed"},
		{"not one per item", []money.Minor{1455}, "ledger.invalid_withholding"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got string
			if _, _, err := paymentWithholding(taxes, owed, discounts, c.expected); err != nil {
				got = problem.From(err).Code
			}
			if got != c.code {
				t.Errorf("got %q, want %q", got, c.code)
			}
		})
	}
}
