package fixture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// DownPayments gives company 2000 its down payment account (159000, an
// asset that is a vendor reconciliation account) in the ledger, as an
// activated configuration with it would (ADR-0037).
func (e Env) DownPayments(tb testing.TB) {
	tb.Helper()
	doc := Document(tb)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}

// PaidDownPayment requests, as the tenant's human, a down payment of the
// supplier in company 2000 of amount EUR with tax code V1 and the
// reference, and pays it from bank account 100000 on 2026-03-05 as a
// payment run's release does (ledger.Postings.PayDownPayment, then the
// request marked paid): the supplier's open special G/L item A on line 1
// of the entry returned. DownPayments must have run.
func (e Env) PaidDownPayment(tb testing.TB, supplier uuid.UUID, amount, reference string) ledger.Entry {
	tb.Helper()
	r, _ := e.Must(201, "POST", "/v1/down-payment-requests", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "amount": {"amount": %q, "currency": "EUR"},
		"due_date": "2026-03-05", "tax_code": "V1", "reference": %q}`, e.DE, supplier, amount, reference), "")
	paid, err := money.ParseAmount(amount, 2)
	if err != nil {
		tb.Fatal(err)
	}
	tax, err := money.ParseAmount(r["tax_amount"].(map[string]any)["amount"].(string), 2)
	if err != nil {
		tb.Fatal(err)
	}
	code, ref := "V1", reference
	var entry ledger.Entry
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
		entry, err = post.PayDownPayment(ctx, ledger.DownPaymentPayment{LegalEntityID: e.DE, SupplierID: supplier, BankAccountID: e.Account(tb, "2000", "100000"),
			DocumentType: "SA", PostingDate: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), Currency: "EUR", Amount: paid, TaxCode: &code, Tax: tax, Reference: &ref,
			SourceDocumentID: uuid.Must(uuid.NewV7())})
		if err != nil {
			return err
		}
		return payables.DownPaymentRequests{}.Paid(ctx, uuid.MustParse(r["id"].(string)), entry.FiscalYear, entry.ID)
	}); err != nil {
		tb.Fatal(err)
	}
	return entry
}
