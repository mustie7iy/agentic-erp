package countrypack

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/packs"
)

// discountAccounts are what Canada 2026.13 and UAE 2026.10 add to the
// chart (ADR-0063 and ADR-0061 As built batch 92), in the chart's order:
// the supplier rebates received, a revenue beside 4800's discounts
// received, and the cash discounts granted to customers, an expense.
var discountAccounts = []config.Account{
	{Number: "4810", Name: "Supplier rebates received", Type: config.Revenue},
	{Number: "6800", Name: "Cash discounts granted", Type: config.Expense},
}

// withDiscountKeys is the account determination ad with the two keys of
// batch 92 on discountAccounts.
func withDiscountKeys(ad config.AccountDetermination) config.AccountDetermination {
	ad.SupplierRebate, ad.CashDiscountGranted = "4810", "6800"
	return ad
}

// discountKeysOnly fails unless next is prev with only discountAccounts
// and their keys added: every file but pack.yaml and accounts.yaml is
// prev's byte for byte (tax.yaml, returns.yaml and numbering.yaml among
// them), the parsed packs differ only in their version, sign-off and
// accounts, prev's accounts are next's in order with the added ones
// among them, and the account determination is prev's with
// cash_discount_granted and supplier_rebate. next carries prev's sign-off
// (decided by default in batch 92, as a numbering-only version carries
// it under the product owner's rule of 2026-11-18: its tax content is
// the content signed off).
func discountKeysOnly(t *testing.T, country, prevVersion, nextVersion string) (*Library, *Pack, *Pack) {
	t.Helper()
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, next := lib.Get(country, prevVersion), lib.Get(country, nextVersion)
	if prev == nil || next == nil {
		t.Fatalf("Get(%s, %s) and Get(%s, %s): got %v and %v, want both", country, prevVersion, country, nextVersion, prev, next)
	}
	if !prev.SignedOff() || !next.SignedOff() || !reflect.DeepEqual(next.Manifest.SignOff, prev.Manifest.SignOff) {
		t.Errorf("sign-off: got %+v on %s and %+v on %s, want both signed off, the second with the first's", prev.Manifest.SignOff, prev.Dir(), next.Manifest.SignOff, next.Dir())
	}
	kept := slices.DeleteFunc(slices.Clone(next.Accounts.Accounts), func(a config.Account) bool { return slices.Contains(discountAccounts, a) })
	if !slices.Equal(kept, prev.Accounts.Accounts) || len(kept)+len(discountAccounts) != len(next.Accounts.Accounts) {
		t.Errorf("accounts: got %+v, want %s's with %+v added", next.Accounts.Accounts, prev.Dir(), discountAccounts)
	}
	if got, want := next.Accounts.AccountDetermination, withDiscountKeys(prev.Accounts.AccountDetermination); !reflect.DeepEqual(got, want) {
		t.Errorf("account determination: got %+v, want %+v", got, want)
	}
	same := *next
	same.Manifest.Version, same.Manifest.SignOff, same.Accounts = prev.Manifest.Version, prev.Manifest.SignOff, prev.Accounts
	if !reflect.DeepEqual(&same, prev) {
		t.Errorf("%s without its version, sign-off and accounts: got other than %s, want it unchanged", next.Dir(), prev.Dir())
	}
	sameFiles(t, prev.Dir(), next.Dir(), "pack.yaml", "accounts.yaml")
	return lib, prev, next
}

// adoptsDiscountKeys applies next to a company of its country in three
// ways. On first adoption the document validates and names both
// accounts. A company on prev upgrades by applying next: it gains the
// two accounts and keys, and nothing else but the adoption changes. A
// company on prev that named its own cash discount granted account keeps
// it, with a conflict note for the reviewers.
func adoptsDiscountKeys(t *testing.T, lib *Library, prev, next *Pack, currencyCode string) {
	t.Helper()
	o := Options{Company: "1000", On: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), FiscalYear: 2026}
	first, notes, err := Apply(companyIn(t, next.Manifest.Country, currencyCode), next, lib, o)
	if err != nil {
		t.Fatalf("Apply %s: %v", next.Dir(), err)
	}
	for _, f := range config.Validate(first, currency.Seeded()) {
		if f.Severity == config.SevError {
			t.Errorf("first adoption: finding %s: %s", f.Path, f.Message)
		}
	}
	if hasNote(notes, "packs[1000]", "no tax specialist has signed this pack off") {
		t.Errorf("notes: got %v, want none saying the pack is not signed off", notes)
	}
	if ad := first.Company("1000").AccountDetermination; ad.CashDiscountGranted != "6800" || ad.SupplierRebate != "4810" {
		t.Errorf("first adoption: got cash_discount_granted %q and supplier_rebate %q, want 6800 and 4810", ad.CashDiscountGranted, ad.SupplierRebate)
	}
	onPrev, _, err := Apply(companyIn(t, prev.Manifest.Country, currencyCode), prev, lib, o)
	if err != nil {
		t.Fatalf("Apply %s: %v", prev.Dir(), err)
	}
	upgraded, notes, err := Apply(onPrev, next, lib, o)
	if err != nil {
		t.Fatalf("Apply %s over %s: %v", next.Dir(), prev.Dir(), err)
	}
	if !hasNote(notes, "packs[1000]", "upgrading from "+prev.Manifest.Version) {
		t.Errorf("upgrade: got the notes %v, want the base named", notes)
	}
	co, was := upgraded.Company("1000"), onPrev.Company("1000")
	if got, want := co.AccountDetermination, withDiscountKeys(was.AccountDetermination); !reflect.DeepEqual(got, want) {
		t.Errorf("upgrade: got the account determination %+v, want %+v", got, want)
	}
	for _, a := range discountAccounts {
		if got := co.Account(a.Number); got == nil || got.Name != a.Name || got.Type != a.Type {
			t.Errorf("upgrade: got account %s %+v, want %+v", a.Number, got, a)
		}
	}
	same := *upgraded
	same.Packs, same.Companies = onPrev.Packs, slices.Clone(upgraded.Companies)
	c := same.Company("1000")
	c.ChartOfAccounts = slices.DeleteFunc(slices.Clone(c.ChartOfAccounts), func(a config.Account) bool { return slices.Contains(discountAccounts, a) })
	c.AccountDetermination = was.AccountDetermination
	if !reflect.DeepEqual(&same, onPrev) {
		t.Error("upgrade: got more than the two accounts, their keys and the adoption changed, want the rest of the document as it was")
	}
	was.ChartOfAccounts = append(was.ChartOfAccounts, config.Account{Number: "6810", Name: "Discounts allowed", Type: config.Expense})
	was.AccountDetermination.CashDiscountGranted = "6810"
	kept, notes, err := Apply(onPrev, next, lib, o)
	if err != nil {
		t.Fatalf("Apply %s over %s with an account of its own: %v", next.Dir(), prev.Dir(), err)
	}
	if got := kept.Company("1000").AccountDetermination.CashDiscountGranted; got != "6810" {
		t.Errorf("an account the company named: got cash_discount_granted %s, want 6810 kept", got)
	}
	if !hasNote(notes, "companies[1000].account_determination[cash_discount_granted]", "conflict") {
		t.Errorf("an account the company named: got the notes %v, want the conflict noted", notes)
	}
}

// Canada 2026.13 is 2026.12 plus the accounts and keys of cash discounts
// granted and supplier rebates, under the same sign-off. It is the latest
// Canada pack, after 2026.12 and 2026.1 by number, not by text.
func TestCanadaPack2026_13(t *testing.T) {
	lib, prev, next := discountKeysOnly(t, "CA", "2026.12", "2026.13")
	adoptsDiscountKeys(t, lib, prev, next, "CAD")
	if got := lib.Latest("CA"); got != next {
		t.Errorf("Latest(CA): got %s, want %s", got.Dir(), next.Dir())
	}
}

// UAE 2026.10 is 2026.9 plus the accounts and keys of cash discounts
// granted and supplier rebates, under the same sign-off. It is the latest
// UAE pack, after 2026.9 and 2026.1 by number, not by text.
func TestUAEPack2026_10(t *testing.T) {
	lib, prev, next := discountKeysOnly(t, "AE", "2026.9", "2026.10")
	adoptsDiscountKeys(t, lib, prev, next, "AED")
	if got := lib.Latest("AE"); got != next {
		t.Errorf("Latest(AE): got %s, want %s", got.Dir(), next.Dir())
	}
}
