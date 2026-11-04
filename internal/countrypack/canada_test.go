package countrypack

import (
	"io/fs"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/packs"
)

// pendingCode is a row of packs/ca/TAX-PENDING-T61a.md as the pack must
// write it: its jurisdiction, its rates as "rate from..to" (to empty when
// open) and its recovery as "account_key account recoverable_percent".
type pendingCode struct {
	jurisdiction string
	rates        []string
	recovery     string
}

// provinces are the ISO 3166-2 codes of the provinces the file's Tax
// column names; a GST row is the country's.
var provinces = map[string]string{"Ontario": "CA-ON", "New Brunswick": "CA-NB", "Newfoundland and Labrador": "CA-NL",
	"Nova Scotia": "CA-NS", "Prince Edward Island": "CA-PE", "Quebec": "CA-QC", "British Columbia": "CA-BC",
	"Manitoba": "CA-MB", "Saskatchewan": "CA-SK"}

var (
	pendingRow  = regexp.MustCompile(`^\| ([A-Z0-9]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \| [^|]+ \|$`)
	pendingFrom = regexp.MustCompile(`^from (\d{4}-\d{2}-\d{2})(?: \(earlier start not sourced\))?$`)
	pendingSpan = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.\.(\d{4}-\d{2}-\d{2})$`)
	pendingAcct = regexp.MustCompile(`^input tax (?:credit|refund), (?:account )?(\d{4})$`)
	pendingURL  = regexp.MustCompile(`^- [^:]+: (https://\S+)$`)
)

// readPending reads the pending file's table and source links, failing on
// a row it cannot read, so a changed layout is not silently skipped.
func readPending(t *testing.T) (map[string]pendingCode, []string) {
	t.Helper()
	b, err := fs.ReadFile(packs.FS, "ca/TAX-PENDING-T61a.md")
	if err != nil {
		t.Fatalf("read the pending file: %v", err)
	}
	codes := map[string]pendingCode{}
	var urls []string
	for _, line := range strings.Split(string(b), "\n") {
		if m := pendingURL.FindStringSubmatch(line); m != nil {
			urls = append(urls, m[1])
		}
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Code ") {
			continue
		}
		m := pendingRow.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("pending row %q: not a row of code, tax, rate, in force, recovery and source", line)
		}
		if _, dup := codes[m[1]]; dup {
			t.Fatalf("pending %s: listed twice", m[1])
		}
		codes[m[1]] = pendingCode{jurisdiction: pendingJurisdiction(t, m[1], m[2]), rates: pendingRates(t, m[1], m[3], m[4]),
			recovery: pendingRecovery(t, m[1], m[5])}
	}
	return codes, urls
}

// pendingJurisdiction is the jurisdiction a row's Tax column names: the
// country for GST, else the one province it names.
func pendingJurisdiction(t *testing.T, code, tax string) string {
	t.Helper()
	if strings.HasPrefix(tax, "GST (") {
		return "CA"
	}
	var found []string
	for name, iso := range provinces {
		if strings.Contains(tax, name) {
			found = append(found, iso)
		}
	}
	if len(found) != 1 {
		t.Fatalf("pending %s: tax %q names provinces %v, want one", code, tax, found)
	}
	return found[0]
}

// pendingRates pairs a row's rates ("13% / 15%") with its periods
// ("2013-04-01..2016-06-30 / from 2016-07-01").
func pendingRates(t *testing.T, code, rates, periods string) []string {
	t.Helper()
	rs, ps := strings.Split(strings.TrimSpace(rates), " / "), strings.Split(strings.TrimSpace(periods), " / ")
	if len(rs) != len(ps) {
		t.Fatalf("pending %s: %d rates for %d periods", code, len(rs), len(ps))
	}
	var out []string
	for i, r := range rs {
		pct, ok := strings.CutSuffix(r, "%")
		if !ok {
			t.Fatalf("pending %s: rate %q is not a percentage", code, r)
		}
		switch from, span := pendingFrom.FindStringSubmatch(ps[i]), pendingSpan.FindStringSubmatch(ps[i]); {
		case from != nil:
			out = append(out, pct+" "+from[1]+"..")
		case span != nil:
			out = append(out, pct+" "+span[1]+".."+span[2])
		default:
			t.Fatalf("pending %s: period %q is neither from nor a span", code, ps[i])
		}
	}
	return out
}

// pendingRecovery reads a row's proposed recovery: an input tax credit or
// refund to an account, or none (expensed).
func pendingRecovery(t *testing.T, code, s string) string {
	t.Helper()
	s = strings.TrimSpace(s)
	if m := pendingAcct.FindStringSubmatch(s); m != nil {
		return config.KeyInput + " " + m[1] + " 100"
	}
	if strings.HasPrefix(s, "none (expensed") {
		// The schema's non-deductible component has no account: its tax
		// is added to the purchase's own line (ADR-0049 §3, §6).
		return config.KeyNonDeductible + "  0"
	}
	t.Fatalf("pending %s: recovery %q is neither an input tax credit or refund nor none", code, s)
	return ""
}

// packed is a code of the pack in the pending file's terms.
func packed(c config.TaxCode) pendingCode {
	out := pendingCode{}
	if len(c.Components) != 1 {
		return out
	}
	p := c.Components[0]
	for _, r := range p.Rates {
		out.rates = append(out.rates, r.RatePercent+" "+r.ValidFrom+".."+r.ValidTo)
	}
	out.jurisdiction = p.Jurisdiction
	out.recovery = p.AccountKey + " " + p.Account + " " + p.Recoverable()
	return out
}

// Canada 2026.4 writes the tax codes of packs/ca/TAX-PENDING-T61a.md
// exactly: the same codes, each one input standard component (category
// S) of the jurisdiction its Tax column names, with the file's rates and
// periods and its proposed recovery (account and recoverable share), and every source the file cites
// (ADR-0048, T62g).
func TestCanadaPackTaxCodes(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.4")
	if ca == nil {
		t.Fatal("Get(CA, 2026.4): got nil, want the Canada pack with tax codes")
	}
	want, urls := readPending(t)
	var got []string
	for _, c := range ca.Tax.TaxCodes {
		got = append(got, c.Code)
	}
	if keys := slices.Sorted(maps.Keys(want)); !reflect.DeepEqual(slices.Sorted(slices.Values(got)), keys) {
		t.Fatalf("codes: got %v, want the pending file's %v", got, keys)
	}
	for _, c := range ca.Tax.TaxCodes {
		t.Run(c.Code, func(t *testing.T) {
			p := packed(c)
			if len(c.Components) != 1 {
				t.Fatalf("components: got %d, want 1", len(c.Components))
			}
			if !reflect.DeepEqual(p.rates, want[c.Code].rates) {
				t.Errorf("rates: got %v, want %v", p.rates, want[c.Code].rates)
			}
			if p.jurisdiction != want[c.Code].jurisdiction {
				t.Errorf("jurisdiction: got %q, want %q", p.jurisdiction, want[c.Code].jurisdiction)
			}
			if p.recovery != want[c.Code].recovery {
				t.Errorf("recovery: got %q, want %q", p.recovery, want[c.Code].recovery)
			}
			if got := c.DirectionOrInput() + " " + c.KindOrStandard() + " " + c.Category; got != "input standard S" {
				t.Errorf("code: got %s, want input standard S", got)
			}
		})
	}
	if len(urls) != 6 {
		t.Fatalf("pending sources: got %d, want the file's 6", len(urls))
	}
	for _, u := range urls {
		if !slices.ContainsFunc(ca.Manifest.Sources, func(s Source) bool { return s.URL == u }) {
			t.Errorf("sources: got none for %s, want the pending file's source", u)
		}
	}
}

// Canada 2026.4 is the version before it, 2026.3, plus the tax codes: the
// cash.yaml liquidity items stay (a version without them would drop them
// from tenants that adopted 2026.2), so do 2026.3's tax settings, the
// manifest (sources, name, valid_from) is the previous version's, and it
// is not signed off (2026.5 is).
func TestCanadaPack2026_4(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.4")
	var prev *Pack
	for _, p := range lib.All() {
		if p.Manifest.Country == "CA" && CompareVersions(p.Manifest.Version, "2026.4") < 0 {
			prev = p // All is in country and version order: the last one before 2026.4
		}
	}
	if ca == nil || prev == nil {
		t.Fatalf("Get(CA, 2026.4) and the version before it: got %v and %v, want both", ca, prev)
	}
	// Everything tax.yaml holds but the codes is the previous version's:
	// 2026.3's tax settings.
	prevTax, caTax := prev.Tax, ca.Tax
	prevTax.TaxCodes, caTax.TaxCodes = nil, nil
	// The manifest is the previous one's but for its version and sign-off.
	prevManifest, caManifest := prev.Manifest, ca.Manifest
	prevManifest.Version, caManifest.Version = "", ""
	prevManifest.SignOff, caManifest.SignOff = nil, nil
	for name, same := range map[string]bool{
		"tax.yaml besides the codes": reflect.DeepEqual(prevTax, caTax),
		"accounts":                   reflect.DeepEqual(prev.Accounts, ca.Accounts),
		"calendar":                   reflect.DeepEqual(prev.Calendar, ca.Calendar),
		"numbering":                  reflect.DeepEqual(prev.Numbering, ca.Numbering),
		"payments":                   reflect.DeepEqual(prev.Payments, ca.Payments),
		"cash":                       reflect.DeepEqual(prev.Cash, ca.Cash),
		"returns":                    reflect.DeepEqual(prev.Returns, ca.Returns),
		"pack.yaml besides version":  reflect.DeepEqual(prevManifest, caManifest),
	} {
		if !same {
			t.Errorf("%s: got other than %s's, want them unchanged", name, prev.Manifest.Version)
		}
	}
	if ca.SignedOff() {
		t.Error("SignedOff: got true, want false: the signed-off version is 2026.5")
	}
}

// Canada 2026.5 is 2026.4 signed off (T62h): every file but pack.yaml is
// 2026.4's byte for byte, and the manifest differs only in its version
// and the sign-off (its header comment aside), the tax
// specialist's as the product owner reported it on 2026-11-02.
func TestCanadaPack2026_5(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ca := lib.Get("CA", "2026.4"), lib.Get("CA", "2026.5")
	if prev == nil || ca == nil {
		t.Fatalf("Get(CA, 2026.4) and Get(CA, 2026.5): got %v and %v, want both", prev, ca)
	}
	if got, want := ca.Manifest.SignOff, (&SignOff{TaxSpecialist: "Musto Ibrahim", Date: "2026-11-02"}); !reflect.DeepEqual(got, want) {
		t.Errorf("sign-off: got %+v, want %+v", got, want)
	}
	if !ca.SignedOff() {
		t.Error("SignedOff: got false, want true")
	}
	unsigned := *ca
	unsigned.Manifest.Version, unsigned.Manifest.SignOff = prev.Manifest.Version, nil
	if !reflect.DeepEqual(&unsigned, prev) {
		t.Error("2026.5 without its version and sign-off: got other than 2026.4, want it unchanged")
	}
	files, err := fs.ReadDir(packs.FS, "ca/2026.5")
	if err != nil {
		t.Fatalf("ReadDir(ca/2026.5): %v", err)
	}
	for _, f := range files {
		if f.Name() == "pack.yaml" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			was, errWas := fs.ReadFile(packs.FS, "ca/2026.4/"+f.Name())
			is, errIs := fs.ReadFile(packs.FS, "ca/2026.5/"+f.Name())
			if errWas != nil || errIs != nil {
				t.Fatalf("read: got %v and %v, want the file in both versions", errWas, errIs)
			}
			if string(was) != string(is) {
				t.Error("got other bytes than 2026.4's, want the same file")
			}
		})
	}
}

// Canada 2026.6 is 2026.5 plus the GST/HST return (T61j): every file but
// pack.yaml is 2026.5's byte for byte and returns.yaml is new; the
// manifest is 2026.5's but for its version, the source of the return's
// lines and no sign-off (its header comment aside), as the tax specialist
// has not confirmed the return. It is the latest Canada pack and defines
// the return type GST34.
func TestCanadaPack2026_6(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ca := lib.Get("CA", "2026.5"), lib.Get("CA", "2026.6")
	if prev == nil || ca == nil {
		t.Fatalf("Get(CA, 2026.5) and Get(CA, 2026.6): got %v and %v, want both", prev, ca)
	}
	if ca.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist confirms the return")
	}
	if got := lib.Latest("CA"); got != ca {
		t.Errorf("Latest(CA): got %s, want ca/2026.6", got.Dir())
	}
	if ca.Return("GST34") == nil || len(prev.Returns.Returns) != 0 {
		t.Errorf("returns: got %d in 2026.5 and GST34 %v in 2026.6, want none and the return", len(prev.Returns.Returns), ca.Return("GST34") != nil)
	}
	if got, want := ca.Manifest.Sources[0].URL, gst34Source; got != want {
		t.Errorf("the first source: got %s, want %s", got, want)
	}
	same := *ca
	same.Manifest.Version, same.Manifest.SignOff, same.Manifest.Sources = prev.Manifest.Version, prev.Manifest.SignOff, ca.Manifest.Sources[1:]
	same.Returns = prev.Returns
	if !reflect.DeepEqual(&same, prev) {
		t.Error("2026.6 without its version, sign-off, the return and its source: got other than 2026.5, want it unchanged")
	}
	files, err := fs.ReadDir(packs.FS, "ca/2026.6")
	if err != nil {
		t.Fatalf("ReadDir(ca/2026.6): %v", err)
	}
	for _, f := range files {
		if f.Name() == "pack.yaml" || f.Name() == "returns.yaml" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			was, errWas := fs.ReadFile(packs.FS, "ca/2026.5/"+f.Name())
			is, errIs := fs.ReadFile(packs.FS, "ca/2026.6/"+f.Name())
			if errWas != nil || errIs != nil {
				t.Fatalf("read: got %v and %v, want the file in both versions", errWas, errIs)
			}
			if string(was) != string(is) {
				t.Error("got other bytes than 2026.5's, want the same file")
			}
		})
	}
}

// gst34Source is the CRA's instructions for the GST/HST return, which
// ADR-0049 records the lines from.
const gst34Source = "https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/calculate-prepare-report/instructions-preparing-return.html"

var pendingLine = regexp.MustCompile(`^\| ([0-9]{3}[A-C]?) \| ([0-9a-z]+) \| ([^|]+) \| (ADR-0049|memory) \|$`)

// TestCanadaPackGST34: the GST34 of Canada 2026.6 has the lines of
// packs/ca/TAX-PENDING-T61j.md in its order, each box coded as the table
// codes it; a line whose sum is from memory says so in its label and is an
// expression, and the file cites the source the manifest lists.
func TestCanadaPackGST34(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	def := lib.Get("CA", "2026.6").Return("GST34")
	b, err := fs.ReadFile(packs.FS, "ca/TAX-PENDING-T61j.md")
	if err != nil {
		t.Fatalf("read the pending file: %v", err)
	}
	var rows [][]string
	for _, line := range strings.Split(string(b), "\n") {
		if m := pendingLine.FindStringSubmatch(line); m != nil {
			rows = append(rows, m)
		} else if strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| Line ") {
			t.Errorf("a table row the test cannot read: %q", line)
		}
	}
	if len(rows) != len(def.Boxes) {
		t.Fatalf("lines: got %d in the file and %d in the pack, want the same", len(rows), len(def.Boxes))
	}
	for i, r := range rows {
		box := def.Boxes[i]
		t.Run(r[1], func(t *testing.T) {
			if box.Code != r[2] || !strings.EqualFold(r[1], r[2]) {
				t.Errorf("box %d: got %s, want %s (line %s)", i, box.Code, r[2], r[1])
			}
			memory := strings.Contains(box.Label, "from memory, pending the tax specialist")
			if memory != (r[4] == "memory") || (memory && box.Select != nil) {
				t.Errorf("label %q and expression %v: got the basis %s, want a memory line marked in its label and computed from others", box.Label, box.Select == nil, r[4])
			}
		})
	}
	if !strings.Contains(string(b), gst34Source) {
		t.Errorf("the pending file: got no link to %s, want the source", gst34Source)
	}
}
