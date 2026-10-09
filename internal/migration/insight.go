package migration

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// AccountProposal is a suggested classification for one G/L account, with
// how confident the evidence makes it.
type AccountProposal struct {
	CompanyCode string           `json:"companyCode"`
	Account     string           `json:"account"`
	Name        string           `json:"name"`
	RuleType    core.AccountType `json:"ruleType,omitempty"` // what the number-range mapping says
	Proposed    core.AccountType `json:"proposed"`
	Confidence  string           `json:"confidence"` // high | medium | low
	Reason      string           `json:"reason"`
}

var nameSignals = []struct {
	words []string
	typ   core.AccountType
}{
	{[]string{"revenue", "sales", "income", "turnover"}, core.Revenue},
	{[]string{"payable", "accrued", "accrual", "loan", "provision"}, core.Liability},
	{[]string{"capital", "equity", "retained"}, core.Equity},
	{[]string{"bank", "cash", "receivable", "input tax", "inventory", "prepaid"}, core.Asset},
	{[]string{"expense", "cost", "consumed", "supplies", "services", "freight", "salar", "wage", "rent", "depreciation"}, core.Expense},
}

func balanceSheet(t core.AccountType) bool {
	return t == core.Asset || t == core.Liability || t == core.Equity
}

// ProposeAccountTypes cross-checks the number-range rules against what SAP
// itself says about each account (balance-sheet indicator, reconciliation
// type) and its name. Disagreements come back with low confidence so a
// person decides.
func ProposeAccountTypes(e *sap.Extract, m Mapping) []AccountProposal {
	var out []AccountProposal
	for _, a := range e.SKB1 {
		p := AccountProposal{CompanyCode: a.BUKRS, Account: alpha(a.SAKNR), Name: a.TXT50}
		rule, ruleErr := m.AccountType(p.Account)
		if ruleErr == nil {
			p.RuleType = rule
		}
		var signal core.AccountType
		lname := strings.ToLower(a.TXT50)
		for _, s := range nameSignals {
			for _, w := range s.words {
				if strings.Contains(lname, w) {
					signal = s.typ
					break
				}
			}
			if signal != "" {
				break
			}
		}
		bs := a.XBILK == "X"
		var reasons []string
		switch {
		case a.MITKZ == "K":
			p.Proposed, p.Confidence = core.Liability, "high"
			reasons = append(reasons, "SAP marks it as a vendor reconciliation account")
			if ruleErr == nil && rule != core.Liability {
				p.Confidence = "low"
				reasons = append(reasons, fmt.Sprintf("but the number-range rule says %s", rule))
			}
		case ruleErr != nil:
			p.Proposed, p.Confidence = signal, "low"
			reasons = append(reasons, "no number-range rule covers it")
			if signal != "" {
				reasons = append(reasons, fmt.Sprintf("name suggests %s", signal))
			}
		case balanceSheet(rule) != bs:
			p.Proposed, p.Confidence = rule, "low"
			reasons = append(reasons, fmt.Sprintf("rule says %s but SAP's balance-sheet indicator is %q", rule, a.XBILK))
			if signal != "" && balanceSheet(signal) == bs {
				p.Proposed = signal
				reasons = append(reasons, fmt.Sprintf("name suggests %s, which agrees with SAP", signal))
			}
		case signal != "" && signal != rule:
			p.Proposed, p.Confidence = rule, "medium"
			reasons = append(reasons, fmt.Sprintf("rule says %s, name suggests %s", rule, signal))
		case signal == rule:
			p.Proposed, p.Confidence = rule, "high"
			reasons = append(reasons, "rule, SAP indicator and name agree")
		default:
			p.Proposed, p.Confidence = rule, "medium"
			reasons = append(reasons, "rule and SAP indicator agree; the name gives no signal")
		}
		p.Reason = strings.Join(reasons, "; ")
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CompanyCode != out[j].CompanyCode {
			return out[i].CompanyCode < out[j].CompanyCode
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// DocDifference compares one SAP document's effect on an account (or a
// vendor's open items) with the migrated journal entry.
type DocDifference struct {
	Document string      `json:"document"`
	SAP      money.Minor `json:"sap"`
	Core     money.Minor `json:"core"`
	Status   string      `json:"status"` // missing_in_core | only_in_core | amount_differs
}

// ExplainAccount lists the documents behind a trial-balance difference on
// one account, so a controller can see exactly which postings disagree.
func ExplainAccount(e *sap.Extract, s *core.Store, m Mapping, company, account string) ([]DocDifference, error) {
	currency := companyCurrency(e, company)
	sapDocs := map[string]money.Minor{}
	for _, l := range e.ACDOCA {
		if l.RBUKRS != company || alpha(l.RACCT) != account {
			continue
		}
		amt, err := m.parseAmount(l.HSL, currency)
		if err != nil {
			return nil, err
		}
		sapDocs[entryID(l.RBUKRS, l.GJAHR, l.BELNR)] += amt
	}
	coreDocs := map[string]money.Minor{}
	for _, je := range s.Journal {
		if je.CompanyCode != company {
			continue
		}
		for _, l := range je.Lines {
			if l.Account == account {
				coreDocs[je.ID] += l.Amount
			}
		}
	}
	return compareDocs(sapDocs, coreDocs), nil
}

// ExplainVendor does the same for a vendor's open items.
func ExplainVendor(e *sap.Extract, s *core.Store, m Mapping, company, vendor string) ([]DocDifference, error) {
	currency := companyCurrency(e, company)
	recon := map[string]bool{}
	for _, a := range e.SKB1 {
		if a.BUKRS == company && a.MITKZ == "K" {
			recon[a.SAKNR] = true
		}
	}
	sapDocs := map[string]money.Minor{}
	for _, l := range e.ACDOCA {
		if l.RBUKRS != company || alpha(l.LIFNR) != vendor || l.AUGBL != "" || !recon[l.RACCT] {
			continue
		}
		amt, err := m.parseAmount(l.HSL, currency)
		if err != nil {
			return nil, err
		}
		sapDocs[entryID(l.RBUKRS, l.GJAHR, l.BELNR)] += amt
	}
	coreDocs := map[string]money.Minor{}
	for _, it := range s.VendorOpenItems(company)[vendor] {
		coreDocs[it.JournalEntryID] += it.Amount
	}
	return compareDocs(sapDocs, coreDocs), nil
}

func compareDocs(sapDocs, coreDocs map[string]money.Minor) []DocDifference {
	keys := map[string]bool{}
	for k := range sapDocs {
		keys[k] = true
	}
	for k := range coreDocs {
		keys[k] = true
	}
	var out []DocDifference
	for _, k := range sortedKeys(keys) {
		sv, inSAP := sapDocs[k]
		cv, inCore := coreDocs[k]
		switch {
		case inSAP && !inCore:
			out = append(out, DocDifference{Document: k, SAP: sv, Status: "missing_in_core"})
		case !inSAP && inCore:
			out = append(out, DocDifference{Document: k, Core: cv, Status: "only_in_core"})
		case sv != cv:
			out = append(out, DocDifference{Document: k, SAP: sv, Core: cv, Status: "amount_differs"})
		}
	}
	return out
}

func companyCurrency(e *sap.Extract, company string) string {
	for _, c := range e.T001 {
		if c.BUKRS == company {
			return c.WAERS
		}
	}
	return ""
}

// CleansingProposal is a suggested fix for a data-quality finding, for a
// data owner to approve and carry out in SAP before cutover.
type CleansingProposal struct {
	Finding string `json:"finding"`
	Action  string `json:"action"`
	Owner   string `json:"owner"`
}

// ProposeCleansing turns data-quality findings into concrete actions.
func ProposeCleansing(e *sap.Extract, issues []Issue) []CleansingProposal {
	openItems := map[string]int{}
	for _, l := range e.ACDOCA {
		if l.LIFNR != "" && l.AUGBL == "" {
			openItems[l.LIFNR]++
		}
	}
	var out []CleansingProposal
	for _, is := range issues {
		p := CleansingProposal{Finding: is.String()}
		switch {
		case strings.Contains(is.Message, "possible duplicate vendors"):
			ids := strings.Split(is.Key, ",")
			keep := ids[0]
			for _, id := range ids[1:] {
				if openItems[id] > openItems[keep] {
					keep = id
				}
			}
			var drop []string
			for _, id := range ids {
				if id != keep {
					drop = append(drop, id)
				}
			}
			p.Action = fmt.Sprintf("Confirm the duplicates; keep %s and block %s for posting after clearing their open items", keep, strings.Join(drop, ", "))
			p.Owner = "vendor master data"
		case strings.Contains(is.Message, "no tax number"):
			p.Action = fmt.Sprintf("Request the tax number from vendor %s and update LFA1-STCD1", is.Key)
			p.Owner = "vendor master data"
		case strings.Contains(is.Message, "posting-blocked but has open payables"):
			p.Action = fmt.Sprintf("Decide before cutover: pay, dispute or write off the open items of blocked vendor %s", is.Key)
			p.Owner = "accounts payable"
		case strings.Contains(is.Message, "will be held at invoice matching"):
			p.Action = fmt.Sprintf("Ask the vendor for a corrected invoice for %s, or approve the difference", is.Key)
			p.Owner = "accounts payable"
		case is.Severity == SevError:
			p.Action = "Correct the record in SAP; the load is blocked until it is fixed"
			p.Owner = "migration team"
		default:
			p.Action = "Review and record a decision"
			p.Owner = "migration team"
		}
		out = append(out, p)
	}
	return out
}
