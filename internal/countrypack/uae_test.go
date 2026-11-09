package countrypack

import (
	"io/fs"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/packs"
)

// uaeRow is a row of packs/ae/TAX-PENDING-T62f.md: code, supply,
// direction, kind, category ("-" for none), rate, start, components and
// basis.
var uaeRow = regexp.MustCompile(`^\| ([A-Z0-9]+) \| [^|]+ \| (input|output) \| ([a-z_]+) \| ([A-Z]+|-) \| ([0-9.]+)% \| from (\d{4}-\d{2}-\d{2}) \(earlier start not sourced\) \| ([^|]+) \| [^|]+ \|$`)

// uaeCode is a code as the pending file states it, or as the pack writes
// it in the file's terms.
type uaeCode struct {
	code, rate, from string
	components       []string
}

// readUAEPending reads the pending file's table and source links, failing
// on a row it cannot read, so a changed layout is not silently skipped.
func readUAEPending(t *testing.T) (map[string]uaeCode, []string) {
	t.Helper()
	b, err := fs.ReadFile(packs.FS, "ae/TAX-PENDING-T62f.md")
	if err != nil {
		t.Fatalf("read the pending file: %v", err)
	}
	codes := map[string]uaeCode{}
	var urls []string
	for _, line := range strings.Split(string(b), "\n") {
		if m := pendingURL.FindStringSubmatch(line); m != nil {
			urls = append(urls, m[1])
		}
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Code ") {
			continue
		}
		m := uaeRow.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("pending row %q: not a row of code, supply, direction, kind, category, rate, start, components and basis", line)
		}
		if _, dup := codes[m[1]]; dup {
			t.Fatalf("pending %s: listed twice", m[1])
		}
		category := strings.TrimPrefix(m[4], "-")
		codes[m[1]] = uaeCode{code: m[2] + " " + m[3] + " " + category, rate: m[5], from: m[6], components: strings.Split(strings.TrimSpace(m[7]), "; ")}
	}
	return codes, urls
}

// uaePacked is a code of the pack in the pending file's terms, and what
// in it the file cannot state: a component not of AE, or not with one
// rate from a date on.
func uaePacked(c config.TaxCode) (uaeCode, []string) {
	out := uaeCode{code: c.DirectionOrInput() + " " + c.KindOrStandard() + " " + c.Category}
	var odd []string
	for _, p := range c.Components {
		s := p.Key + " " + p.AccountKey + " " + p.Account + " " + p.Recoverable()
		if p.JurisdictionOnLine {
			s += " on line"
		}
		out.components = append(out.components, s)
		if p.Jurisdiction != "AE" || len(p.Rates) != 1 || p.Rates[0].ValidTo != "" {
			odd = append(odd, p.Key)
			continue
		}
		out.rate, out.from = p.Rates[0].RatePercent, p.Rates[0].ValidFrom
	}
	return out, odd
}

// TestUAEPackTaxCodes: UAE 2026.1 writes the codes of
// packs/ae/TAX-PENDING-T62f.md exactly: the same codes, each with the
// file's direction, kind, category, components (account key, account,
// recoverable share, levied on the line's emirate), each of AE with the
// file's one rate from its start; and the manifest lists every source the
// file links (T62f).
func TestUAEPackTaxCodes(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.1")
	if ae == nil {
		t.Fatal("Get(AE, 2026.1): got nil, want the UAE pack")
	}
	want, urls := readUAEPending(t)
	var got []string
	for _, c := range ae.Tax.TaxCodes {
		got = append(got, c.Code)
	}
	if keys := slices.Sorted(maps.Keys(want)); !reflect.DeepEqual(slices.Sorted(slices.Values(got)), keys) {
		t.Fatalf("codes: got %v, want the pending file's %v", got, keys)
	}
	for _, c := range ae.Tax.TaxCodes {
		t.Run(c.Code, func(t *testing.T) {
			p, odd := uaePacked(c)
			if len(odd) != 0 {
				t.Errorf("components %v: got another jurisdiction or rates, want AE with one open rate", odd)
			}
			if p.code != want[c.Code].code {
				t.Errorf("direction, kind and category: got %q, want %q", p.code, want[c.Code].code)
			}
			if p.rate+" "+p.from != want[c.Code].rate+" "+want[c.Code].from {
				t.Errorf("rate: got %s from %s, want %s from %s", p.rate, p.from, want[c.Code].rate, want[c.Code].from)
			}
			if !reflect.DeepEqual(p.components, want[c.Code].components) {
				t.Errorf("components: got %v, want %v", p.components, want[c.Code].components)
			}
		})
	}
	if len(urls) != 5 {
		t.Fatalf("pending sources: got %d, want the file's 5", len(urls))
	}
	for _, u := range urls {
		if !slices.ContainsFunc(ae.Manifest.Sources, func(s Source) bool { return s.URL == u }) {
			t.Errorf("sources: got none for %s, want the pending file's source", u)
		}
	}
}

// TestUAEPack: UAE 2026.1 is not signed off, holds no holidays and no
// return (2026.2 adds the VAT 201, T61k), and states ADR-0049 §6 As built's documentary tier: above AED
// 10,000 (a total equal to it not in the tier), the supplier's TRN,
// pending sign-off, under Art. 59; the average rate and rounding per
// document. Applied to a UAE company on first adoption, the company gets
// the pack's chart, codes and tax settings, with a note that no tax
// specialist signed it off.
func TestUAEPack(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.1")
	if ae == nil {
		t.Fatal("Get(AE, 2026.1): got none, want the UAE pack")
	}
	if ae.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist signs it off")
	}
	if len(ae.Calendar.Holidays) != 0 || len(ae.Returns.Returns) != 0 {
		t.Errorf("holidays and returns: got %d and %d, want none (no source recorded; the VAT 201 is T61k)", len(ae.Calendar.Holidays), len(ae.Returns.Returns))
	}
	s := ae.Tax.Settings
	if s.RateType != "average" || s.Rounding != config.TaxRoundDocument {
		t.Errorf("rate type and rounding: got %s %s, want average document", s.RateType, s.Rounding)
	}
	if len(s.DocumentaryRequirements) != 1 {
		t.Fatalf("documentary tiers: got %d, want the one above AED 10,000", len(s.DocumentaryRequirements))
	}
	tier := s.DocumentaryRequirements[0]
	if amount, inclusive := tier.Threshold(); amount != "10000" || inclusive {
		t.Errorf("threshold: got %s inclusive %v, want above 10000", amount, inclusive)
	}
	if !reflect.DeepEqual(tier.Requires, []string{config.RequiresSupplierTaxID}) || !tier.PendingSignOff || !strings.Contains(tier.Source, "Art. 59") {
		t.Errorf("tier: got %+v, want the supplier's TRN, pending sign-off, under Art. 59", tier)
	}
	draft, notes, err := Apply(companyIn(t, "AE", "AED"), ae, lib, Options{Company: "1000", On: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	co := draft.Company("1000")
	if !reflect.DeepEqual(co.TaxSettings, s) {
		t.Errorf("company's tax settings: got %+v, want the pack's", co.TaxSettings)
	}
	if !reflect.DeepEqual(co.TaxCodes, ae.Tax.TaxCodes) {
		t.Errorf("company's tax codes: got %d codes, want the pack's %d", len(co.TaxCodes), len(ae.Tax.TaxCodes))
	}
	if !reflect.DeepEqual(co.AccountDetermination, ae.Accounts.AccountDetermination) {
		t.Errorf("account determination: got %+v, want the pack's", co.AccountDetermination)
	}
	if !hasNote(notes, "packs[1000]", "no tax specialist has signed this pack off") {
		t.Errorf("notes: got %v, want the pack not signed off", notes)
	}
}

// UAE 2026.2 is 2026.1 plus the VAT 201 (T61k): every file but pack.yaml
// is 2026.1's byte for byte and returns.yaml is new; the manifest is
// 2026.1's but for its version and the VAT 201 sources' titles (its
// header comment aside), with no sign-off, as the tax specialist has not
// confirmed the return. It defines the return type VAT201.
func TestUAEPack2026_2(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ae := lib.Get("AE", "2026.1"), lib.Get("AE", "2026.2")
	if prev == nil || ae == nil {
		t.Fatalf("Get(AE, 2026.1) and Get(AE, 2026.2): got %v and %v, want both", prev, ae)
	}
	if ae.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist confirms the return")
	}
	if ae.Return("VAT201") == nil || len(prev.Returns.Returns) != 0 {
		t.Errorf("returns: got %d in 2026.1 and VAT201 %v in 2026.2, want none and the return", len(prev.Returns.Returns), ae.Return("VAT201") != nil)
	}
	var urls []string
	for _, s := range ae.Manifest.Sources {
		urls = append(urls, s.URL)
	}
	for _, s := range prev.Manifest.Sources {
		if !slices.Contains(urls, s.URL) {
			t.Errorf("sources: got none for %s, want 2026.1's sources", s.URL)
		}
	}
	same := *ae
	same.Manifest.Version, same.Manifest.Sources, same.Returns = prev.Manifest.Version, prev.Manifest.Sources, prev.Returns
	if len(ae.Manifest.Sources) != len(prev.Manifest.Sources) || !reflect.DeepEqual(&same, prev) {
		t.Error("2026.2 without its version, its sources' titles and the return: got other than 2026.1, want it unchanged")
	}
	sameFiles(t, "ae/2026.1", "ae/2026.2", "pack.yaml", "returns.yaml")
}

// emirates are the ISO 3166-2 codes of the seven emirates, in the order of
// boxes 1a-1g, as the tax specialist confirmed them (product owner,
// 2026-11-09).
var emirates = []string{"AE-AZ", "AE-DU", "AE-SH", "AE-AJ", "AE-UQ", "AE-RK", "AE-FU"}

// UAE 2026.3 is 2026.2 with the emirate guard (ADR-0049 §7 As built): S5's
// component levying the line's emirate lists the seven emirates, the
// emirates boxes 1a-1g select, and nothing else differs: every file but
// pack.yaml and tax.yaml is 2026.2's byte for byte, and the manifest is
// 2026.2's but for its version (its header comment aside), with no
// sign-off (2026.4 has it).
func TestUAEPack2026_3(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ae := lib.Get("AE", "2026.2"), lib.Get("AE", "2026.3")
	if prev == nil || ae == nil {
		t.Fatalf("Get(AE, 2026.2) and Get(AE, 2026.3): got %v and %v, want both", prev, ae)
	}
	if ae.SignedOff() {
		t.Error("SignedOff: got true, want false: the sign-off is a later version")
	}
	i := slices.IndexFunc(ae.Tax.TaxCodes, func(c config.TaxCode) bool { return c.Code == "S5" })
	if i < 0 || len(ae.Tax.TaxCodes[i].Components) != 1 {
		t.Fatalf("S5: got %v, want the code with its one component", ae.Tax.TaxCodes)
	}
	if got := ae.Tax.TaxCodes[i].Components[0].Subdivisions; !reflect.DeepEqual(got, emirates) {
		t.Errorf("S5's subdivisions: got %v, want the seven emirates %v", got, emirates)
	}
	var boxed []string
	for _, b := range ae.Return("VAT201").Boxes {
		if b.Select != nil && b.Select.Direction == config.TaxOutput && slices.Contains(b.Select.Kinds, config.TaxStandard) {
			boxed = append(boxed, b.Select.Jurisdictions...)
		}
	}
	// Each emirate's boxes are adjacent (amount, VAT), so compacting them
	// leaves the emirates in the boxes' order, a repeat apart showing.
	if boxed = slices.Compact(boxed); !reflect.DeepEqual(boxed, emirates) {
		t.Errorf("the emirates of boxes 1a-1g: got %v, want S5's %v", boxed, emirates)
	}
	same := *ae
	same.Manifest.Version = prev.Manifest.Version
	same.Tax.TaxCodes = slices.Clone(ae.Tax.TaxCodes)
	s5 := same.Tax.TaxCodes[i]
	s5.Components = []config.TaxComponent{s5.Components[0]}
	s5.Components[0].Subdivisions = nil
	same.Tax.TaxCodes[i] = s5
	if !reflect.DeepEqual(&same, prev) {
		t.Error("2026.3 without its version and S5's subdivisions: got other than 2026.2, want it unchanged")
	}
	sameFiles(t, "ae/2026.2", "ae/2026.3", "pack.yaml", "tax.yaml")
}

// UAE 2026.4 is 2026.3 signed off (T62f's and T61k's sign-off leaves, one
// version): the tax specialist's sign-off as the product owner reported it
// on 2026-11-09, the documentary tier no longer pending, and nothing else:
// every file but pack.yaml and tax.yaml is 2026.3's byte for byte, and the
// manifest differs only in its version and sign-off (its header comment
// aside). It is the latest UAE pack, and applies without the note that no
// tax specialist signed it off.
func TestUAEPack2026_4(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ae := lib.Get("AE", "2026.3"), lib.Get("AE", "2026.4")
	if prev == nil || ae == nil {
		t.Fatalf("Get(AE, 2026.3) and Get(AE, 2026.4): got %v and %v, want both", prev, ae)
	}
	if got, want := ae.Manifest.SignOff, (&SignOff{TaxSpecialist: "Musto Ibrahim", Date: "2026-11-09"}); !reflect.DeepEqual(got, want) {
		t.Errorf("sign-off: got %+v, want %+v", got, want)
	}
	if !ae.SignedOff() {
		t.Error("SignedOff: got false, want true")
	}
	tiers := ae.Tax.Settings.DocumentaryRequirements
	if len(tiers) != 1 || tiers[0].PendingSignOff {
		t.Fatalf("documentary tiers: got %+v, want the one tier, not pending", tiers)
	}
	same := *ae
	same.Manifest.Version, same.Manifest.SignOff = prev.Manifest.Version, nil
	same.Tax.Settings.DocumentaryRequirements = []config.DocumentaryRequirement{tiers[0]}
	same.Tax.Settings.DocumentaryRequirements[0].PendingSignOff = true
	if !reflect.DeepEqual(&same, prev) {
		t.Error("2026.4 without its version, sign-off and the tier's pending flag: got other than 2026.3, want it unchanged")
	}
	if got := lib.Latest("AE"); got != ae {
		t.Errorf("Latest(AE): got %s, want ae/2026.4", got.Dir())
	}
	sameFiles(t, "ae/2026.3", "ae/2026.4", "pack.yaml", "tax.yaml")
	_, notes, err := Apply(companyIn(t, "AE", "AED"), ae, lib, Options{Company: "1000", On: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if hasNote(notes, "packs[1000]", "no tax specialist has signed this pack off") {
		t.Errorf("notes: got %v, want none saying the pack is not signed off", notes)
	}
}

// sameFiles fails unless every file of dir but those excepted has the
// bytes of the same file in prev.
func sameFiles(t *testing.T, prev, dir string, except ...string) {
	t.Helper()
	files, err := fs.ReadDir(packs.FS, dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, f := range files {
		if slices.Contains(except, f.Name()) {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			was, errWas := fs.ReadFile(packs.FS, prev+"/"+f.Name())
			is, errIs := fs.ReadFile(packs.FS, dir+"/"+f.Name())
			if errWas != nil || errIs != nil {
				t.Fatalf("read: got %v and %v, want the file in both versions", errWas, errIs)
			}
			if string(was) != string(is) {
				t.Errorf("got other bytes than %s's, want the same file", prev)
			}
		})
	}
}

// vat201Row is a row of packs/ae/TAX-PENDING-T61k.md: the form's box, the
// pack's code, what it computes and its basis.
var vat201Row = regexp.MustCompile(`^\| ([0-9]{1,2}[a-g]?) \| ([0-9a-z_]+) \| ([^|]+) \| (ADR-0049|ADR-0049 box|memory) \|$`)

// vat201Computed is what a box computes in the pending file's terms.
func vat201Computed(b Box) string {
	if b.Select == nil {
		return "= " + b.Expression
	}
	s := b.Select
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "-"
		}
		return strings.Join(xs, ", ")
	}
	out := strings.Join([]string{s.Direction, list(s.Kinds), list(s.Components), list(s.Jurisdictions), list(s.AccountKeys), s.Measure}, " · ")
	if s.Negate {
		out += " · shown positive"
	}
	return out
}

// TestUAEPackVAT201: the VAT 201 of UAE 2026.2 has the boxes of
// packs/ae/TAX-PENDING-T61k.md in its order, each coded, labelled with its
// box and computed as the table says; a box whose column the ADR does not
// record says so in its label, as does a total written from memory, which
// is an expression; the threshold is P7's AED 10,000; the file links the
// secondary sources the manifest cites for the VAT 201.
func TestUAEPackVAT201(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.2")
	if ae == nil || ae.Return("VAT201") == nil {
		t.Fatal("Get(AE, 2026.2).Return(VAT201): got none, want the VAT 201")
	}
	def := ae.Return("VAT201")
	if def.PriorPeriodThreshold != "10000" {
		t.Errorf("prior-period threshold: got %q, want P7's 10000", def.PriorPeriodThreshold)
	}
	b, err := fs.ReadFile(packs.FS, "ae/TAX-PENDING-T61k.md")
	if err != nil {
		t.Fatalf("read the pending file: %v", err)
	}
	var rows [][]string
	for _, line := range strings.Split(string(b), "\n") {
		if m := vat201Row.FindStringSubmatch(line); m != nil {
			rows = append(rows, m)
		} else if strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| Box ") {
			t.Errorf("a table row the test cannot read: %q", line)
		}
	}
	if len(rows) != len(def.Boxes) {
		t.Fatalf("boxes: got %d in the file and %d in the pack, want the same", len(rows), len(def.Boxes))
	}
	for i, r := range rows {
		box := def.Boxes[i]
		t.Run(r[2], func(t *testing.T) {
			if box.Code != r[2] || !strings.HasPrefix(box.Label, r[1]+" ") {
				t.Errorf("box %d: got %s labelled %q, want %s of box %s", i, box.Code, box.Label, r[2], r[1])
			}
			if got := vat201Computed(box); got != strings.TrimSpace(r[3]) {
				t.Errorf("computed as: got %q, want %q", got, strings.TrimSpace(r[3]))
			}
			memory := strings.Contains(box.Label, "from memory, pending the tax specialist")
			column := strings.Contains(box.Label, "column pending the tax specialist")
			if memory != (r[4] == "memory") || (memory && box.Select != nil) || column != (r[4] == "ADR-0049 box") {
				t.Errorf("label %q and expression %v: got the basis %s, want a memory total marked and computed from others, a pending column marked", box.Label, box.Select == nil, r[4])
			}
		})
	}
	for _, u := range []string{"https://www.zoho.com/ae/books/vat/guides/vat-return.html", "https://cleartax.com/ae/vat-return-uae"} {
		if !strings.Contains(string(b), u) || !slices.ContainsFunc(ae.Manifest.Sources, func(s Source) bool { return s.URL == u }) {
			t.Errorf("source %s: got it missing from the pending file or the manifest, want both", u)
		}
	}
}

// TestUAEPackVAT201SignedOff: the VAT 201 the tax specialist signed off
// (T61k's sign-off leaf, product owner 2026-11-09) is 2026.2's, box for box,
// in UAE 2026.4, the version signed off: the sign-off froze the return as
// TestUAEPackVAT201 holds it to packs/ae/TAX-PENDING-T61k.md.
func TestUAEPackVAT201SignedOff(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	was, signed := lib.Get("AE", "2026.2"), lib.Get("AE", "2026.4")
	if was == nil || signed == nil || signed.Return("VAT201") == nil {
		t.Fatalf("Get(AE, 2026.2) and Get(AE, 2026.4).Return(VAT201): got %v and %v, want both with the return", was, signed)
	}
	if !reflect.DeepEqual(signed.Return("VAT201"), was.Return("VAT201")) {
		t.Error("2026.4's VAT 201: got other than 2026.2's, want the return unchanged")
	}
	if !signed.SignedOff() {
		t.Error("2026.4 SignedOff: got false, want true")
	}
}
