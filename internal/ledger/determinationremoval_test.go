package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// determinationKeys lists company 2000's account determination as
// "key=account number", in key order.
func (e env) determinationKeys(tb testing.TB) string {
	tb.Helper()
	var out string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT coalesce(string_agg(d.key || '=' || g.number, ', ' ORDER BY d.key), '')
		FROM ledger.account_determination d JOIN ledger.gl_account g ON g.id = d.account_id WHERE d.legal_entity_id = $1`, e.de).Scan(&out); err != nil {
		tb.Fatalf("list the account determination: %v", err)
	}
	return out
}

// determined is the account Accounts.Determination gives company 2000's
// key, "none" when there is none.
func (e env) determined(tb testing.TB, key string) string {
	tb.Helper()
	var got string
	if err := apitest.InRequest(tb, e.ten, e.ten.Human, func(ctx context.Context) error {
		id, ok, err := ledger.Accounts{}.Determination(ctx, e.de, key)
		got = map[bool]string{true: id.String(), false: "none"}[ok]
		return err
	}); err != nil {
		tb.Fatal(err)
	}
	return got
}

// withKeys is the test document with company 2000's discount and
// payment difference keys, and valuation class 3000's stock and
// consumption accounts, as set.
func withKeys(tb testing.TB, granted, expense, stock string) *config.Config {
	tb.Helper()
	doc := document(tb)
	ad := &doc.Companies[1].AccountDetermination
	ad.CashDiscountGranted, ad.PaymentDifferenceExpense = granted, expense
	if stock != "" {
		ad.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: stock, Consumption: "510000"}}
	}
	return doc
}

// TestDeterminationKeysRemoved (ADR-0048 As built batch 93): a key the
// document no longer names is removed when it is activated, each removal
// recorded with the account it named, so cash_discount_granted falls back
// to payment_difference_expense's account again and a valuation class's
// accounts are no longer determined; applying the same document again
// removes nothing more, and a key named again is set again. The keys the
// ledger never writes are left alone.
func TestDeterminationKeysRemoved(t *testing.T) {
	e := setup(t)
	apply := func(doc *config.Config) {
		t.Helper()
		if err := e.applyTax(t, doc); err != nil {
			t.Fatal(err)
		}
	}
	legacy := e.account(t, "2000", "540000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'rounding', $3, $4)`,
		e.ten.ID, e.de, legacy, e.ten.Human.ID)
	apply(withKeys(t, "520000", "530000", "101000"))
	const all = "ap_reconciliation=211000, bank=100000, cash_discount_granted=520000, input_tax=140000, inventory_consumption:3000=510000, " +
		"inventory_stock:3000=101000, payment_difference_expense=530000, rounding=540000"
	if got := e.determinationKeys(t); got != all {
		t.Fatalf("every key named: got %s, want %s", got, all)
	}
	apply(withKeys(t, "", "530000", ""))
	const left = "ap_reconciliation=211000, bank=100000, input_tax=140000, payment_difference_expense=530000, rounding=540000"
	if got := e.determinationKeys(t); got != left {
		t.Errorf("after the removal: got %s, want %s", got, left)
	}
	if got, want := e.determined(t, "cash_discount_granted"), e.account(t, "2000", "530000"); got != want {
		t.Errorf("cash_discount_granted after its removal: got %s, want payment_difference_expense's %s", got, want)
	}
	if got := e.determined(t, config.InventoryKey(config.InventoryStock, "3000")); got != "none" {
		t.Errorf("class 3000's stock account after its removal: got %s, want none", got)
	}
	var removals string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT string_agg(r.key || '=' || g.number || ' by ' || (r.removed_by = $2)::text, ', ' ORDER BY r.key)
		FROM ledger.account_determination_removal r JOIN ledger.gl_account g ON g.id = r.account_id WHERE r.legal_entity_id = $1`,
		e.de, e.ten.Human.ID).Scan(&removals); err != nil {
		t.Fatal(err)
	}
	if want := "cash_discount_granted=520000 by true, inventory_consumption:3000=510000 by true, inventory_stock:3000=101000 by true"; removals != want {
		t.Errorf("removals recorded: got %s, want %s", removals, want)
	}
	apply(withKeys(t, "", "530000", "")) // nothing left to remove
	apply(withKeys(t, "610000", "530000", ""))
	if got, want := e.determined(t, "cash_discount_granted"), e.account(t, "2000", "610000"); got != want {
		t.Errorf("cash_discount_granted named again: got %s, want %s", got, want)
	}
}

// TestDeterminationKeyPostedKept (ADR-0048 As built batch 93): no key
// of a valuation class whose stock account has postings is removed, as a
// reversal of the class's material documents determines its accounts
// again, the price difference account too, though nothing was posted to
// it; nor does that stock account change, so it keeps every posting of
// the class across activations. The submission check reports each, and
// the activation is refused with 409 ledger.determination_key_posted, the
// keys kept. A class without postings changes its accounts and goes.
func TestDeterminationKeyPostedKept(t *testing.T) {
	e := setup(t)
	classes := func(stock string, priceDifference bool, unposted string) *config.Config {
		doc := withKeys(t, "", "530000", "")
		inv := config.InventoryAccounts{ValuationClass: "3000", Stock: stock, Consumption: "510000"}
		if priceDifference {
			inv.PriceDifference = "540000"
		}
		doc.Companies[1].AccountDetermination.Inventory = []config.InventoryAccounts{inv}
		if unposted != "" {
			doc.Companies[1].AccountDetermination.Inventory = append(doc.Companies[1].AccountDetermination.Inventory,
				config.InventoryAccounts{ValuationClass: "3001", Stock: unposted})
		}
		return doc
	}
	before := classes("101000", true, "610000")
	if err := e.applyTax(t, before); err != nil {
		t.Fatal(err)
	}
	stock, bank := uuid.MustParse(e.account(t, "2000", "101000")), uuid.MustParse(e.account(t, "2000", "100000"))
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-02"), PostingDate: date("2026-03-02"),
			Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: stock, Amount: 100}, {AccountID: bank, Amount: -100}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check := func(doc *config.Config) string {
		t.Helper()
		var findings []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, before, doc)
			for _, f := range fs {
				findings = append(findings, f.Path+" "+strings.SplitN(f.Message, ":", 2)[0])
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(findings, "\n")
	}
	const p = "companies[2000].account_determination.inventory[3000]."
	if got, want := check(withKeys(t, "", "530000", "")), p+"consumption account 510000\n"+p+"price_difference account 540000\n"+p+"stock account 101000"; got != want {
		t.Errorf("submission check removing both classes: got\n%s\nwant\n%s", got, want)
	}
	if got, want := check(classes("520000", true, "610000")), p+"stock account 101000 has postings"; got != want {
		t.Errorf("submission check changing the posted stock account: got\n%s\nwant\n%s", got, want)
	}
	for _, c := range []struct {
		name, detail string
		doc          *config.Config
	}{
		{"the price difference key, never posted", "inventory[3000].price_difference (account 540000)", classes("101000", false, "610000")},
		{"the posted stock account changed", "inventory[3000].stock, whose account 101000 has postings", classes("520000", true, "610000")},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := e.applyTax(t, c.doc)
			if p := (*problem.Problem)(nil); !errors.As(err, &p) || p.Code != "ledger.determination_key_posted" || p.Status != 409 {
				t.Fatalf("got %v, want 409 ledger.determination_key_posted", err)
			} else if !strings.Contains(p.Detail, c.detail) {
				t.Errorf("detail: got %q, want it to name %s", p.Detail, c.detail)
			}
			if got := e.determinationKeys(t); !strings.Contains(got, "inventory_price_difference:3000=540000, inventory_stock:3000=101000") {
				t.Errorf("class 3000's keys after the refusal: got %s, want them kept", got)
			}
		})
	}
	// Class 3001 has no postings: its stock account changes, then it goes.
	if err := e.applyTax(t, classes("101000", true, "520000")); err != nil {
		t.Fatalf("activation changing class 3001's stock account: %v", err)
	}
	if got, want := e.determined(t, config.InventoryKey(config.InventoryStock, "3001")), e.account(t, "2000", "520000"); got != want {
		t.Errorf("class 3001's stock key changed: got %s, want %s", got, want)
	}
	if err := e.applyTax(t, classes("101000", true, "")); err != nil {
		t.Fatalf("activation removing class 3001: %v", err)
	}
	if got := e.determined(t, config.InventoryKey(config.InventoryStock, "3001")); got != "none" {
		t.Errorf("class 3001's stock key removed: got %s, want none", got)
	}
}
