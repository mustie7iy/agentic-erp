package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	bank, revenue, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "211000")
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	pgtest.Exec(t, `INSERT INTO ledger.tax_code (tenant_id, legal_entity_id, code, description, direction, rate_pct, account_id, valid_from, created_by)
		VALUES ($1, $2, 'V9', 'Input', 'input', 19, $3, '2026-01-01', $4)`, e.ten.ID, e.de, e.account(t, "2000", "140000"), e.ten.Human.ID)
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	inv, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}), "")
	pay, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{ap, "5.00", e.supplier.String()}, line{bank, "-5.00", ""}), "")
	cl, _ := e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{inv, 2}, ref{pay, 1}), "")
	inv2, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}), "")
	pay2, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{ap, "5.00", e.supplier.String()}, line{bank, "-5.00", ""}), "")
	rate, _ := e.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "GBP", "to_currency": "EUR", "valid_from": "2026-01-01", "rate": "1.1", "source": "x"}`, "")
	periods, _ := e.Must(200, "GET", "/v1/fiscal-periods?legal_entity_id="+e.de.String()+"&order_by=period&page_size=1", "", "")
	period := uuid.MustParse(periods["items"].([]any)[0].(map[string]any)["id"].(string))
	id := func(m map[string]any) uuid.UUID { return uuid.MustParse(m["id"].(string)) }
	u := func(s string) uuid.UUID { return uuid.MustParse(s) }
	qty := gen.NewOptQuantity(gen.Quantity{Value: "1", Unit: "EA"})
	setup := ledger.Setup{LegalEntities: tenancy.Directory{}}

	calls := map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"),
				Currency: "USD", Lines: []gen.JournalLineInput{
					{AccountID: u(bank), Amount: gen.Money{Amount: "1.00", Currency: "USD"}, TaxCode: gen.NewOptString("V9"), Quantity: qty},
					{AccountID: u(ap), Amount: gen.Money{Amount: "-1.00", Currency: "USD"}, PartnerID: gen.NewOptUUID(e.supplier)}}}, gen.PostJournalEntryParams{})
			return err
		},
		"get entry": func(ctx context.Context) error {
			_, err := h.GetJournalEntry(ctx, gen.GetJournalEntryParams{ID: id(je)})
			return err
		},
		"list entries": func(ctx context.Context) error {
			_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{}, gen.ReverseJournalEntryParams{ID: id(je), IfMatch: "*"})
			return err
		},
		"open items": func(ctx context.Context) error {
			_, err := h.ListOpenItems(ctx, gen.ListOpenItemsParams{})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := h.ListBalances(ctx, gen.ListBalancesParams{LegalEntityID: e.de, FiscalYear: 2026})
			return err
		},
		"create clearing": func(ctx context.Context) error {
			_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{
				{FiscalYear: 2026, JournalEntryID: id(inv2), LineNo: 2}, {FiscalYear: 2026, JournalEntryID: id(pay2), LineNo: 1}}}, gen.CreateClearingParams{})
			return err
		},
		"get clearing": func(ctx context.Context) error {
			_, err := h.GetClearing(ctx, gen.GetClearingParams{ID: id(cl)})
			return err
		},
		"list clearings": func(ctx context.Context) error {
			_, err := h.ListClearings(ctx, gen.ListClearingsParams{})
			return err
		},
		"create rate": func(ctx context.Context) error {
			_, err := h.CreateExchangeRate(ctx, &gen.ExchangeRateCreate{RateType: "average", FromCurrency: "CHF", ToCurrency: "EUR", ValidFrom: date("2026-01-01"),
				Rate: "1.05", Source: "x"}, gen.CreateExchangeRateParams{})
			return err
		},
		"get rate": func(ctx context.Context) error {
			_, err := h.GetExchangeRate(ctx, gen.GetExchangeRateParams{ID: id(rate)})
			return err
		},
		"list rates": func(ctx context.Context) error {
			_, err := h.ListExchangeRates(ctx, gen.ListExchangeRatesParams{})
			return err
		},
		"get period": func(ctx context.Context) error {
			_, err := h.GetFiscalPeriod(ctx, gen.GetFiscalPeriodParams{ID: period})
			return err
		},
		"list periods": func(ctx context.Context) error {
			_, err := h.ListFiscalPeriods(ctx, gen.ListFiscalPeriodsParams{})
			return err
		},
		"close period": func(ctx context.Context) error {
			_, err := h.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: period, IfMatch: "*"})
			return err
		},
		"get account": func(ctx context.Context) error {
			_, err := h.GetAccount(ctx, gen.GetAccountParams{ID: u(bank)})
			return err
		},
		"list accounts": func(ctx context.Context) error {
			_, err := h.ListAccounts(ctx, gen.ListAccountsParams{})
			return err
		},
		"setup check": func(ctx context.Context) error {
			_, err := setup.Check(ctx, document(t), document(t))
			return err
		},
		"setup apply": func(ctx context.Context) error { return setup.Apply(ctx, document(t)) },
		"reconciliation account": func(ctx context.Context) error {
			_, err := ledger.Accounts{}.IsReconciliationAccount(ctx, e.de, u(ap), "supplier")
			return err
		},
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
	}
	// The opening transition fails like closing; it needs a closed period.
	apitest.EveryFailure(t, e.ten, e.ten.Human, "open period", func(ctx context.Context) error {
		if _, err := h.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: period, IfMatch: "*"}); err != nil {
			return err
		}
		_, err := h.OpenFiscalPeriod(ctx, gen.OpenFiscalPeriodParams{ID: period, IfMatch: "*"})
		return err
	})
	if _, err := h.OpenFiscalPeriod(context.Background(), gen.OpenFiscalPeriodParams{ID: period, IfMatch: "*"}); err == nil {
		t.Error("open period outside a unit of work")
	}

	// Handlers check page sizes and list rules for callers other than the generated router.
	zero := gen.NewOptInt32(0)
	for name, call := range map[string]func(ctx context.Context) error{
		"entries": func(ctx context.Context) error {
			_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{PageSize: zero})
			return err
		},
		"open items": func(ctx context.Context) error {
			_, err := h.ListOpenItems(ctx, gen.ListOpenItemsParams{PageSize: zero})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := h.ListBalances(ctx, gen.ListBalancesParams{LegalEntityID: e.de, FiscalYear: 2026, PageSize: zero})
			return err
		},
		"clearings": func(ctx context.Context) error {
			_, err := h.ListClearings(ctx, gen.ListClearingsParams{PageSize: zero})
			return err
		},
		"rates": func(ctx context.Context) error {
			_, err := h.ListExchangeRates(ctx, gen.ListExchangeRatesParams{PageSize: zero})
			return err
		},
		"periods": func(ctx context.Context) error {
			_, err := h.ListFiscalPeriods(ctx, gen.ListFiscalPeriodsParams{PageSize: zero})
			return err
		},
		"accounts": func(ctx context.Context) error {
			_, err := h.ListAccounts(ctx, gen.ListAccountsParams{PageSize: zero})
			return err
		},
		"duplicate clearing item": func(ctx context.Context) error {
			ref := gen.ClearingItemRef{FiscalYear: 2026, JournalEntryID: id(inv2), LineNo: 2}
			_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{ref, ref}},
				gen.CreateClearingParams{})
			return err
		},
	} {
		if err := apitest.InRequest(t, e.ten, e.ten.Human, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConstraintProblems(t *testing.T) {
	for name, code := range map[string]string{"reversal_pkey": "ledger.already_reversed", "clearing_item_pkey": "ledger.item_already_cleared"} {
		if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: name}); p.Code != code || p.Status != 409 {
			t.Errorf("%s: %v", name, p)
		}
	}
}
