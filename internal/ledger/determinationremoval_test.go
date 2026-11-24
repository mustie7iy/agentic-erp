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

// TestDeterminationKeyPostedKept (ADR-0048 As built batch 93): a
// valuation class's key whose account has postings is not removed, as a
// reversal of the material documents that posted them determines it
// again: the submission check reports it, and the activation is refused
// with 409 ledger.determination_key_posted, the key kept. Its account may
// still change, and a key without a reversal that needs it goes.
func TestDeterminationKeyPostedKept(t *testing.T) {
	e := setup(t)
	before := withKeys(t, "520000", "530000", "101000")
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
	removed := withKeys(t, "", "530000", "")
	var findings []string
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, before, removed)
		for _, f := range fs {
			findings = append(findings, f.Path+": "+f.Message)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(findings, "\n"), "companies[2000].account_determination.inventory[3000].stock: account 101000 has postings"; !strings.HasPrefix(got, want) {
		t.Errorf("submission check: got\n%s\nwant one finding starting %q", got, want)
	}
	err := e.applyTax(t, removed)
	if p := (*problem.Problem)(nil); !errors.As(err, &p) || p.Code != "ledger.determination_key_posted" || p.Status != 409 {
		t.Fatalf("activation removing the posted stock key: got %v, want 409 ledger.determination_key_posted", err)
	} else if !strings.Contains(p.Detail, "inventory[3000].stock, whose account 101000 has postings") {
		t.Errorf("detail: got %q, want it to name inventory[3000].stock and account 101000", p.Detail)
	}
	if got := e.determined(t, config.InventoryKey(config.InventoryStock, "3000")); got != stock.String() {
		t.Errorf("stock key after the refusal: got %s, want %s kept", got, stock)
	}
	// The consumption account has no postings: it goes, the stock key stays.
	changed := withKeys(t, "", "530000", "")
	changed.Companies[1].AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "100000"}}
	if err := e.applyTax(t, changed); err != nil {
		t.Fatalf("activation changing the stock account: %v", err)
	}
	if got := e.determined(t, config.InventoryKey(config.InventoryStock, "3000")); got != bank.String() {
		t.Errorf("stock key changed: got %s, want %s", got, bank)
	}
	if got := e.determined(t, config.InventoryKey(config.InventoryConsumption, "3000")); got != "none" {
		t.Errorf("consumption key removed: got %s, want none", got)
	}
}
