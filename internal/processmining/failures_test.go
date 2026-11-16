package processmining_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/processmining"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestEventLogDatabaseFailures reads a log, as JSON and as CSV, outside a
// unit of work and with each of its database statements failing in turn;
// each port's read needs a unit of work too.
func TestEventLogDatabaseFailures(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 2, "2026-03-02", 3, 1_000_000)
	account := e.Account(t, "2000", "520000")
	e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "parked", "EUR", "2026-03-05", fixture.InvoiceItem{Net: 100, Account: &account})
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ep := app.ProcessMining(app.Config{Pages: pages})
	from, _ := time.Parse(time.DateOnly, "2026-03-01") // the constants parse
	to, _ := time.Parse(time.DateOnly, "2026-03-31")
	for _, format := range []string{"json", "csv"} {
		call := func(ctx context.Context) error {
			_, err := ep.GetProcessEventLog(ctx, gen.GetProcessEventLogParams{Process: "purchase_to_pay", LegalEntityID: e.DE, From: from, To: to,
				Format: gen.NewOptString(format), PageSize: gen.NewOptInt32(2)})
			return err
		}
		t.Run(format, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "a log read as "+format, call)
		})
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ep.Read(ctx, processmining.Query{Process: processmining.PurchaseToPay, LegalEntity: e.DE, From: from, To: to, SizeSet: true})
		return err
	}); err == nil || !strings.Contains(err.Error(), "page") {
		t.Errorf("a page of no case: got %v, want it refused", err)
	}
	bg := context.Background()
	for name, call := range map[string]func() error{
		"orders": func() error {
			_, err := (procurement.ProcessLog{}).Orders(bg, e.DE, from, to, nil, uuid.Nil, 1)
			return err
		},
		"orders counted": func() error { _, err := (procurement.ProcessLog{}).CountOrders(bg, e.DE, from, to); return err },
		"documents of":   func() error { _, err := (payables.ProcessLog{}).OfOrders(bg, nil); return err },
		"documents alone": func() error {
			_, err := (payables.ProcessLog{}).WithoutOrder(bg, e.DE, from, to, nil, uuid.Nil, 1)
			return err
		},
		"documents counted": func() error { _, err := (payables.ProcessLog{}).CountWithoutOrder(bg, e.DE, from, to); return err },
		"payments":          func() error { _, err := (payments.ProcessLog{}).Paying(bg, nil, nil); return err },
		"the trail":         func() error { _, err := (audit.Trail{}).About(bg, nil, nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
		})
	}
}
