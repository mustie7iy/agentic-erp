package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestSettlePort pays open items the way payments does: one entry that
// offsets the items, credits the bank and clears the items.
func TestSettlePort(t *testing.T) {
	s := &settleScenario{e: setup(t)}
	if !t.Run("rates and invoices", s.ratesAndInvoices) {
		t.FailNow()
	}
	if !t.Run("open items, a settlement and its refusals", s.openItems) {
		t.FailNow()
	}
	if !t.Run("a gain at a lower rate", s.gain) {
		t.FailNow()
	}
	if !t.Run("a loss at a higher rate and overflowing rates", s.loss) {
		t.FailNow()
	}
	t.Run("outside a unit of work and failing statements", s.outsideAUnitOfWork)
}

// settleScenario is TestSettlePort's scenario: the EUR, GBP and USD
// invoices the first step posts.
type settleScenario struct {
	e             env
	eur, gbp, usd ledger.ItemRef
}

// ratesAndInvoices adds the rates and posts the three invoices.
func (s *settleScenario) ratesAndInvoices(t *testing.T) {
	e := s.e.in(t)
	settleRate(t, e, "USD", "2026-03-01", "0.90")
	settleRate(t, e, "USD", "2026-04-01", "0.80")
	settleRate(t, e, "GBP", "2026-03-01", "1.20")
	s.eur = settlePost(t, e, e.de, "2000", "EUR", 10000, "INV-1")
	s.gbp = settlePost(t, e, e.de, "2000", "GBP", 5000, "INV-2")
	s.usd = settlePost(t, e, e.us, "1000", "USD", 10000, "INV-3")
}

// gain pays at 0.80 what was booked at 0.90.
func (s *settleScenario) gain(t *testing.T) {
	e, usd := s.e.in(t), s.usd
	// Paid at 0.80 what was booked at 0.90: a gain in group currency.
	gain := settleAccount(t, e, "1000", "400000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
		e.ten.ID, e.us, gain, e.ten.Human.ID)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		entry, err := postings().Settle(ctx, settlementOf(t, e, e.us, "1000", "USD", "2026-04-15", usd))
		if err != nil {
			return err
		}
		if len(entry.Lines) != 3 {
			t.Errorf("gain: got %d lines %+v, want 3", len(entry.Lines), entry.Lines)
			return nil
		}
		if entry.Lines[2].AccountID != gain {
			t.Errorf("gain account: got %s, want %s", entry.Lines[2].AccountID, gain)
		}
		if entry.Lines[2].TC != 0 {
			t.Errorf("gain amount: got %d, want 0", entry.Lines[2].TC)
		}
		if entry.Lines[2].GC != -1000 {
			t.Errorf("gain group amount: got %d, want -1000", entry.Lines[2].GC)
		}
		if entry.Lines[1].GC != -8000 {
			t.Errorf("bank group amount: got %d, want -8000", entry.Lines[1].GC)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// loss pays at 1.00 what was booked at 0.90, and at rates that overflow.
func (s *settleScenario) loss(t *testing.T) {
	e := s.e.in(t)
	// Paid at 1.00 what was booked at 0.90: a loss; at a rate that
	// overflows the amount in local or group currency: refused.
	settleRate(t, e, "USD", "2026-05-01", "1.00")
	settleRate(t, e, "USD", "2026-06-01", "1000000")
	settleRate(t, e, "GBP", "2026-06-01", "1000000")
	loss := settleAccount(t, e, "1000", "520000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
		e.ten.ID, e.us, loss, e.ten.Human.ID)
	dear, bigUSD, bigGBP := settlePost(t, e, e.us, "1000", "USD", 10000, "INV-4"), settlePost(t, e, e.us, "1000", "USD", 10_000_000_000_000, "INV-5"),
		settlePost(t, e, e.de, "2000", "GBP", 10_000_000_000_000, "INV-6")
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		entry, err := postings().Settle(ctx, settlementOf(t, e, e.us, "1000", "USD", "2026-05-15", dear))
		switch {
		case err != nil:
			t.Errorf("loss: %v", err)
		case len(entry.Lines) != 3:
			t.Errorf("loss: got %d lines %+v, want 3", len(entry.Lines), entry.Lines)
		default:
			if entry.Lines[2].AccountID != loss {
				t.Errorf("loss account: got %s, want %s", entry.Lines[2].AccountID, loss)
			}
			if entry.Lines[2].GC != 1000 {
				t.Errorf("loss group amount: got %d, want 1000", entry.Lines[2].GC)
			}
		}
		if _, err := postings().Settle(ctx, settlementOf(t, e, e.us, "1000", "USD", "2026-06-15", bigUSD)); err == nil {
			t.Error("group amount overflowed")
		}
		if _, err := postings().Settle(ctx, settlementOf(t, e, e.de, "2000", "GBP", "2026-06-15", bigGBP)); err == nil {
			t.Error("local amount overflowed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// outsideAUnitOfWork calls the ports outside a unit of work and with each
// statement failing.
func (s *settleScenario) outsideAUnitOfWork(t *testing.T) {
	e, eur, usd := s.e.in(t), s.eur, s.usd
	for name, call := range map[string]func(ctx context.Context) error{
		"payable": func(ctx context.Context) error {
			_, err := ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-12-31"))
			return err
		},
		"references": func(ctx context.Context) error {
			_, err := ledger.OpenItems{}.References(ctx, []uuid.UUID{eur.EntryID})
			return err
		},
		"settle": func(ctx context.Context) error {
			_, err := postings().Settle(ctx, settlementOf(t, e, e.us, "1000", "USD", "2026-04-15", usd))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}

// settleAccount is the id of an account of a company's chart.
func settleAccount(t *testing.T, e env, company, number string) uuid.UUID {
	t.Helper()
	return uuid.MustParse(e.in(t).account(t, company, number))
}

// settleRate adds an average rate to EUR.
func settleRate(t *testing.T, e env, from, validFrom, rate string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', $2, 'EUR', $3, $4, 'ECB', $5)`, e.ten.ID, from, validFrom, rate, e.ten.Human.ID)
}

// settlePost posts a supplier invoice and returns its open item.
func settlePost(t *testing.T, e env, le uuid.UUID, company, currency string, amount money.Minor, ref string) ledger.ItemRef {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: le, DocumentType: "SA", DocumentDate: date("2026-03-01"),
			PostingDate: date("2026-03-01"), Currency: currency, Reference: &ref, SourceContext: "payables", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: settleAccount(t, e, company, "520000"), Amount: amount}, {AccountID: settleAccount(t, e, company, "211000"), Amount: -amount,
				PartnerID: &e.supplier}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: 2}
}

// settlementOf is a payment of the items from the company's bank account.
func settlementOf(t *testing.T, e env, le uuid.UUID, company, currency, day string, items ...ledger.ItemRef) ledger.Settlement {
	t.Helper()
	text := "Payment"
	return ledger.Settlement{LegalEntityID: le, DocumentType: "SA", PostingDate: date(day), Currency: currency, BankAccountID: settleAccount(t, e, company, "100000"),
		HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.New(), Items: items}
}

// openItems is TestSettlePort's first settlement: the open items and
// references it reads, the EUR item it settles, and the settlements it
// refuses.
func (s *settleScenario) openItems(t *testing.T) {
	e, eur, gbp, usd := s.e.in(t), s.eur, s.gbp, s.usd
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		items, err := ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-03-01"))
		switch {
		case err != nil:
			t.Errorf("payable: %v", err)
		case len(items) != 1:
			t.Errorf("payable: got %d items %+v, want 1", len(items), items)
		default:
			if items[0].ItemRef != eur {
				t.Errorf("payable item: got %+v, want %+v", items[0].ItemRef, eur)
			}
			if items[0].Amount != -10000 {
				t.Errorf("payable amount: got %d, want -10000", items[0].Amount)
			}
			if items[0].PartnerID != e.supplier {
				t.Errorf("payable partner: got %s, want %s", items[0].PartnerID, e.supplier)
			}
		}
		if items, _ := (ledger.OpenItems{}).Payable(ctx, e.de, "EUR", date("2026-02-28")); len(items) != 0 {
			t.Errorf("documents after the date: %+v", items)
		}
		refs, err := ledger.OpenItems{}.References(ctx, []uuid.UUID{eur.EntryID, usd.EntryID})
		if err != nil {
			t.Errorf("references: %v", err)
		}
		if refs[eur.EntryID] != "INV-1" {
			t.Errorf("reference of the EUR item: got %q, want INV-1", refs[eur.EntryID])
		}
		if refs[usd.EntryID] != "INV-3" {
			t.Errorf("reference of the USD item: got %q, want INV-3", refs[usd.EntryID])
		}
		entry, err := postings().Settle(ctx, settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", eur))
		switch {
		case err != nil:
			t.Errorf("settled: %v", err)
		case len(entry.Lines) != 2:
			t.Errorf("settled: got %d lines %+v, want 2", len(entry.Lines), entry.Lines)
		default:
			if entry.DocumentType != "SA" {
				t.Errorf("settled document type: got %s, want SA", entry.DocumentType)
			}
			if entry.Lines[0].TC != 10000 {
				t.Errorf("settled line 1: got %d, want 10000", entry.Lines[0].TC)
			}
			if entry.Lines[1].TC != -10000 {
				t.Errorf("settled line 2: got %d, want -10000", entry.Lines[1].TC)
			}
		}
		if items, _ := (ledger.OpenItems{}).Payable(ctx, e.de, "EUR", date("2026-12-31")); len(items) != 0 {
			t.Errorf("open after settling: %+v", items)
		}
		cases := []struct {
			name string
			in   ledger.Settlement
			code string
		}{
			{"no items", settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15"), "ledger.no_items"},
			{"unknown item", settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.New(), LineNo: 1}), "ledger.not_an_open_item"},
			{"other legal entity", settlementOf(t, e, e.de, "2000", "USD", "2026-04-15", usd), "ledger.not_an_open_item"},
			{"cleared", settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", eur), "ledger.item_already_cleared"},
			{"other currency", settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", gbp), "ledger.mixed_items"},
			{"no local rate", settlementOf(t, e, e.de, "2000", "GBP", "2026-02-15", gbp), "ledger.no_exchange_rate"},
			{"no group rate", settlementOf(t, e, e.us, "1000", "USD", "2026-02-15", usd), "ledger.no_exchange_rate"},
			{"no fx account", settlementOf(t, e, e.us, "1000", "USD", "2026-04-15", usd), "ledger.no_fx_account"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if _, err := postings().Settle(ctx, c.in); problem.From(err).Code != c.code {
					t.Errorf("got %v, want %s", err, c.code)
				}
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigrationPorts clears migrated items by an existing entry and reads
// the totals reconciliation compares.
func TestMigrationPorts(t *testing.T) {
	e := setup(t)
	account := func(number string) uuid.UUID { return uuid.MustParse(e.account(t, "2000", number)) }
	post := func(amount money.Minor, partner *uuid.UUID) ledger.Entry {
		t.Helper()
		var entry ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-01"), Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
				Lines: []ledger.PostingLine{{AccountID: account("100000"), Amount: amount}, {AccountID: account("211000"), Amount: -amount, PartnerID: partner}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	invoice, payment, other := post(10000, &e.supplier), post(-10000, &e.supplier), post(500, &e.vendor)
	item := func(en ledger.Entry) ledger.ItemRef {
		return ledger.ItemRef{FiscalYear: en.FiscalYear, EntryID: en.ID, LineNo: 2}
	}
	clear := func(items ...ledger.ItemRef) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := postings().Clear(ctx, e.de, date("2026-03-20"), item(payment), items)
			return err
		}
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		chart, err := ledger.Accounts{}.Chart(ctx, e.de)
		if err != nil {
			t.Errorf("chart: %v", err)
		}
		if got := chart["211000"].ReconciliationKind; got != "supplier" {
			t.Errorf("211000 reconciliation kind: got %s, want supplier", got)
		}
		if got := chart["100000"].Type; got != "asset" {
			t.Errorf("100000 type: got %s, want asset", got)
		}
		totals, err := ledger.Reports{}.Totals(ctx, e.de, 2026)
		if err != nil {
			t.Errorf("totals: %v", err)
		}
		if got := totals.ByAccount["211000"]; got != -500 {
			t.Errorf("211000 total: got %d, want -500", got)
		}
		if totals.Debits != 20500 {
			t.Errorf("debits: got %d, want 20500", totals.Debits)
		}
		open, err := ledger.Reports{}.OpenPayables(ctx, e.de, 2026)
		if err != nil {
			t.Errorf("open payables: %v", err)
		}
		if got := open[e.supplier]; got != 0 {
			t.Errorf("open payables of the supplier: got %d, want 0", got)
		}
		if got := open[e.vendor]; got != -500 {
			t.Errorf("open payables of the vendor: got %d, want -500", got)
		}
		for name, c := range map[string]struct {
			items []ledger.ItemRef
			code  string
		}{
			"unknown":    {[]ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.New(), LineNo: 1}}, "ledger.not_an_open_item"},
			"unbalanced": {[]ledger.ItemRef{item(invoice), item(other)}, "ledger.clearing_not_balanced"},
		} {
			t.Run(name, func(t *testing.T) {
				if err := clear(c.items...)(ctx); problem.From(err).Code != c.code {
					t.Errorf("got %v, want %s", err, c.code)
				}
			})
		}
		if err := clear(item(invoice), item(payment))(ctx); err != nil {
			return err
		}
		if err := clear(item(invoice))(ctx); problem.From(err).Code != "ledger.item_already_cleared" {
			t.Errorf("cleared twice: %v", err)
		}
		open, _ = ledger.Reports{}.OpenPayables(ctx, e.de, 2026)
		if _, ok := open[e.supplier]; ok {
			t.Errorf("cleared items still open: %v", open)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"chart":         func(ctx context.Context) error { _, err := ledger.Accounts{}.Chart(ctx, e.de); return err },
		"totals":        func(ctx context.Context) error { _, err := ledger.Reports{}.Totals(ctx, e.de, 2026); return err },
		"open payables": func(ctx context.Context) error { _, err := ledger.Reports{}.OpenPayables(ctx, e.de, 2026); return err },
		"clear":         clear(item(invoice), item(payment)),
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}
