package config

import (
	"regexp"
	"slices"
	"strings"
)

var (
	ledgerCodeRe = regexp.MustCompile(`^[0-9A-Z]{2}$`)
	groupCodeRe  = regexp.MustCompile(`^[0-9A-Z]{1,4}$`)
	principleRe  = regexp.MustCompile(`^[A-Z0-9_]{2,20}$`)
)

// ledgers checks the parallel ledgers (T52a, ADR-0045): valid, unique
// ledgers with the leading ledger 0L among them, companies keeping known
// ledgers with 0L, and groups of known ledgers whose representative is a
// member, the leading ledger when it is one. What is activated is checked
// against the tenant's data when a change is submitted (ledger.Setup).
func (v *validator) ledgers(c *Config) {
	known := map[string]bool{}
	for _, l := range c.Ledgers {
		p := "ledgers[" + l.Code + "]"
		switch {
		case !ledgerCodeRe.MatchString(l.Code):
			v.err(p+".code", "ledger codes are two upper-case letters or digits")
		case known[l.Code]:
			v.err(p, "duplicate ledger")
		}
		known[l.Code] = true
		v.ledgerName(p, l.Name)
		if !principleRe.MatchString(l.AccountingPrinciple) {
			v.err(p+".accounting_principle", "accounting principle %q must be 2-20 upper-case letters, digits or underscores (IFRS, US_GAAP, HGB)", l.AccountingPrinciple)
		}
	}
	if len(c.Ledgers) > 0 && !known[LeadingLedger] {
		v.err("ledgers", "the leading ledger %s is required", LeadingLedger)
	}
	known[LeadingLedger] = true
	for _, co := range c.Companies {
		v.codes("companies["+co.Code+"].ledgers", co.Ledgers, known)
		if len(co.Ledgers) > 0 && !slices.Contains(co.Ledgers, LeadingLedger) {
			v.err("companies["+co.Code+"].ledgers", "every company keeps the leading ledger %s", LeadingLedger)
		}
	}
	v.ledgerGroups(c, known)
}

// ledgerGroups checks the groups against the known ledgers. A group's code
// differs from every ledger's, so a posting can name either (T52b).
func (v *validator) ledgerGroups(c *Config, known map[string]bool) {
	seen := map[string]bool{}
	for _, g := range c.LedgerGroups {
		p := "ledger_groups[" + g.Code + "]"
		switch {
		case !groupCodeRe.MatchString(g.Code):
			v.err(p+".code", "ledger group codes are 1-4 upper-case letters or digits")
		case seen[g.Code]:
			v.err(p, "duplicate ledger group")
		case known[g.Code]:
			v.err(p+".code", "a ledger group's code differs from every ledger's")
		}
		seen[g.Code] = true
		v.ledgerName(p, g.Name)
		v.codes(p+".ledgers", g.Ledgers, known)
		switch {
		case !slices.Contains(g.Ledgers, g.Representative):
			v.err(p+".representative", "the representative ledger %q is one of the group's ledgers", g.Representative)
		case slices.Contains(g.Ledgers, LeadingLedger) && g.Representative != LeadingLedger:
			v.err(p+".representative", "a group with the leading ledger has it as its representative")
		}
	}
}

// codes checks a list of ledger codes: each known and named once.
func (v *validator) codes(p string, codes []string, known map[string]bool) {
	seen := map[string]bool{}
	for _, code := range codes {
		switch {
		case !known[code]:
			v.err(p+"["+code+"]", "no ledger %s", code)
		case seen[code]:
			v.err(p+"["+code+"]", "ledger %s is named twice", code)
		}
		seen[code] = true
	}
}

// ledgerName checks a ledger's or group's name: 1-200 characters, as the
// database holds them.
func (v *validator) ledgerName(p, name string) {
	switch n := len([]rune(strings.TrimSpace(name))); {
	case n == 0:
		v.err(p+".name", "name is required")
	case n > 200:
		v.err(p+".name", "names are at most 200 characters")
	}
}
