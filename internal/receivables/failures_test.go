package receivables_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *receivables.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Receivables(app.Config{Pages: pages})
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	c := e.CostCenters(t, outputCodes(""))
	h := endpoints(t)
	entered, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1")), "")
	id := uuid.MustParse(entered["id"].(string))
	create := &gen.CustomerInvoiceCreate{LegalEntityID: e.DE, CustomerID: e.customer, InvoiceDate: date(t, "2026-03-10"),
		PostingDate: gen.NewOptDate(date(t, "2026-03-11")), Currency: "EUR", PaymentTermCode: gen.NewOptString("NT14"), Items: []gen.CustomerInvoiceItemCreate{
			{AccountID: e.revenue, NetAmount: gen.Money{Amount: "100.00", Currency: "EUR"}, TaxCode: gen.NewOptString("A1"),
				CostCenterID: gen.NewOptUUID(c.Sales), ProfitCenterID: gen.NewOptUUID(c.Profit), Description: gen.NewOptString("Goods")},
			{AccountID: e.revenue, NetAmount: gen.Money{Amount: "5.00", Currency: "EUR"}}}}
	draft, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1")), "")
	post := gen.PostCustomerInvoiceParams{ID: uuid.MustParse(draft["id"].(string)), IfMatch: `"1"`}
	h.Now = func() time.Time { return date(t, "2026-03-15") }
	// A credit memo naming a posted invoice, to enter, to post and to read.
	credited := uuid.MustParse(e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12")["id"].(string))
	memo := uuid.MustParse(e.entered(t, e.memo(e.customer, credited.String(), "2026-03-13", e.item("10.00", "A1"))))
	createMemo := *create
	createMemo.Kind, createMemo.ReferenceInvoiceID = gen.NewOptString("credit_memo"), gen.NewOptUUID(credited)
	// A receipt to apply to an invoice, and one applied already.
	receipt, spent := e.receipt(t, 10000, "2026-03-14"), e.receipt(t, 5000, "2026-03-14")
	owed := gen.ClearingItemRef{FiscalYear: 2026, JournalEntryID: entryOf(t, e.invoiced(t, e.customer, "100.00", "2026-03-10")), LineNo: 1}
	applied, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, spent.ID, "2026-03-15", e.invoiced(t, e.customer, "50.00", "2026-03-10")), "")
	apply := func(receipt uuid.UUID, day gen.OptDate) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateIncomingPayment(ctx, &gen.IncomingPaymentCreate{LegalEntityID: e.DE, CustomerID: e.customer, ReceiptJournalEntryID: receipt,
				PostingDate: day, Items: []gen.ClearingItemRef{owed}}, gen.CreateIncomingPaymentParams{})
			return err
		}
	}
	calls := map[string]func(ctx context.Context) error{
		"apply a payment":                       apply(receipt.ID, gen.NewOptDate(date(t, "2026-03-15"))),
		"apply a payment on the company's date": apply(receipt.ID, gen.OptDate{}),
		"apply a receipt applied already": func(ctx context.Context) error {
			err := apply(spent.ID, gen.OptDate{})(ctx)
			if problem.From(err).Code == "receivables.receipt_already_applied" {
				return nil // every statement ran: the refusal is the call's answer
			}
			return err
		},
		"get a payment": func(ctx context.Context) error {
			_, err := h.GetIncomingPayment(ctx, gen.GetIncomingPaymentParams{ID: uuid.MustParse(applied["id"].(string))})
			return err
		},
		"list payments": func(ctx context.Context) error {
			_, err := h.ListIncomingPayments(ctx, gen.ListIncomingPaymentsParams{})
			return err
		},
		"list receipts": func(ctx context.Context) error {
			_, err := h.ListUnappliedReceipts(ctx, gen.ListUnappliedReceiptsParams{LegalEntityID: e.DE})
			return err
		},
		"post": func(ctx context.Context) error {
			_, err := h.PostCustomerInvoice(ctx, gen.NewOptCustomerInvoicePost(gen.CustomerInvoicePost{PostingDate: gen.NewOptDate(date(t, "2026-03-12"))}), post)
			return err
		},
		"post on the company's date": func(ctx context.Context) error {
			_, err := h.PostCustomerInvoice(ctx, gen.OptCustomerInvoicePost{}, post)
			return err
		},
		"create a credit memo": func(ctx context.Context) error {
			_, err := h.CreateCustomerInvoice(ctx, &createMemo, gen.CreateCustomerInvoiceParams{})
			return err
		},
		"post a credit memo": func(ctx context.Context) error {
			_, err := h.PostCustomerInvoice(ctx, gen.NewOptCustomerInvoicePost(gen.CustomerInvoicePost{PostingDate: gen.NewOptDate(date(t, "2026-03-14"))}),
				gen.PostCustomerInvoiceParams{ID: memo, IfMatch: `"1"`})
			return err
		},
		"get a credit memo": func(ctx context.Context) error {
			_, err := h.GetCustomerInvoice(ctx, gen.GetCustomerInvoiceParams{ID: memo})
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := h.CreateCustomerInvoice(ctx, create, gen.CreateCustomerInvoiceParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetCustomerInvoice(ctx, gen.GetCustomerInvoiceParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListCustomerInvoices(ctx, gen.ListCustomerInvoicesParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	for name, list := range map[string]func(ctx context.Context) error{
		"invoices": func(ctx context.Context) error {
			_, err := h.ListCustomerInvoices(ctx, gen.ListCustomerInvoicesParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"payments": func(ctx context.Context) error {
			_, err := h.ListIncomingPayments(ctx, gen.ListIncomingPaymentsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"receipts": func(ctx context.Context) error {
			_, err := h.ListUnappliedReceipts(ctx, gen.ListUnappliedReceiptsParams{LegalEntityID: e.DE, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run("list page size, "+name, func(t *testing.T) {
			if p := problem.From(e.inRequest(t, list)); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}

// badRate is a ledger whose tax codes carry a rate the calculator cannot
// read: what a code the ledger read on the date never is.
type badRate struct{ ledger.Accounts }

func (badRate) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	return ledger.TaxCode{Code: "A1", Direction: config.TaxOutput, Components: []ledger.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput,
		RecoverablePct: "100", RatePct: "a lot"}}}, true, nil
}

// noCompany is a configuration without the invoice's company.
type noCompany struct{}

func (noCompany) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{}, 1, nil
}

// TestEntryThroughThePort: what the contract's own validation keeps from
// the handler and what only a port could answer: an invoice without
// items is refused; a company the configuration lacks rounds by the
// default; a tax code the calculator cannot calculate fails the entry.
func TestEntryThroughThePort(t *testing.T) {
	e := setup(t)
	in := receivables.Input{LegalEntityID: e.DE, CustomerID: e.customer, InvoiceDate: date(t, "2026-03-10"), Currency: "EUR",
		Items: []receivables.ItemInput{{AccountID: e.revenue, Net: receivables.Amount{Value: "0.09", Currency: "EUR"}, TaxCode: ptr("A1")}}}
	t.Run("no items", func(t *testing.T) {
		none := in
		none.Items = nil
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t).CreateInvoice(ctx, none)
			return err
		})
		if p := problem.From(err); p.Code != "receivables.items_required" || p.Status != 422 {
			t.Errorf("got %v, want 422 receivables.items_required", err)
		}
	})
	t.Run("a company the configuration lacks", func(t *testing.T) {
		h := endpoints(t)
		h.Settings = noCompany{}
		err := e.inRequest(t, func(ctx context.Context) error {
			v, err := h.CreateInvoice(ctx, in)
			if err == nil && (v.Tax != 2 || v.Gross != 11) {
				t.Errorf("tax and gross: got %d and %d minor units, want 2 and 11", v.Tax, v.Gross)
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the invoice entered", err)
		}
	})
	t.Run("a code the calculator cannot calculate", func(t *testing.T) {
		h := endpoints(t)
		h.Accounts = badRate{}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.CreateInvoice(ctx, in)
			return err
		})
		var p *problem.Problem
		if err == nil || errors.As(err, &p) {
			t.Errorf("got %v, want an error that is no refusal", err)
		}
	})
}

func ptr[T any](v T) *T { return &v }
