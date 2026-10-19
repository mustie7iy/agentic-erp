package payables_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestClearDownPaymentsRefusalsAndFailures: down payments are cleared only
// against a posted invoice, and every database statement of a clearing
// failing in turn is reported (T46d).
func TestClearDownPaymentsRefusalsAndFailures(t *testing.T) {
	e := setup(t)
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	r, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Supplier, "100.00", "EUR", ""), "")
	var entry ledger.Entry
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		if err := (ledger.Setup{LegalEntities: tenancy.Directory{}}).Apply(ctx, doc); err != nil {
			return err
		}
		entry, err = endpoints(nil).Postings.(ledger.Postings).PayDownPayment(ctx, ledger.DownPaymentPayment{LegalEntityID: e.DE,
			SupplierID: e.Supplier, BankAccountID: e.Account(t, "2000", "100000"), DocumentType: "SA", PostingDate: date("2026-03-16"),
			Currency: "EUR", Amount: 10000, SourceDocumentID: uuid.New()})
		if err != nil {
			return err
		}
		return payables.DownPaymentRequests{}.Paid(ctx, uuid.MustParse(id(r)), entry.FiscalYear, entry.ID)
	}); err != nil {
		t.Fatal(err)
	}
	parked, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-1", "200.00", "0.00", accountItem(e.expense, "200.00")), "")
	body := `{"posting_date": "2026-03-20", "items": [{"fiscal_year": 2026, "journal_entry_id": "` + entry.ID.String() + `", "line_no": 1}]}`
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(parked)+":clearDownPayments", body, `"1"`)
	e.act(t, 200, id(parked), "post", `{"posting_date": "2026-03-21"}`)
	h := endpoints(nil)
	call := func(ctx context.Context) error {
		_, err := h.ClearSupplierInvoiceDownPayments(ctx, &gen.DownPaymentClearingInput{PostingDate: date("2026-03-22"),
			Items: []gen.ClearingItemRef{{FiscalYear: int32(entry.FiscalYear), JournalEntryID: entry.ID, LineNo: 1}}},
			gen.ClearSupplierInvoiceDownPaymentsParams{ID: uuid.MustParse(id(parked)), IfMatch: "*"})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "clear down payments", call)
}
