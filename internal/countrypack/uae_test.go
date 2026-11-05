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
// return, and states ADR-0049 §6 As built's documentary tier: above AED
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
	if ae == nil || lib.Latest("AE") != ae {
		t.Fatal("Get(AE, 2026.1): got none or not the latest, want the UAE pack")
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
