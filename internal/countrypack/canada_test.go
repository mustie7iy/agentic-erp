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
// write it: its rates as "rate from..to" (to empty when open) and its
// recovery as "account_key account".
type pendingCode struct {
	rates    []string
	recovery string
}

var (
	pendingRow  = regexp.MustCompile(`^\| ([A-Z0-9]+) \| [^|]+ \| ([^|]+) \| ([^|]+) \| ([^|]+) \| [^|]+ \|$`)
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
		codes[m[1]] = pendingCode{rates: pendingRates(t, m[1], m[2], m[3]), recovery: pendingRecovery(t, m[1], m[4])}
	}
	return codes, urls
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
		return config.KeyInput + " " + m[1]
	}
	if strings.HasPrefix(s, "none (expensed") {
		// The schema's non-deductible component has no account: its tax
		// is added to the purchase's own line (ADR-0049 §3, §6).
		return config.KeyNonDeductible + " "
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
	out.recovery = p.AccountKey + " " + p.Account
	return out
}

// Canada 2026.3 writes the tax codes of packs/ca/TAX-PENDING-T61a.md
// exactly: the same codes, each one component with the file's rates and
// periods and its proposed recovery, and every source the file cites
// (ADR-0048, T62g).
func TestCanadaPackTaxCodes(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.3")
	if ca == nil {
		t.Fatal("Get(CA, 2026.3): got nil, want the Canada pack with tax codes")
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
			if p.recovery != want[c.Code].recovery {
				t.Errorf("recovery: got %q, want %q", p.recovery, want[c.Code].recovery)
			}
			if c.DirectionOrInput() != config.TaxInput || c.KindOrStandard() != config.TaxStandard {
				t.Errorf("code: got %s %s, want an input standard code", c.DirectionOrInput(), c.KindOrStandard())
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

// Canada 2026.3 is 2026.2 plus the tax codes: the cash.yaml liquidity
// items stay (a version without them would drop them from tenants that
// adopted 2026.2), the sources are 2026.2's, and it is not signed off.
func TestCanadaPack2026_3(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ca := lib.Get("CA", "2026.2"), lib.Get("CA", "2026.3")
	for name, same := range map[string]bool{
		"accounts":  reflect.DeepEqual(prev.Accounts, ca.Accounts),
		"calendar":  reflect.DeepEqual(prev.Calendar, ca.Calendar),
		"numbering": reflect.DeepEqual(prev.Numbering, ca.Numbering),
		"payments":  reflect.DeepEqual(prev.Payments, ca.Payments),
		"cash":      reflect.DeepEqual(prev.Cash, ca.Cash),
		"sources":   reflect.DeepEqual(prev.Manifest.Sources, ca.Manifest.Sources),
	} {
		if !same {
			t.Errorf("%s: got other than 2026.2's, want them unchanged", name)
		}
	}
	if ca.SignedOff() {
		t.Error("SignedOff: got true, want false until the tax specialist signs a version off")
	}
	if lib.Latest("CA") != ca {
		t.Errorf("Latest(CA): got %s, want 2026.3", lib.Latest("CA").Dir())
	}
}
