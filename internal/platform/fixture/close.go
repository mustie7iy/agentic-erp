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
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ClosePermissions are the permissions of the accountant who closes
// periods (T32) and revalues foreign currency at their end (ADR-0042):
// reading, opening, closing and locking periods and asking for their
// unlock, reading the ledgers and accounts, and reading and posting
// entries.
var ClosePermissions = []string{"ledger.period.read", "ledger.period.manage", "ledger.period.lock", "ledger.account.read", "ledger.entry.read",
	"ledger.entry.post"}

// Parallel gives the configuration a parallel ledger 2L (HGB) that
// company 2000 keeps beside the leading 0L, with a group of both, for
// NewWith (ADR-0045).
func Parallel(c *config.Config) {
	c.Ledgers = []config.Ledger{{Code: config.LeadingLedger, Name: "Leading ledger", AccountingPrinciple: "IFRS"},
		{Code: "2L", Name: "HGB", AccountingPrinciple: "HGB"}}
	c.LedgerGroups = []config.LedgerGroup{{Code: "ALL", Name: "Every ledger", Representative: config.LeadingLedger, Ledgers: []string{config.LeadingLedger, "2L"}}}
	c.Company("2000").Ledgers = []string{config.LeadingLedger, "2L"}
}

// ThreeLedgers gives the configuration three ledgers that company 2000
// keeps, for NewWith, created in the order 1L, 0L, 0A: their ids, and the
// rows of their periods, sort 1L, 0L, 0A, their codes 0A, 0L, 1L, so a
// statement that locks their periods in the order of its rows or of the
// ledgers' ids takes neither the one order nor the codes' (ADR-0058 §5).
func ThreeLedgers(c *config.Config) {
	c.Ledgers = []config.Ledger{{Code: "1L", Name: "Tax ledger", AccountingPrinciple: "TAX"},
		{Code: config.LeadingLedger, Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "0A", Name: "Statutory", AccountingPrinciple: "HGB"}}
	c.LedgerGroups = []config.LedgerGroup{{Code: "ALL", Name: "Every ledger", Representative: config.LeadingLedger,
		Ledgers: []string{"1L", config.LeadingLedger, "0A"}}}
	c.Company("2000").Ledgers = []string{"1L", config.LeadingLedger, "0A"}
}

// LockOrder is the one order in which every statement locks the periods
// of ThreeLedgers' ledgers within a fiscal year and period: the leading
// ledger first, then by code (ADR-0058 §5, ADR-0008).
var LockOrder = []string{config.LeadingLedger, "0A", "1L"}

// Ledger is the id of a ledger of the tenant by its code.
func (e Env) Ledger(tb testing.TB, code string) uuid.UUID {
	tb.Helper()
	var id uuid.UUID
	pgtest.Scan(tb, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND code = $2`, []any{e.Tenant.ID, code}, &id)
	return id
}

// LedgerOnly posts, as the tenant's human, EUR amount from revenue
// account 400000 to bank account 100000 of company 2000 on the day, in
// the ledger alone: a ledger-specific posting, which its reconciliation
// with the other ledgers shows as a difference (ADR-0045). The tenant
// needs ledger.entry.post.
func (e Env) LedgerOnly(tb testing.TB, ledgerID uuid.UUID, day, amount string) {
	tb.Helper()
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "document_date": %q, "posting_date": %q,
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}, {"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}}]}`,
		e.DE, ledgerID, day, day, e.Account(tb, "2000", "100000"), amount, e.Account(tb, "2000", "400000"), amount), "")
}

// Revalues gives company 2000 the foreign-currency valuation of ADR-0042
// as an activated configuration would: its payables reconciliation
// account 211000 revalued at bank_selling rates against adjustment
// account 211099 and unrealised gain 400000 and loss 610000. USD is worth
// 0.8 at the average rate from 1 January 2026 and 0.9 at bank_selling
// from 15 March; a USD supplier item of 100.00 is posted on 2 March, so
// a revaluation on 31 March values a loss of EUR 10.00. The tenant's
// configuration document is Document changed by extend, as NewWith's was.
func (e Env) Revalues(tb testing.TB, extend ...func(*config.Config)) {
	tb.Helper()
	doc := Document(tb)
	for _, x := range extend {
		x(doc)
	}
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "211099", Name: "Payables valuation", Type: config.Liability})
	de.FXValuation = config.FXValuation{RateType: "bank_selling", Accounts: []config.FXValuationAccount{{Account: "211000", Adjustment: "211099",
		UnrealisedGain: "400000", UnrealisedLoss: "610000"}}}
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'bank_selling', 'USD', 'EUR', '2026-03-15', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "USD",
			SourceContext: "payables", SourceDocumentID: uuid.Must(uuid.NewV7()),
			Lines: []ledger.PostingLine{{AccountID: e.Account(tb, "2000", "520000"), Amount: 10000},
				{AccountID: e.Account(tb, "2000", "211000"), Amount: -10000, PartnerID: &e.Supplier}}})
		return err
	}); err != nil {
		tb.Fatal(err)
	}
}

// GRIROpen records, in company 2000, an open GR/IR position as receipts
// and an invoice posted through GR/IR would leave it (T33): item 1 of a
// purchase order received 10 EA and invoiced 4, a balance of EUR -72.00
// last posted on 5 March 2026. It returns the order's id.
func (e Env) GRIROpen(tb testing.TB) uuid.UUID {
	tb.Helper()
	po := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id, currency_tc, currency_lc,
		currency_gc, uom_code, received, invoiced, amount_tc, amount_lc, amount_gc, last_posting_date)
		VALUES ($1, $2, $3, 1, $4, 'EUR', 'EUR', 'EUR', 'EA', 10, 4, -7200, -7200, -7200, '2026-03-05')`, e.Tenant.ID, e.DE, po, e.Account(tb, "2000", "520000"))
	return po
}

// GRIRReceived posts, as the tenant's human, what a goods movement of
// item 1 of the order posts through GR/IR in company 2000 on the day (T33):
// qty EA at EUR 12.00 against expense account 520000, signed as the GR/IR
// line (negative received, positive returned). Company 2000 needs a GR/IR
// account (GRIRAccounts).
func (e Env) GRIRReceived(tb testing.TB, order uuid.UUID, day, qty string) {
	tb.Helper()
	on, err := time.Parse(time.DateOnly, day)
	if err != nil {
		tb.Fatal(err)
	}
	expense := e.Account(tb, "2000", "520000")
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: on, PostingDate: on, Currency: "EUR",
			SourceContext: "procurement", SourceDocumentID: uuid.Must(uuid.NewV7()),
			GRIR: []ledger.GRIRItem{{PurchaseOrderID: order, POItemNo: 1, Quantity: qty, Unit: "EA", UnitPrice: 1200, PriceUnit: 1, OffsetAccountID: &expense}}})
		return err
	}); err != nil {
		tb.Fatal(err)
	}
}
