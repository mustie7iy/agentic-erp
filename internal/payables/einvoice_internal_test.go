package payables

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// taxNumbers answers HasTaxNumber with held or err, HoldersOfTaxNumber
// with holders or err.
type taxNumbers struct {
	held    bool
	holders []partner.Found
	err     error
}

func (n taxNumbers) HasTaxNumber(context.Context, uuid.UUID, string) (bool, error) {
	return n.held, n.err
}

func (n taxNumbers) HoldersOfTaxNumber(context.Context, string) ([]partner.Found, error) {
	return n.holders, n.err
}

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
// in its currency's digits, a credit note as a credit memo whose cited
// invoices capture's single number leaves to citedInvoices; an order line
// that is no item number is refused, quoting at most 40 characters of it.
func TestExtractionOf(t *testing.T) {
	credit, err := extractionOf(EInvoice{Kind: KindCreditMemo, Number: "GOI-7", Cited: []Citation{{Number: "GOI-1"}}, Currency: "AED", Exponent: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := credit.DocumentKind + " " + credit.InvoiceNumber + " [" + credit.CreditedInvoiceNumber + "]"; got != "credit_memo GOI-7 []" {
		t.Errorf("a credit note: got %s, want credit_memo GOI-7 []", got)
	}
	d := EInvoice{Kind: KindInvoice, Number: "GOI-1", IssueDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), Currency: "AED", Exponent: 2, OrderReference: "4500000001",
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

// TestEnterEInvoiceOutsideRequest: entering needs the request's
// transaction; without one it is refused with reqctx's error and reads
// nothing (ADR-0061 §6).
func TestEnterEInvoiceOutsideRequest(t *testing.T) {
	if _, err := (&Endpoints{}).EnterEInvoice(context.Background(), EInvoice{}); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("got %v, want %v", err, reqctx.ErrOutsideRequest)
	}
}

// suppliers answers Supplier with the partners of known, or err.
type suppliers struct {
	known map[uuid.UUID]partner.Supplier
	err   error
}

func (s suppliers) Supplier(_ context.Context, id, _ uuid.UUID) (partner.Supplier, bool, error) {
	v, ok := s.known[id]
	return v, ok, s.err
}

func (suppliers) PaymentTerm(context.Context, string) (partner.PaymentTerm, bool, error) {
	return partner.PaymentTerm{}, false, nil
}

// determination answers Determination with account, or none when it is
// nil, or err; the other questions are not asked of it.
type determination struct {
	Accounts
	account *uuid.UUID
	err     error
}

func (d determination) Determination(context.Context, uuid.UUID, string) (uuid.UUID, bool, error) {
	if d.account == nil {
		return uuid.Nil, false, d.err
	}
	return *d.account, true, d.err
}

// TestSellerSupplier: a document naming no order is the one supplier of
// the company holding the seller's TRN; a holder that is no supplier of
// the company does not count; none is payables.einvoice_seller_unknown,
// several payables.einvoice_seller_ambiguous naming their numbers, and
// neither names the TRN beyond its last four (ADR-0061 As built batch
// 89); the partner context failing is its error.
func TestSellerSupplier(t *testing.T) {
	down := errors.New("partner down")
	one, two, customer := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	known := map[uuid.UUID]partner.Supplier{one: {ID: one, IsSupplier: true, SetUp: true}, two: {ID: two, IsSupplier: true, SetUp: true},
		customer: {ID: customer, IsSupplier: false, SetUp: false}}
	holder := func(id uuid.UUID, number string) partner.Found { return partner.Found{ID: id, Number: number} }
	for name, c := range map[string]struct {
		numbers   taxNumbers
		suppliers suppliers
		want      string // the supplier found, or the problem's code and a word of its detail
	}{
		"one supplier":          {taxNumbers{holders: []partner.Found{holder(one, "BP1"), holder(customer, "BP3")}}, suppliers{known: known}, "one"},
		"none":                  {taxNumbers{}, suppliers{known: known}, "payables.einvoice_seller_unknown no order"},
		"only a customer":       {taxNumbers{holders: []partner.Found{holder(customer, "BP3")}}, suppliers{known: known}, "payables.einvoice_seller_unknown no order"},
		"not a known partner":   {taxNumbers{holders: []partner.Found{holder(uuid.Nil, "BP9")}}, suppliers{known: known}, "payables.einvoice_seller_unknown no order"},
		"several":               {taxNumbers{holders: []partner.Found{holder(one, "BP1"), holder(two, "BP2")}}, suppliers{known: known}, "payables.einvoice_seller_ambiguous (BP1, BP2)"},
		"the partners failing":  {taxNumbers{err: down}, suppliers{known: known}, "down"},
		"the suppliers failing": {taxNumbers{holders: []partner.Found{holder(one, "BP1")}}, suppliers{err: down}, "down"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := (&Endpoints{TaxNumbers: c.numbers, Suppliers: c.suppliers}).sellerSupplier(context.Background(), uuid.Nil, "100555555500003")
			switch {
			case c.want == "down":
				if !errors.Is(err, down) {
					t.Errorf("got %v, want the partner's error", err)
				}
			case c.want == "one":
				if err != nil || got != one {
					t.Errorf("got %s (%v), want the one supplier %s", got, err, one)
				}
			default:
				code, word, _ := strings.Cut(c.want, " ")
				if p := problem.From(err); p.Code != code || !strings.Contains(p.Detail, word) || !strings.Contains(p.Detail, "ending 0003,") ||
					strings.Contains(p.Detail, "100555555500003") {
					t.Errorf("got %v, want %s saying %q, the TRN's last four only", err, code, word)
				}
			}
		})
	}
}

// TestRebateInput: a volume discount naming no order is a credit memo of
// the seller's supplier, each line an item of 1 EA on the account of
// discounts received, its amounts as stated (negative ones as their
// magnitude); a company without that account is
// payables.capture_needs_account; a document capture cannot read is
// payables.unreadable_document; the ledger or the partner context failing
// is its error (ADR-0061 As built batch 89).
func TestRebateInput(t *testing.T) {
	down := errors.New("ledger down")
	supplier, account := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	numbers := taxNumbers{holders: []partner.Found{{ID: supplier, Number: "BP1"}}}
	known := suppliers{known: map[uuid.UUID]partner.Supplier{supplier: {ID: supplier, IsSupplier: true, SetUp: true}}}
	note := func(change func(*EInvoice)) EInvoice {
		d := EInvoice{Kind: KindCreditMemo, Number: "VD-1", IssueDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), Currency: "AED", Exponent: 2,
			Reason: &CreditReason{Code: reasonVolumeDiscount}, SellerTRN: "100555555500003", Tax: 1250, Gross: 26250,
			Lines: []EInvoiceLine{{Description: "Volume discount", Quantity: "40", Net: 25000, Category: "S", Percent: "5"}}}
		if change != nil {
			change(&d)
		}
		return d
	}
	t.Run("entered", func(t *testing.T) {
		d := note(func(d *EInvoice) { d.Gross, d.Tax, d.Lines[0].Net = -26250, -1250, -25000 })
		ext, err := extractionOf(d)
		if err != nil {
			t.Fatal(err)
		}
		in, err := (&Endpoints{TaxNumbers: numbers, Suppliers: known, Accounts: determination{account: &account}}).rebateInput(context.Background(), d, ext)
		if err != nil {
			t.Fatal(err)
		}
		it := in.Items[0]
		if got, want := fmt.Sprintf("%s %s %s %s %s %v %s %s %s %v", in.Kind, in.SupplierReference, in.InvoiceDate.Format(time.DateOnly), in.Gross.Value,
			in.Tax.Value, in.PurchaseOrderID == nil, it.Quantity, it.Unit, it.Amount.Value, *it.AccountID == account),
			"credit_memo VD-1 2026-03-10 262.50 12.50 true 1 EA 250.00 true"; got != want || in.SupplierID != supplier {
			t.Errorf("got %s of %s, want %s of the supplier %s", got, in.SupplierID, want, supplier)
		}
	})
	for name, c := range map[string]struct {
		accounts determination
		change   func(*EInvoice)
		ext      func(*capture.Extraction)
		want     string
	}{
		"no account":            {determination{}, nil, nil, "payables.capture_needs_account"},
		"the ledger failing":    {determination{account: &account, err: down}, nil, nil, "down"},
		"the seller unknown":    {determination{account: &account}, func(d *EInvoice) { d.SellerTRN = "100999999900003" }, nil, "payables.einvoice_seller_unknown"},
		"no date":               {determination{account: &account}, nil, func(x *capture.Extraction) { x.InvoiceDate = "" }, "payables.unreadable_document"},
		"a gross amount unread": {determination{account: &account}, nil, func(x *capture.Extraction) { x.GrossAmount = "1.234" }, "payables.unreadable_document"},
		"no lines":              {determination{account: &account}, func(d *EInvoice) { d.Lines = nil }, nil, "payables.unreadable_document"},
		"a line amount unread":  {determination{account: &account}, nil, func(x *capture.Extraction) { x.Lines[0].Amount = "x" }, "payables.unreadable_document"},
		"amounts of both signs": {determination{account: &account}, func(d *EInvoice) { d.Lines[0].Net = -25000 }, nil, "payables.unreadable_document"},
	} {
		t.Run(name, func(t *testing.T) {
			d := note(c.change)
			ext, err := extractionOf(d)
			if err != nil {
				t.Fatal(err)
			}
			if c.ext != nil {
				c.ext(ext)
			}
			tn := numbers
			if d.SellerTRN != "100555555500003" {
				tn = taxNumbers{}
			}
			_, err = (&Endpoints{TaxNumbers: tn, Suppliers: known, Accounts: c.accounts}).rebateInput(context.Background(), d, ext)
			if c.want == "down" {
				if !errors.Is(err, down) {
					t.Errorf("got %v, want the ledger's error", err)
				}
				return
			}
			if p := problem.From(err); p.Code != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
}
