package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
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
	d := &ledgerDocs{e: e}
	if !t.Run("documents to call on", d.post) {
		t.FailNow()
	}
	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		for name, call := range ledgerCalls(t, e, h, *d) {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got no error, want one")
				}
				apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
			})
		}
	})
	t.Run("opening a period", func(t *testing.T) {
		// The opening transition fails like closing; it needs a closed period.
		apitest.EveryFailure(t, e.ten, e.ten.Human, "open period", func(ctx context.Context) error {
			if _, err := h.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err != nil {
				return err
			}
			_, err := h.OpenFiscalPeriod(ctx, gen.OpenFiscalPeriodParams{ID: d.period, IfMatch: "*"})
			return err
		})
		if _, err := h.OpenFiscalPeriod(context.Background(), gen.OpenFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err == nil {
			t.Error("open period outside a unit of work")
		}
	})
	t.Run("locking a period", func(t *testing.T) {
		// Locking fails like closing; it needs a closed period.
		apitest.EveryFailure(t, e.ten, e.ten.Human, "lock period", func(ctx context.Context) error {
			if _, err := h.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err != nil {
				return err
			}
			_, err := h.LockFiscalPeriod(ctx, gen.LockFiscalPeriodParams{ID: d.period, IfMatch: "*"})
			return err
		})
		if _, err := h.LockFiscalPeriod(context.Background(), gen.LockFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err == nil {
			t.Error("lock period outside a unit of work")
		}
	})
	t.Run("unlocking a period", func(t *testing.T) {
		// Each call closes and locks the period, asks for its unlock and
		// ends the request; every step fails like closing.
		request := uuid.New()
		unlock := func(ctx context.Context) error {
			if _, err := h.CloseFiscalPeriod(ctx, gen.CloseFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err != nil {
				return err
			}
			if _, err := h.LockFiscalPeriod(ctx, gen.LockFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err != nil {
				return err
			}
			u := *h
			u.Approvals = opener{request, true}
			_, err := u.UnlockFiscalPeriod(ctx, &gen.ReasonInput{Reason: "r"}, gen.UnlockFiscalPeriodParams{ID: d.period, IfMatch: "*"})
			return err
		}
		for name, end := range map[string]func(ctx context.Context) error{
			"reject unlock":  func(ctx context.Context) error { return ledger.PeriodUnlocks{}.Rejected(ctx, d.period, request, "c") },
			"approve unlock": func(ctx context.Context) error { return ledger.PeriodUnlocks{}.Approved(ctx, d.period, request) },
		} {
			if err := end(context.Background()); err == nil {
				t.Errorf("%s outside a unit of work: got no error, want one", name)
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, func(ctx context.Context) error {
				if err := unlock(ctx); err != nil {
					return err
				}
				return end(ctx)
			})
		}
		if _, err := h.UnlockFiscalPeriod(context.Background(), &gen.ReasonInput{Reason: "r"}, gen.UnlockFiscalPeriodParams{ID: d.period, IfMatch: "*"}); err == nil {
			t.Error("unlock period outside a unit of work")
		}
	})
	t.Run("posting a parked entry", func(t *testing.T) {
		// Each call posts the parked entry directly, or submits it and ends
		// its request; every step fails like parking.
		request := uuid.New()
		post := func(needed bool) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				p := *h
				p.Approvals = opener{request, needed}
				_, err := p.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: d.id(d.parked), IfMatch: "*"})
				return err
			}
		}
		m := manualEntries()
		for name, call := range map[string]func(ctx context.Context) error{
			"post parked":   post(false),
			"submit parked": post(true),
			"approve entry": func(ctx context.Context) error { return m.Approved(ctx, d.id(d.parked), request) },
			"reject entry":  func(ctx context.Context) error { return m.Rejected(ctx, d.id(d.parked), request, "c") },
			"return entry":  func(ctx context.Context) error { return m.Cancelled(ctx, d.id(d.parked), request, "c") },
		} {
			if err := call(context.Background()); err == nil {
				t.Errorf("%s outside a unit of work: got no error, want one", name)
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, func(ctx context.Context) error {
				if name == "post parked" || name == "submit parked" {
					return call(ctx)
				}
				if err := post(true)(ctx); err != nil {
					return err
				}
				return call(ctx)
			})
		}
	})
	t.Run("page sizes and list rules", func(t *testing.T) {
		// Handlers check page sizes and list rules for callers other than the generated router.
		zero := gen.NewOptInt32(0)
		for name, call := range map[string]func(ctx context.Context) error{
			"entries": func(ctx context.Context) error {
				_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{PageSize: zero})
				return err
			},
			"parked entries": func(ctx context.Context) error {
				_, err := h.ListParkedJournalEntries(ctx, gen.ListParkedJournalEntriesParams{PageSize: zero})
				return err
			},
			"open items": func(ctx context.Context) error {
				_, err := h.ListOpenItems(ctx, gen.ListOpenItemsParams{PageSize: zero})
				return err
			},
			"GR/IR balances": func(ctx context.Context) error {
				_, err := h.ListGrIrBalances(ctx, gen.ListGrIrBalancesParams{LegalEntityID: e.de, PageSize: zero})
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
			"ledgers": func(ctx context.Context) error {
				_, err := h.ListLedgers(ctx, gen.ListLedgersParams{PageSize: zero})
				return err
			},
			"ledger groups": func(ctx context.Context) error {
				_, err := h.ListLedgerGroups(ctx, gen.ListLedgerGroupsParams{PageSize: zero})
				return err
			},
			// A caller other than the API, whose contract admits only
			// decimals, can pass anything: refused, not a nil dereference.
			"rate not a decimal": func(ctx context.Context) error {
				_, err := ledger.CreateRate(ctx, ledger.NewRate{RateType: "average", FromCurrency: "USD", ToCurrency: "GBP", ValidFrom: date("2026-01-01"),
					Rate: "x", Source: "x"})
				return err
			},
			"duplicate clearing item": func(ctx context.Context) error {
				ref := gen.ClearingItemRef{FiscalYear: 2026, JournalEntryID: d.id(d.inv2), LineNo: 2}
				_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{ref, ref}},
					gen.CreateClearingParams{})
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				if err := apitest.InRequest(t, e.ten, e.ten.Human, call); err == nil {
					t.Error("got no error, want the call refused")
				}
			})
		}
	})
}

// ledgerDocs are the documents TestDatabaseFailures calls the handlers on.
type ledgerDocs struct {
	e                              env
	bank, ap                       string
	je, inv2, pay2, inv3, cl, rate map[string]any
	parked                         map[string]any
	period                         uuid.UUID
}

func (ledgerDocs) id(m map[string]any) uuid.UUID { return uuid.MustParse(m["id"].(string)) }

// post posts entries, a clearing and a rate, and finds a period.
func (d *ledgerDocs) post(t *testing.T) {
	e := d.e.in(t)
	var revenue string
	d.bank, revenue, d.ap = e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "211000")
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	fixture.TaxCodes(t, e.ten, document(t), config.TaxCode{Code: "V9", Description: "Input", RatePercent: "19", Account: "140000"})
	bank, ap := d.bank, d.ap
	d.je, _ = e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	inv, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}), "")
	pay, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{ap, "5.00", e.supplier.String()}, line{bank, "-5.00", ""}), "")
	d.cl, _ = e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{inv, 2}, ref{pay, 1}), "")
	d.inv2, _ = e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}), "")
	d.pay2, _ = e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{ap, "5.00", e.supplier.String()}, line{bank, "-5.00", ""}), "")
	d.inv3, _ = e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}), "")
	d.parked, _ = e.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	d.rate, _ = e.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "GBP", "to_currency": "EUR", "valid_from": "2026-01-01", "rate": "1.1", "source": "x"}`, "")
	periods, _ := e.Must(200, "GET", "/v1/fiscal-periods?legal_entity_id="+e.de.String()+"&order_by=period&page_size=1", "", "")
	d.period = uuid.MustParse(periods["items"].([]any)[0].(map[string]any)["id"].(string))
}

// ledgerCalls are the handlers and context functions TestDatabaseFailures
// calls outside a unit of work and with each statement failing.
func ledgerCalls(t *testing.T, e env, h *ledger.Endpoints, d ledgerDocs) map[string]func(ctx context.Context) error {
	t.Helper()
	id, u := d.id, uuid.MustParse
	bank, ap, period := d.bank, d.ap, d.period
	qty := gen.NewOptQuantity(gen.Quantity{Value: "1", Unit: "EA"})
	setup := ledger.Setup{LegalEntities: tenancy.Directory{}}
	return map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"),
				Currency: "USD", Lines: []gen.JournalLineInput{
					{AccountID: u(bank), Amount: gen.Money{Amount: "1.00", Currency: "USD"}, TaxCode: gen.NewOptString("V9"), Quantity: qty},
					{AccountID: u(ap), Amount: gen.Money{Amount: "-1.00", Currency: "USD"}, PartnerID: gen.NewOptUUID(e.supplier)}}}, gen.PostJournalEntryParams{})
			return err
		},
		"park": func(ctx context.Context) error {
			_, err := h.ParkJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"),
				Currency: "USD", Lines: []gen.JournalLineInput{
					{AccountID: u(bank), Amount: gen.Money{Amount: "1.00", Currency: "USD"}, TaxCode: gen.NewOptString("V9"), Quantity: qty},
					{AccountID: u(ap), Amount: gen.Money{Amount: "-1.00", Currency: "USD"}, PartnerID: gen.NewOptUUID(e.supplier)}}}, gen.ParkJournalEntryParams{})
			return err
		},
		"get parked": func(ctx context.Context) error {
			_, err := h.GetParkedJournalEntry(ctx, gen.GetParkedJournalEntryParams{ID: id(d.parked)})
			return err
		},
		"change parked": func(ctx context.Context) error {
			_, err := h.UpdateParkedJournalEntry(ctx, &gen.ParkedJournalEntryUpdate{HeaderText: gen.NewOptNilString("x"), Lines: []gen.JournalLineInput{
				{AccountID: u(bank), Amount: gen.Money{Amount: "2.00", Currency: "EUR"}},
				{AccountID: u(ap), Amount: gen.Money{Amount: "-2.00", Currency: "EUR"}, PartnerID: gen.NewOptUUID(e.supplier)}}},
				gen.UpdateParkedJournalEntryParams{ID: id(d.parked), IfMatch: "*"})
			return err
		},
		"cancel parked": func(ctx context.Context) error {
			_, err := h.CancelParkedJournalEntry(ctx, &gen.ReasonInput{Reason: "r"}, gen.CancelParkedJournalEntryParams{ID: id(d.parked), IfMatch: "*"})
			return err
		},
		"list parked": func(ctx context.Context) error {
			_, err := h.ListParkedJournalEntries(ctx, gen.ListParkedJournalEntriesParams{})
			return err
		},
		"get entry": func(ctx context.Context) error {
			_, err := h.GetJournalEntry(ctx, gen.GetJournalEntryParams{ID: id(d.je)})
			return err
		},
		"list entries": func(ctx context.Context) error {
			_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{}, gen.ReverseJournalEntryParams{ID: id(d.je), IfMatch: "*"})
			return err
		},
		// The reversal of an entry with open items also clears the pair.
		"reverse an invoice": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{}, gen.ReverseJournalEntryParams{ID: id(d.inv3), IfMatch: "*"})
			return err
		},
		"open items": func(ctx context.Context) error {
			_, err := h.ListOpenItems(ctx, gen.ListOpenItemsParams{})
			return err
		},
		"GR/IR balances": func(ctx context.Context) error {
			_, err := h.ListGrIrBalances(ctx, gen.ListGrIrBalancesParams{LegalEntityID: e.de})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := h.ListBalances(ctx, gen.ListBalancesParams{LegalEntityID: e.de, FiscalYear: 2026})
			return err
		},
		"create clearing": func(ctx context.Context) error {
			_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{
				{FiscalYear: 2026, JournalEntryID: id(d.inv2), LineNo: 2}, {FiscalYear: 2026, JournalEntryID: id(d.pay2), LineNo: 1}}}, gen.CreateClearingParams{})
			return err
		},
		"get clearing": func(ctx context.Context) error {
			_, err := h.GetClearing(ctx, gen.GetClearingParams{ID: id(d.cl)})
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
			_, err := h.GetExchangeRate(ctx, gen.GetExchangeRateParams{ID: id(d.rate)})
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
		"setup check GR/IR": func(ctx context.Context) error {
			_, err := setup.Check(ctx, document(t), withGRIR(t))
			return err
		},
		"setup check open items": func(ctx context.Context) error {
			_, err := setup.Check(ctx, document(t), withOpenItems(t))
			return err
		},
		"setup apply":            func(ctx context.Context) error { return setup.Apply(ctx, document(t)) },
		"setup apply open items": func(ctx context.Context) error { return setup.Apply(ctx, with102000OpenItems(t)) },
		"setup check parallel ledgers": func(ctx context.Context) error {
			_, err := setup.Check(ctx, parallel(t), parallel(t))
			return err
		},
		"setup apply parallel ledgers": func(ctx context.Context) error { return setup.Apply(ctx, parallel(t)) },
		"list ledgers": func(ctx context.Context) error {
			_, err := h.ListLedgers(ctx, gen.ListLedgersParams{})
			return err
		},
		"list ledger groups": func(ctx context.Context) error {
			_, err := h.ListLedgerGroups(ctx, gen.ListLedgerGroupsParams{})
			return err
		},
		"reconciliation account": func(ctx context.Context) error {
			_, err := ledger.Accounts{}.IsReconciliationAccount(ctx, e.de, u(ap), "supplier")
			return err
		},
	}
}

func TestConstraintProblems(t *testing.T) {
	for name, code := range map[string]string{"reversal_pkey": "ledger.already_reversed", "clearing_item_pkey": "ledger.item_already_cleared",
		"item_reserved_once": "ledger.item_reserved", "account_determination_gr_ir_fixed": "ledger.gr_ir_account_fixed",
		"account_determination_down_payment_fixed": "ledger.down_payment_account_fixed"} {
		t.Run(name, func(t *testing.T) {
			p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: name})
			if p.Code != code {
				t.Errorf("code: got %s, want %s", p.Code, code)
			}
			if p.Status != 409 {
				t.Errorf("status: got %d, want 409", p.Status)
			}
		})
	}
}
