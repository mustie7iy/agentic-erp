package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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
	parked, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-1", "200.00", "0.00", untaxedItem(e.expense, "200.00")), "")
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

// TestClearDownPaymentsCountsEachRequestOnce: the down payments chosen
// are found by their payment entry, so two lines of one entry name the
// same request, which counts once in the invoice's gross check and the
// cleared total; a choice that is not exactly a request's down payments
// is refused, the transfer rolled back (review:batch39). No public path
// posts these shapes (a run posts one entry per request with one down
// payment, ADR-0037), so the entries are posted here directly.
func TestClearDownPaymentsCountsEachRequestOnce(t *testing.T) {
	e := setup(t)
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return (ledger.Setup{LegalEntities: tenancy.Directory{}}).Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		requests []string // amounts of the requests the entry pays
		lines    []int64  // the entry's down payment lines, in cents
		chosen   []int32  // the lines chosen
		status   int
		code     string
		cleared  int64 // the requests' total cleared against the invoice
	}{
		{name: "one request on two lines, both chosen", requests: []string{"100.00"}, lines: []int64{6000, 4000}, chosen: []int32{1, 2}, status: 200, cleared: 10000},
		{name: "one of a request's two lines", requests: []string{"100.00"}, lines: []int64{6000, 4000}, chosen: []int32{1}, status: 422,
			code: "payables.not_a_down_payment_request"},
		{name: "an entry paying two requests", requests: []string{"60.00", "40.00"}, lines: []int64{6000, 4000}, chosen: []int32{1, 2}, status: 422,
			code: "payables.not_a_down_payment_request"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			entry := e.paidOnOneEntry(t, c.requests, c.lines)
			// 150.00: the request counted once per line (200.00) would exceed it.
			parked, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, fmt.Sprintf("DP-ONCE-%d", i), "150.00", "0.00",
				untaxedItem(e.expense, "150.00")), "")
			e.act(t, 200, id(parked), "post", `{"posting_date": "2026-03-21"}`)
			refs := make([]string, len(c.chosen))
			for j, n := range c.chosen {
				refs[j] = fmt.Sprintf(`{"fiscal_year": %d, "journal_entry_id": %q, "line_no": %d}`, entry.FiscalYear, entry.ID, n)
			}
			body := `{"posting_date": "2026-03-22", "items": [` + strings.Join(refs, ", ") + `]}`
			if c.status == 200 {
				e.act(t, 200, id(parked), "clearDownPayments", body)
			} else {
				e.Problem(c.status, c.code, "POST", "/v1/supplier-invoices/"+id(parked)+":clearDownPayments", body, e.tag(t, id(parked)))
			}
			var n, cleared int64
			pgtest.Scan(t, `SELECT count(*), coalesce(sum(amount), 0) FROM payables.down_payment_request WHERE cleared_invoice_id = $1`,
				[]any{id(parked)}, &n, &cleared)
			if cleared != c.cleared {
				t.Errorf("cleared against the invoice: got %d, want %d", cleared, c.cleared)
			}
			if want := map[bool]int64{true: int64(len(c.requests)), false: 0}[c.status == 200]; n != want {
				t.Errorf("requests cleared: got %d, want %d", n, want)
			}
			var transfers int
			pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE source_context = 'payables' AND source_document_id = $1 AND document_type = 'AB'`,
				[]any{id(parked)}, &transfers)
			if want := map[bool]int{true: 1, false: 0}[c.status == 200]; transfers != want {
				t.Errorf("transfers posted: got %d, want %d", transfers, want)
			}
		})
	}
}

// paidOnOneEntry requests the down payments and records them all as paid
// by one entry holding the lines, each the supplier's special G/L item A,
// against the bank: a shape no run posts (ADR-0037).
func (e env) paidOnOneEntry(t *testing.T, amounts []string, lines []int64) ledger.Entry {
	t.Helper()
	ids := make([]uuid.UUID, len(amounts))
	for i, a := range amounts {
		r, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Supplier, a, "EUR", ""), "")
		ids[i] = uuid.MustParse(id(r))
	}
	special := ledger.SpecialDownPayment
	var posting []ledger.PostingLine
	var bank int64
	for _, l := range lines {
		posting = append(posting, ledger.PostingLine{AccountID: e.Account(t, "2000", "159000"), Amount: money.Minor(l), PartnerID: &e.Supplier, SpecialGL: &special})
		bank += l
	}
	posting = append(posting, ledger.PostingLine{AccountID: e.Account(t, "2000", "100000"), Amount: money.Minor(-bank)})
	var entry ledger.Entry
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		entry, err = endpoints(nil).Postings.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date("2026-03-16"),
			PostingDate: date("2026-03-16"), Currency: "EUR", SourceContext: "payments", SourceDocumentID: uuid.New(), Lines: posting})
		if err != nil {
			return err
		}
		for _, r := range ids {
			if err := (payables.DownPaymentRequests{}).Paid(ctx, r, entry.FiscalYear, entry.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}
