package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Tier is the risk class of a change; it decides who must approve it.
type Tier int

const (
	TierCosmetic       Tier = 1 // texts and descriptions
	TierOperational    Tier = 2 // routes, tolerances, terms
	TierPostingRelated Tier = 3 // account determination, tax, authorizations
	TierStructural     Tier = 4 // companies, currencies, chart of accounts
)

func (t Tier) String() string {
	switch t {
	case TierCosmetic:
		return "tier 1 (cosmetic)"
	case TierOperational:
		return "tier 2 (operational)"
	case TierPostingRelated:
		return "tier 3 (posting-relevant)"
	case TierStructural:
		return "tier 4 (structural)"
	}
	return "no change"
}

// Change is one setting that differs between two configurations.
type Change struct {
	Path string `json:"path"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
	Tier Tier   `json:"tier"`
}

func (c Change) String() string {
	switch {
	case c.Old == "":
		return fmt.Sprintf("+ %s = %s", c.Path, c.New)
	case c.New == "":
		return fmt.Sprintf("- %s (was %s)", c.Path, c.Old)
	}
	return fmt.Sprintf("~ %s: %s -> %s", c.Path, c.Old, c.New)
}

// tierRules map path patterns to tiers; the first match wins. Paths use
// [key] for list items, matched here as [*]. Anything unmatched is treated
// as posting-relevant, the safe default.
var tierRules = []struct {
	pattern *regexp.Regexp
	tier    Tier
}{
	{rx(`^companies\[\*\]$`), TierStructural},
	{rx(`^companies\[\*\]\.(code|currency|fiscal_year_start_month)$`), TierStructural},
	{rx(`^companies\[\*\]\.chart_of_accounts\[\*\]\.name$`), TierCosmetic},
	{rx(`^companies\[\*\]\.ledgers`), TierStructural},
	// Flagging an account for revaluation changes what runs post (ADR-0042).
	{rx(`^companies\[\*\]\.chart_of_accounts\[\*\]\.revalue_foreign_currency$`), TierPostingRelated},
	{rx(`^companies\[\*\]\.chart_of_accounts`), TierStructural},
	{rx(`^companies\[\*\]\.(name|country)$`), TierCosmetic},
	{rx(`^companies\[\*\]\.tax_codes\[\*\]\.description$`), TierCosmetic},
	{rx(`^companies\[\*\]\.withholding_tax_codes\[\*\]\.description$`), TierCosmetic},
	{rx(`^companies\[\*\]\.payment_methods\[\*\]\.description$`), TierCosmetic},
	{rx(`^companies\[\*\]\.bank_statements\.posting_rules\[\*\]\.description$`), TierCosmetic},
	{rx(`^companies\[\*\]\.withholding_tax_codes`), TierPostingRelated},
	{rx(`^companies\[\*\]\.(account_determination|tax_codes|auto_post_limit|manual_entry_approval_limit|house_bank)`), TierPostingRelated},
	{rx(`^companies\[\*\]\.(invoice_tolerance|gr_over_delivery_percent)`), TierOperational},
	{rx(`^(ledgers|ledger_groups)\[\*\]\.name$`), TierCosmetic},
	{rx(`^ledgers`), TierStructural},
	// A group decides which ledgers a posting naming it reaches (T52b).
	{rx(`^ledger_groups`), TierPostingRelated},
	{rx(`^payment_terms\[\*\]\.description$`), TierCosmetic},
	// Discounts change what payments post (cash discount and tax).
	{rx(`^payment_terms\[\*\]\.discounts`), TierPostingRelated},
	{rx(`^payment_terms`), TierOperational},
	{rx(`^purchasing\.`), TierOperational},
	{rx(`^payments\.bank_change_hold_days$`), TierOperational},
	{rx(`^payments\.`), TierPostingRelated},
	{rx(`^output\.`), TierOperational},
	// Number ranges need no approval: like SAP, which treats number range
	// intervals as current settings maintained directly in production.
	// Submission only lets new ranges and raised upper limits through, so
	// issued numbers stay valid.
	{rx(`^number_ranges`), TierCosmetic},
	// A rule refuses writes: tier 3, with :simulate evidence (ADR-0046).
	{rx(`^rules\.`), TierPostingRelated},
	// A metric's or dimension's words change no value; its definition
	// changes every report computing it, so it is simulated (Evidence)
	// and its owner approves it.
	{rx(`^analytics\.metrics\[\*\]\.(label|description|synonyms)`), TierCosmetic},
	{rx(`^analytics\.dimensions\[\*\]\.(label|description)$`), TierCosmetic},
	{rx(`^analytics(\.|$)`), TierOperational},
	// Which pack version a company adopted posts nothing itself; the
	// settings applying it changes carry their own tiers. It decides what
	// the next upgrade merges from, so it is not cosmetic (ADR-0048).
	{rx(`^packs`), TierOperational},
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var keyRe = regexp.MustCompile(`\[[^\]]*\]`)

func classify(path string) Tier {
	generic := keyRe.ReplaceAllString(path, "[*]")
	for _, r := range tierRules {
		if r.pattern.MatchString(generic) {
			return r.tier
		}
	}
	return TierPostingRelated
}

// listKeys names the identity field of each list, so items are compared by
// identity instead of position.
var listKeys = map[string]string{
	"companies": "code", "chart_of_accounts": "number", "tax_codes": "code", "withholding_tax_codes": "code",
	"house_banks": "name", "payment_methods": "code", "ranking": "currency",
	// Keys are list names wherever the list is: "accounts" is today only
	// fx_valuation.accounts, and a list of that name elsewhere shares it.
	"accounts":      "account",
	"posting_rules": "transaction_code", "matching_rules": "kind",
	"payment_terms": "key", "ledgers": "code", "ledger_groups": "code",
	"number_ranges": "company,document_type,fiscal_year",
	// The analytics section's; a metric's own dimensions are names, listed
	// by position.
	"metrics": "name", "dimensions": "name",
	"packs": "company",
}

// Diff lists every setting that differs from old to new, with its tier.
// A list item that was added or removed as a whole (a company, an account,
// a user) is reported once, at the item's path, with the highest tier of
// anything inside it.
//
// The error result is always nil today; it is kept for API stability.
func Diff(old, new *Config) ([]Change, error) {
	a, b := flatten(old), flatten(new)
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	var raw []Change
	for p := range paths {
		if a[p] != b[p] {
			raw = append(raw, Change{Path: p, Old: a[p], New: b[p], Tier: classify(p)})
		}
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Path < raw[j].Path })

	var out []Change
	reported := map[string]bool{}
	for _, c := range raw {
		item := wholeItem(c.Path, a, b)
		if item == "" {
			out = append(out, c)
			continue
		}
		if reported[item] {
			continue
		}
		reported[item] = true
		ch := Change{Path: item, Tier: classify(item)}
		for _, other := range raw {
			if strings.HasPrefix(other.Path, item+".") || strings.HasPrefix(other.Path, item+"[") {
				ch.Tier = maxTier(ch.Tier, other.Tier)
			}
		}
		if hasPrefixKey(a, item) {
			ch.Old = "(removed)"
		} else {
			ch.New = "(added)"
		}
		out = append(out, ch)
	}
	return append(out, extensionChanges(old, new)...), nil
}

// wholeItem returns the outermost list item containing path that exists in
// only one of the two configurations, or "" if there is none.
func wholeItem(path string, a, b map[string]string) string {
	for i := 0; i < len(path); i++ {
		if path[i] != ']' || i == len(path)-1 {
			continue
		}
		item := path[:i+1]
		if hasPrefixKey(a, item) != hasPrefixKey(b, item) {
			return item
		}
	}
	return ""
}

func hasPrefixKey(m map[string]string, item string) bool {
	for k := range m {
		if k == item || strings.HasPrefix(k, item+".") || strings.HasPrefix(k, item+"[") {
			return true
		}
	}
	return false
}

func maxTier(a, b Tier) Tier {
	if a > b {
		return a
	}
	return b
}

// MaxTier is the tier of a change set: its riskiest change.
func MaxTier(changes []Change) Tier {
	t := Tier(0)
	for _, c := range changes {
		t = maxTier(t, c.Tier)
	}
	return t
}

// flatten renders a configuration as path -> value. Config contains only
// JSON-safe types, so the round trip through JSON cannot fail.
func flatten(c *Config) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	flat := *c
	flat.Extensions = nil // compared field by field by extensionChanges
	b, _ := json.Marshal(flat)
	var v any
	_ = json.Unmarshal(b, &v)
	walk("", "", v, out)
	return out
}

func walk(path, field string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walk(p, k, x[k], out)
		}
	case []any:
		key := listKeys[field]
		for i, item := range x {
			id := itemID(key, i, item)
			walk(fmt.Sprintf("%s[%s]", path, id), "", item, out)
		}
	case nil:
	default:
		out[path] = fmt.Sprint(x)
	}
}

// itemID is a list item's identity: the fields key names of an object, a
// code itself in a list of codes (a company's ledgers), else its position.
func itemID(key string, i int, item any) string {
	if s, ok := item.(string); ok && key != "" {
		return s
	}
	m, ok := item.(map[string]any)
	if !ok || key == "" {
		return fmt.Sprint(i)
	}
	var parts []string
	for _, k := range strings.Split(key, ",") {
		parts = append(parts, fmt.Sprint(m[k]))
	}
	return strings.Join(parts, "/")
}
