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
