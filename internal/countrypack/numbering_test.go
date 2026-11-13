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

// addedRanges are the document types a numbering-only version adds
// (product owner, 2026-11-12), in the pack's order: the ranges the
// documents built after the first packs ask for, which a company
// activated from the earlier version lacks (409 config.no_number_range).
// They are procurement's rfq, sales' quotation, sales_order and
// billing_document, receivables' customer_invoice (credit memos too) and
// incoming_payment, and the journal ranges of the entries receivables
// posts: DR invoices, DG credit memos, DZ incoming payments.
var addedRanges = []NumberRange{
	{DocumentType: "rfq", Prefix: "RQ", First: 1, Last: 9999999},
	{DocumentType: "quotation", Prefix: "QT", First: 1, Last: 9999999},
	{DocumentType: "sales_order", Prefix: "SO", First: 1, Last: 9999999},
	{DocumentType: "billing_document", Prefix: "BD", First: 1, Last: 9999999},
	{DocumentType: "customer_invoice", Prefix: "CI", First: 1, Last: 9999999},
	{DocumentType: "incoming_payment", Prefix: "IP", First: 1, Last: 9999999},
	{DocumentType: "journal_dr", Prefix: "DR", First: 1, Last: 9999999},
	{DocumentType: "journal_dg", Prefix: "DG", First: 1, Last: 9999999},
	{DocumentType: "journal_dz", Prefix: "DZ", First: 1, Last: 9999999},
}

// numberingOnly fails unless next is prev with only number ranges added:
// every file but pack.yaml and numbering.yaml is prev's byte for byte
// (tax.yaml and returns.yaml among them), the parsed tax codes, tax
// settings and returns are prev's, the manifest differs only in its
// version and sign-off (its header comment aside), and the ranges are
// prev's, unchanged and in order, then addedRanges. The tax specialist
// signed prev off, and next carries the same sign-off (product owner,
// 2026-11-13: the sign-off of 2026-11-09 covers the numbering-only
// versions). It returns the library and the two packs.
func numberingOnly(t *testing.T, country, prevVersion, nextVersion string) (*Library, *Pack, *Pack) {
	t.Helper()
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, next := lib.Get(country, prevVersion), lib.Get(country, nextVersion)
	if prev == nil || next == nil {
		t.Fatalf("Get(%s, %s) and Get(%s, %s): got %v and %v, want both", country, prevVersion, country, nextVersion, prev, next)
	}
	if !reflect.DeepEqual(next.Tax, prev.Tax) {
		t.Errorf("tax codes and settings: got other than %s's, want them unchanged", prev.Dir())
	}
	if !reflect.DeepEqual(next.Returns, prev.Returns) {
		t.Errorf("returns: got other than %s's, want them unchanged", prev.Dir())
	}
	if !prev.SignedOff() || !next.SignedOff() || !reflect.DeepEqual(next.Manifest.SignOff, prev.Manifest.SignOff) {
		t.Errorf("sign-off: got %+v on %s and %+v on %s, want both signed off, the second with the first's", prev.Manifest.SignOff, prev.Dir(), next.Manifest.SignOff, next.Dir())
	}
	if got, want := next.Numbering.NumberRanges, append(slices.Clone(prev.Numbering.NumberRanges), addedRanges...); !slices.Equal(got, want) {
		t.Errorf("number ranges: got %+v, want %+v", got, want)
	}
	same := *next
	same.Manifest.Version, same.Manifest.SignOff, same.Numbering = prev.Manifest.Version, prev.Manifest.SignOff, prev.Numbering
	if !reflect.DeepEqual(&same, prev) {
		t.Errorf("%s without its version, sign-off and number ranges: got other than %s, want it unchanged", next.Dir(), prev.Dir())
	}
	if got := lib.Latest(country); got != next {
		t.Errorf("Latest(%s): got %s, want %s", country, got.Dir(), next.Dir())
	}
	sameFiles(t, prev.Dir(), next.Dir(), "pack.yaml", "numbering.yaml")
	return lib, prev, next
}

// rangeKeys are the keys of the document's ranges, in its order.
func rangeKeys(c *config.Config) []string {
	var out []string
	for _, n := range c.NumberRanges {
		out = append(out, n.Key())
	}
	return out
}

// adoptsAddedRanges applies next to a company of its country in three
// ways. On first adoption the document validates and holds every added
// range. A company on prev upgrades by applying next: the document gains
// the added ranges, after its own, and nothing else but the adoption
// changes. A company on prev that already set one of the ranges up keeps
// its own, with a conflict note for the reviewers. Signed off, next
// brings no note saying otherwise.
func adoptsAddedRanges(t *testing.T, lib *Library, prev, next *Pack, currencyCode string) {
	t.Helper()
	o := Options{Company: "1000", On: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), FiscalYear: 2026}
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
	var added []string
	for _, r := range addedRanges {
		added = append(added, "1000/"+r.DocumentType+"/2026")
		if !slices.Contains(rangeKeys(first), added[len(added)-1]) {
			t.Errorf("first adoption: got no range %s, want it", added[len(added)-1])
		}
	}
	onPrev, _, err := Apply(companyIn(t, prev.Manifest.Country, currencyCode), prev, lib, o)
	if err != nil {
		t.Fatalf("Apply %s: %v", prev.Dir(), err)
	}
	upgraded, notes, err := Apply(onPrev, next, lib, o)
	if err != nil {
		t.Fatalf("Apply %s over %s: %v", next.Dir(), prev.Dir(), err)
	}
	if got, want := rangeKeys(upgraded), append(rangeKeys(onPrev), added...); !slices.Equal(got, want) {
		t.Errorf("upgrade: got the ranges %v, want %v", got, want)
	}
	if !hasNote(notes, "packs[1000]", "upgrading from "+prev.Manifest.Version) {
		t.Errorf("upgrade: got the notes %v, want the base named", notes)
	}
	same := *upgraded
	same.NumberRanges, same.Packs = onPrev.NumberRanges, onPrev.Packs
	if !reflect.DeepEqual(&same, onPrev) {
		t.Error("upgrade: got more than number ranges and the adoption changed, want the rest of the document as it was")
	}
	own := config.NumberRange{Company: "1000", DocumentType: "journal_dr", FiscalYear: 2026, First: 1800000000, Last: 1899999999}
	onPrev.NumberRanges = append(onPrev.NumberRanges, own)
	kept, notes, err := Apply(onPrev, next, lib, o)
	if err != nil {
		t.Fatalf("Apply %s over %s with a range of its own: %v", next.Dir(), prev.Dir(), err)
	}
	if i := slices.Index(rangeKeys(kept), own.Key()); i < 0 || kept.NumberRanges[i] != own {
		t.Errorf("a range the company set up: got %+v, want it kept as %+v", kept.NumberRanges, own)
	}
	if !hasNote(notes, "number_ranges["+own.Key()+"]", "conflict: already here and different in the pack") {
		t.Errorf("a range the company set up: got the notes %v, want the conflict noted", notes)
	}
}

// Canada 2026.9 is 2026.8 plus the number ranges of the documents built
// since (product owner, 2026-11-12): its tax codes, rates, tax settings
// and GST/HST return are 2026.8's, the content the tax specialist signed
// off, and carries that sign-off (product owner, 2026-11-13). It is the
// latest Canada pack.
func TestCanadaPack2026_9(t *testing.T) {
	lib, prev, next := numberingOnly(t, "CA", "2026.8", "2026.9")
	adoptsAddedRanges(t, lib, prev, next, "CAD")
}

// UAE 2026.6 is 2026.5 plus the same number ranges: its VAT codes, rates,
// tax settings and VAT 201 are 2026.5's, the content the tax specialist
// signed off, and carries that sign-off (product owner, 2026-11-13). It is
// the latest UAE pack.
func TestUAEPack2026_6(t *testing.T) {
	lib, prev, next := numberingOnly(t, "AE", "2026.5", "2026.6")
	adoptsAddedRanges(t, lib, prev, next, "AED")
}
