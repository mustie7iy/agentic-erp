package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// migratedDownPayment is a down payment of 119.00 with 19.00 input tax on
// V1 (19%) that SAP posted on 2026-03-10 for the supplier in legal entity
// 2000, in the given entry.
func (e env) migratedDownPayment(entry uuid.UUID) payables.MigratedDownPayment {
	code := "V1"
	return payables.MigratedDownPayment{LegalEntityID: e.DE, SupplierID: e.Supplier, Currency: "EUR", Amount: 11900, TaxCode: &code, Tax: 1900,
		PostingDate: date("2026-03-10"), Reference: "SAP 2000/2026/0100000009", EntryYear: 2026, EntryID: entry}
}

// TestTakeOverDownPayment: a migrated down payment is recorded as a paid
// down payment request naming the entry that posted it (T72c), numbered
// from the request range, listed and read as any request, audited, and
// published as nothing, since nothing was requested or paid here.
func TestTakeOverDownPayment(t *testing.T) {
	e := setup(t)
	entry := uuid.New()
	var r payables.DownPaymentRequest
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		r, err = endpoints(nil).TakeOverDownPayment(ctx, e.migratedDownPayment(entry))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r.Status != payables.RequestPaid {
		t.Errorf("status: got %s, want paid", r.Status)
	}
	if r.JournalEntryID == nil || *r.JournalEntryID != entry {
		t.Errorf("journal entry: got %v, want %s", r.JournalEntryID, entry)
	}
	if !strings.HasPrefix(r.DocumentNumber, "DP") {
		t.Errorf("document number: got %s, want one of the DP range", r.DocumentNumber)
	}
	got, _ := e.Must(200, "GET", "/v1/down-payment-requests?status=paid&supplier_id="+e.Supplier.String(), "", "")
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("paid requests of the supplier: got %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	for field, want := range map[string]string{"reference": "SAP 2000/2026/0100000009", "due_date": "2026-03-10", "tax_code": "V1",
		"amount": "map[amount:119.00 currency:EUR]", "tax_amount": "map[amount:19.00 currency:EUR]", "status": "paid", "journal_entry_id": entry.String()} {
		if got := fmt.Sprint(item[field]); got != want {
			t.Errorf("listed %s: got %s, want %s", field, got, want)
		}
	}
	var actions int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'payables.down_payment_request.take_over' AND object_id = $2`,
		[]any{e.Tenant.ID, r.ID}, &actions)
	if actions != 1 {
		t.Errorf("take-over audit entries: got %d, want 1", actions)
	}
	for _, name := range []string{"down_payment_requested", "down_payment_request_paid"} {
		if n := len(events(t, e.Tenant, name)); n != 0 {
			t.Errorf("%s events: got %d, want 0", name, n)
		}
	}
}

// TestTakeOverDownPaymentRefusals: a migrated down payment is checked as
// a request is, on the amounts the source system posted.
func TestTakeOverDownPaymentRefusals(t *testing.T) {
	e := setup(t)
	e.TaxCodes(t, config.TaxCode{Code: "A1", Description: "Output tax", Direction: config.TaxOutput, RatePercent: "19", Account: "140000"}, twoParts)
	code := func(c string) *string { return &c }
	cases := map[string]struct {
		change func(*payables.MigratedDownPayment)
		code   string
	}{
		"unknown legal entity":    {func(m *payables.MigratedDownPayment) { m.LegalEntityID = uuid.New() }, "request.reference_not_found"},
		"unknown supplier":        {func(m *payables.MigratedDownPayment) { m.SupplierID = uuid.New() }, "request.reference_not_found"},
		"unknown currency":        {func(m *payables.MigratedDownPayment) { m.Currency = "XXQ" }, "payables.unknown_currency"},
		"blank reference":         {func(m *payables.MigratedDownPayment) { m.Reference = " " }, "payables.invalid_reference"},
		"long reference":          {func(m *payables.MigratedDownPayment) { m.Reference = strings.Repeat("x", 101) }, "payables.invalid_reference"},
		"zero":                    {func(m *payables.MigratedDownPayment) { m.Amount, m.Tax = 0, 0 }, "payables.invalid_amount"},
		"negative tax":            {func(m *payables.MigratedDownPayment) { m.Tax = -1 }, "payables.invalid_tax_amount"},
		"tax of the whole amount": {func(m *payables.MigratedDownPayment) { m.Tax = m.Amount }, "payables.invalid_tax_amount"},
		"tax without a code":      {func(m *payables.MigratedDownPayment) { m.TaxCode = nil }, "payables.invalid_tax_amount"},
		"unknown tax code":        {func(m *payables.MigratedDownPayment) { m.TaxCode = code("V7") }, "payables.unknown_tax_code"},
		"output tax code":         {func(m *payables.MigratedDownPayment) { m.TaxCode = code("A1") }, "payables.unknown_tax_code"},
		"code with components":    {func(m *payables.MigratedDownPayment) { m.TaxCode = code("V2") }, "payables.tax_code_components"},
		"no fiscal year":          {func(m *payables.MigratedDownPayment) { m.PostingDate, m.TaxCode, m.Tax = date("2031-03-10"), nil, 0 }, "ledger.no_fiscal_period"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := e.migratedDownPayment(uuid.New())
			c.change(&in)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := endpoints(nil).TakeOverDownPayment(ctx, in)
				return err
			})
			pgtest.WantErr(t, err, c.code)
		})
	}
	in := e.migratedDownPayment(uuid.New())
	in.TaxCode, in.Tax = nil, 0
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		r, err := endpoints(nil).TakeOverDownPayment(ctx, in)
		if err == nil && (r.TaxCode != nil || r.TaxAmount != money.Minor(0)) {
			t.Errorf("untaxed down payment: got tax code %v and tax %d, want none", r.TaxCode, r.TaxAmount)
		}
		return err
	}); err != nil {
		t.Errorf("untaxed down payment: %v", err)
	}
}

// TestTakeOverDownPaymentDatabaseFailures takes a down payment over outside
// a unit of work and with each of its database statements failing in turn.
func TestTakeOverDownPaymentDatabaseFailures(t *testing.T) {
	e := setup(t)
	call := func(ctx context.Context) error {
		_, err := endpoints(nil).TakeOverDownPayment(ctx, e.migratedDownPayment(uuid.New()))
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "take over a down payment", call)
}
