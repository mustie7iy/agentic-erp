package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestSettlePort pays open items the way payments does: one entry that
// offsets the items, credits the bank and clears the items.
func TestSettlePort(t *testing.T) {
	e := setup(t)
	account := func(company, number string) uuid.UUID { return uuid.MustParse(e.account(t, company, number)) }
	rate := func(from, validFrom, rate string) {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', $2, 'EUR', $3, $4, 'ECB', $5)`, e.ten.ID, from, validFrom, rate, e.ten.Human.ID)
	}
	rate("USD", "2026-03-01", "0.90")
	rate("USD", "2026-04-01", "0.80")
	rate("GBP", "2026-03-01", "1.20")
	post := func(le uuid.UUID, company, currency string, amount int64, ref string) ledger.ItemRef {
		t.Helper()
		var entry ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: le, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-01"), Currency: currency, Reference: &ref, SourceContext: "payables", SourceDocumentID: uuid.New(),
				Lines: []ledger.PostingLine{{AccountID: account(company, "520000"), Amount: amount}, {AccountID: account(company, "211000"), Amount: -amount,
					PartnerID: &e.supplier}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: 2}
	}
	eur := post(e.de, "2000", "EUR", 10000, "INV-1")
	gbp := post(e.de, "2000", "GBP", 5000, "INV-2")
	usd := post(e.us, "1000", "USD", 10000, "INV-3")
	settlement := func(le uuid.UUID, company, currency, day string, items ...ledger.ItemRef) ledger.Settlement {
		text := "Payment"
		return ledger.Settlement{LegalEntityID: le, DocumentType: "SA", PostingDate: date(day), Currency: currency, BankAccountID: account(company, "100000"),
			HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.New(), Items: items}
	}

	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		items, err := ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-03-01"))
		if err != nil || len(items) != 1 || items[0].ItemRef != eur || items[0].Amount != -10000 || items[0].PartnerID != e.supplier {
			t.Errorf("payable: %+v %v", items, err)
		}
		if items, _ := (ledger.OpenItems{}).Payable(ctx, e.de, "EUR", date("2026-02-28")); len(items) != 0 {
			t.Errorf("documents after the date: %+v", items)
		}
		refs, err := ledger.OpenItems{}.References(ctx, []uuid.UUID{eur.EntryID, usd.EntryID})
		if err != nil || refs[eur.EntryID] != "INV-1" || refs[usd.EntryID] != "INV-3" {
			t.Errorf("references: %v %v", refs, err)
		}
		entry, err := postings().Settle(ctx, settlement(e.de, "2000", "EUR", "2026-04-15", eur))
		if err != nil || entry.DocumentType != "SA" || len(entry.Lines) != 2 || entry.Lines[0].TC != 10000 || entry.Lines[1].TC != -10000 {
			t.Errorf("settled: %+v %v", entry, err)
		}
		if items, _ := (ledger.OpenItems{}).Payable(ctx, e.de, "EUR", date("2026-12-31")); len(items) != 0 {
			t.Errorf("open after settling: %+v", items)
		}
		cases := []struct {
			name string
			in   ledger.Settlement
			code string
		}{
			{"no items", settlement(e.de, "2000", "EUR", "2026-04-15"), "ledger.no_items"},
			{"unknown item", settlement(e.de, "2000", "EUR", "2026-04-15", ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.New(), LineNo: 1}), "ledger.not_an_open_item"},
			{"other legal entity", settlement(e.de, "2000", "USD", "2026-04-15", usd), "ledger.not_an_open_item"},
			{"cleared", settlement(e.de, "2000", "EUR", "2026-04-15", eur), "ledger.item_already_cleared"},
			{"other currency", settlement(e.de, "2000", "EUR", "2026-04-15", gbp), "ledger.mixed_items"},
			{"no local rate", settlement(e.de, "2000", "GBP", "2026-02-15", gbp), "ledger.no_exchange_rate"},
			{"no group rate", settlement(e.us, "1000", "USD", "2026-02-15", usd), "ledger.no_exchange_rate"},
			{"no fx account", settlement(e.us, "1000", "USD", "2026-04-15", usd), "ledger.no_fx_account"},
		}
		for _, c := range cases {
			if _, err := postings().Settle(ctx, c.in); problem.From(err).Code != c.code {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Paid at 0.80 what was booked at 0.90: a gain in group currency.
	gain := account("1000", "400000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
		e.ten.ID, e.us, gain, e.ten.Human.ID)
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		entry, err := postings().Settle(ctx, settlement(e.us, "1000", "USD", "2026-04-15", usd))
		if err == nil && (len(entry.Lines) != 3 || entry.Lines[2].AccountID != gain || entry.Lines[2].TC != 0 || entry.Lines[2].GC != -1000 ||
			entry.Lines[1].GC != -8000) {
			t.Errorf("gain: %+v", entry.Lines)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Paid at 1.00 what was booked at 0.90: a loss; at a rate that
	// overflows the amount in local or group currency: refused.
	rate("USD", "2026-05-01", "1.00")
	rate("USD", "2026-06-01", "1000000")
	rate("GBP", "2026-06-01", "1000000")
	loss := account("1000", "520000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
		e.ten.ID, e.us, loss, e.ten.Human.ID)
	dear, bigUSD, bigGBP := post(e.us, "1000", "USD", 10000, "INV-4"), post(e.us, "1000", "USD", 10_000_000_000_000, "INV-5"),
		post(e.de, "2000", "GBP", 10_000_000_000_000, "INV-6")
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		entry, err := postings().Settle(ctx, settlement(e.us, "1000", "USD", "2026-05-15", dear))
		if err != nil || len(entry.Lines) != 3 || entry.Lines[2].AccountID != loss || entry.Lines[2].GC != 1000 {
			t.Errorf("loss: %+v %v", entry.Lines, err)
		}
		if _, err := postings().Settle(ctx, settlement(e.us, "1000", "USD", "2026-06-15", bigUSD)); err == nil {
			t.Error("group amount overflowed")
		}
		if _, err := postings().Settle(ctx, settlement(e.de, "2000", "GBP", "2026-06-15", bigGBP)); err == nil {
			t.Error("local amount overflowed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

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
			_, err := postings().Settle(ctx, settlement(e.us, "1000", "USD", "2026-04-15", usd))
			return err
		},
	} {
		if call(context.Background()) == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
	}
}

// TestMigrationPorts clears migrated items by an existing entry and reads
// the totals reconciliation compares.
func TestMigrationPorts(t *testing.T) {
	e := setup(t)
	account := func(number string) uuid.UUID { return uuid.MustParse(e.account(t, "2000", number)) }
	post := func(amount int64, partner *uuid.UUID) ledger.Entry {
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
		if err != nil || chart["211000"].ReconciliationKind != "supplier" || chart["100000"].Type != "asset" {
			t.Errorf("chart: %v %v", chart, err)
		}
		totals, err := ledger.Reports{}.Totals(ctx, e.de, 2026)
		if err != nil || totals.ByAccount["211000"] != -500 || totals.Debits != 20500 {
			t.Errorf("totals: %+v %v", totals, err)
		}
		open, err := ledger.Reports{}.OpenPayables(ctx, e.de, 2026)
		if err != nil || open[e.supplier] != 0 || open[e.vendor] != -500 {
			t.Errorf("open payables: %v %v", open, err)
		}
		for name, c := range map[string]struct {
			items []ledger.ItemRef
			code  string
		}{
			"unknown":    {[]ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.New(), LineNo: 1}}, "ledger.not_an_open_item"},
			"unbalanced": {[]ledger.ItemRef{item(invoice), item(other)}, "ledger.clearing_not_balanced"},
		} {
			if err := clear(c.items...)(ctx); problem.From(err).Code != c.code {
				t.Errorf("%s: %v", name, err)
			}
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
		if call(context.Background()) == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
	}
}
