package payables

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// taxNumbers answers HasTaxNumber with held or err.
type taxNumbers struct {
	held bool
	err  error
}

func (n taxNumbers) HasTaxNumber(context.Context, uuid.UUID, string) (bool, error) { return n.held, n.err }

// TestSellerOf: the seller's TRN not held by the order's supplier is
// refused naming at most its last four characters; the partner context
// failing is its error (ADR-0061 §6).
func TestSellerOf(t *testing.T) {
	down := errors.New("partner down")
	for name, c := range map[string]struct {
		numbers    taxNumbers
		trn, want  string
		wantDetail string
	}{
		"held":                {taxNumbers{held: true}, "100555555500003", "", ""},
		"not held":            {taxNumbers{}, "100555555500003", "payables.einvoice_seller_unknown", "ending 0003,"},
		"a short one":         {taxNumbers{}, "123", "payables.einvoice_seller_unknown", "ending 123,"},
		"the partner failing": {taxNumbers{err: down}, "100555555500003", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := (&Endpoints{TaxNumbers: c.numbers}).sellerOf(context.Background(), uuid.Nil, c.trn)
			switch {
			case c.numbers.err != nil:
				if !errors.Is(err, down) {
					t.Errorf("got %v, want the partner's error", err)
				}
			case c.want == "":
				if err != nil {
					t.Errorf("got %v, want none", err)
				}
			default:
				if p := problem.From(err); p.Code != c.want || !strings.Contains(p.Detail, c.wantDetail) || strings.Contains(p.Detail, "100555555500003") {
					t.Errorf("got %v, want %s naming %q and no TRN in clear", err, c.want, c.wantDetail)
				}
			}
		})
	}
}

// TestExtractionOf: an e-invoice reads as capture's extraction, amounts
// in its currency's digits; an order line that is no item number is
// refused, quoting at most 40 characters of it.
func TestExtractionOf(t *testing.T) {
	d := EInvoice{Number: "GOI-1", IssueDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), Currency: "AED", Exponent: 2, OrderReference: "4500000001",
		Lines: []EInvoiceLine{{OrderLine: "10", Description: "Desk", Quantity: "4", Net: 100000, Category: "S", Percent: "5"}}, Tax: 5000, Gross: 105000}
	ext, err := extractionOf(d)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join([]string{ext.DocumentKind, ext.InvoiceNumber, ext.InvoiceDate, ext.PurchaseOrder, ext.GrossAmount, ext.TaxAmount,
		ext.Lines[0].Amount, ext.Lines[0].TaxRate}, " "), "invoice GOI-1 2026-03-10 4500000001 1050.00 50.00 1000.00 5"; got != want || ext.Lines[0].POLine != 10 {
		t.Errorf("got %s line %d, want %s line 10", got, ext.Lines[0].POLine, want)
	}
	for name, line := range map[string]string{"a word": "ten", "zero": "0", "a long one": strings.Repeat("x", 50)} {
		t.Run(name, func(t *testing.T) {
			d.Lines[0].OrderLine = line
			_, err := extractionOf(d)
			if p := problem.From(err); p.Code != "payables.unreadable_document" || len(p.Detail) > 160 {
				t.Errorf("got %v, want payables.unreadable_document quoting at most 40 characters", err)
			}
		})
	}
}
