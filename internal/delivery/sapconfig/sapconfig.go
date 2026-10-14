// Package sapconfig generates a draft configuration document (03-data-model
// §3.5) from a client's SAP customizing tables (mapped as in §3.17), so
// years of setup are migrated instead of re-keyed. The draft is a proposal
// for a change request: anything without an SAP equivalent gets a documented
// default and a note for the reviewers. Roles and users are not part of the
// document (the identity context owns them); the notes list the SAP roles to
// provision there.
package sapconfig

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// RangeRule classifies accounts From <= number <= To, compared numerically.
type RangeRule struct {
	From, To string
	Type     config.AccountType
}

// Options carry the client-specific mapping decisions.
type Options struct {
	// AccountTypes classify G/L accounts by number range (after ALPHA conversion).
	AccountTypes []RangeRule
	// RequisitionApprover, ReleaseRole and VendorApproverRole name the
	// identity roles the approval policies route to.
	RequisitionApprover, ReleaseRole, VendorApproverRole string
	AutoPostLimit                                        string
}

// DefaultOptions suit a standard S/4HANA procure-to-pay role design.
func DefaultOptions() Options {
	return Options{
		AccountTypes: []RangeRule{
			{From: "100000", To: "199999", Type: config.Asset},
			{From: "200000", To: "299999", Type: config.Liability},
			{From: "300000", To: "399999", Type: config.Equity},
			{From: "400000", To: "499999", Type: config.Revenue},
			{From: "500000", To: "999999", Type: config.Expense},
		},
		RequisitionApprover: "Z_PURCH_MGR", ReleaseRole: "Z_TREASURY_MGR", VendorApproverRole: "vendor_master_approver",
		AutoPostLimit: "50000.00",
	}
}

// accountType classifies an account number.
func (o Options) accountType(number string) (config.AccountType, error) {
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return "", fmt.Errorf("account %q is not numeric; add an explicit mapping rule", number)
	}
	for _, r := range o.AccountTypes {
		from, err1 := strconv.ParseInt(r.From, 10, 64)
		to, err2 := strconv.ParseInt(r.To, 10, 64)
		if err1 == nil && err2 == nil && n >= from && n <= to {
			return r.Type, nil
		}
	}
	return "", fmt.Errorf("no account type rule covers account %s", number)
}

// Note explains a generated setting reviewers should look at.
type Note struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (n Note) String() string { return n.Path + ": " + n.Message }

// documentTypes get number ranges in every company and fiscal year of the
// extract, besides one journal range per SAP document type it holds.
var documentTypes = map[string]string{"journal_sa": "", "journal_ab": "", "journal_kr": "", "journal_zp": "", "journal_ob": "",
	"requisition": "PR", "purchase_order": "PO", "contract": "CT", "goods_receipt": "GR", "supplier_invoice": "SI", "payment_run": "PY",
	"partner_change": ""}

// Generate builds a draft configuration; validate it before proposing it.
func Generate(e *sap.Extract, opts Options) (*config.Config, []Note) {
	g := &gen{e: e, opts: opts}
	c := &config.Config{Version: 1, Output: config.Output{PurchaseOrderChannel: "email"}}
	for _, t := range e.T001 {
		c.Companies = append(c.Companies, g.company(t))
	}
	c.PaymentTerms = g.paymentTerms()
	approver := opts.RequisitionApprover
	g.note("purchasing.requisition_approval", "SAP release strategies are not in the extract; generated two tiers (up to 10,000: "+approver+
		"; above: "+approver+" then process_owner)")
	c.Purchasing = config.Purchasing{RequisitionApproval: []config.ApprovalTier{
		{UpTo: "10000.00", Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}, {Role: "process_owner", Approvals: 1}}},
	}}
	c.Payments = config.Payments{ReleaseRole: opts.ReleaseRole, ReleaseApprovals: 1, BankChangeHoldDays: 5, VendorApproverRole: opts.VendorApproverRole}
	g.note("payments.bank_change_hold_days", "no SAP equivalent; payments to a vendor are held for 5 days after a bank change")
	c.NumberRanges = g.numberRanges()
	g.roles(c)
	return c, g.notes
}

type gen struct {
	e     *sap.Extract
	opts  Options
	notes []Note
}

func (g *gen) note(path, msg string) { g.notes = append(g.notes, Note{path, msg}) }

// unknownDays marks a payment term whose SAP days could not be read. Any
// guess would be wrong in a way that pays early or late (0 days would
// propose every invoice under the term on its document date), so the
// draft carries a value config.Validate refuses ("must be 0-365" on
// payment_terms[KEY].net_days) until a reviewer sets the real one.
const unknownDays = -1

// paymentTerms is SAP's payment terms (T052), or a net-30 placeholder with
// a note when the extract has none. Days are whole numbers in SAP; a term
// whose days do not parse gets unknownDays and a note.
func (g *gen) paymentTerms() []config.PaymentTerm {
	var out []config.PaymentTerm
	for _, t := range g.e.T052 {
		days, err := strconv.Atoi(strings.TrimSpace(t.ZTAG1))
		if err != nil {
			days = unknownDays
			g.note("payment_terms["+t.ZTERM+"].net_days", fmt.Sprintf("T052 days %q are not a whole number; set the term's days (left at %d, which fails validation)", t.ZTAG1, unknownDays))
		}
		out = append(out, config.PaymentTerm{Key: t.ZTERM, Description: t.TEXT1, NetDays: days})
	}
	if len(out) == 0 {
		out = []config.PaymentTerm{{Key: "NT30", Description: "Net 30 days", NetDays: 30}}
		g.note("payment_terms", "T052 not in extract; added NT30 as a placeholder")
	}
	return out
}

// wholePercent parses an SAP percentage ("2", "2.0", "2.5", ".5", or
// "0,50" with the decimal comma of locale-formatted file extracts) to the
// whole percent the configuration holds, dropping any fraction (a lower
// price tolerance sends more invoices to a person, never fewer). exact is
// false when a fraction was dropped; problem says why s is not a usable
// percentage ("is not a number", "is out of range") and is "" when it is.
func wholePercent(s string) (pct int, exact bool, problem string) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1) // a lone comma is the decimal separator
	}
	whole, frac, _ := strings.Cut(s, ".")
	if strings.Trim(frac, "0123456789") != "" || whole == "" && frac == "" {
		return 0, false, "is not a number"
	}
	if whole == "" {
		whole = "0" // ".5" is how some systems write 0.5
	}
	pct, err := strconv.Atoi(whole)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return 0, false, "is out of range"
	case err != nil:
		return 0, false, "is not a number"
	}
	return pct, strings.Trim(frac, "0") == "", ""
}

func (g *gen) company(t sap.T001) config.Company {
	p := fmt.Sprintf("companies[%s]", t.BUKRS)
	co := config.Company{
		Code: t.BUKRS, Name: t.BUTXT, Currency: t.WAERS, Country: t.LAND1, FiscalYearStartMonth: 1,
		AutoPostLimit: g.opts.AutoPostLimit, OverDeliveryPercent: 0,
	}
	g.note(p+".fiscal_year_start_month", "fiscal year variant not in extract; assumed calendar year (K4)")
	g.note(p+".auto_post_limit", "no SAP equivalent; invoices above "+g.opts.AutoPostLimit+" always go to a person")
	co.ChartOfAccounts = g.chartOfAccounts(p, t.BUKRS)
	for _, r := range g.e.T030 {
		if r.BUKRS != t.BUKRS {
			continue
		}
		switch r.KTOSL {
		case "VST":
			co.AccountDetermination.InputTax = sap.Alpha(r.KONTS)
		case "BNK":
			co.AccountDetermination.Bank = sap.Alpha(r.KONTS)
		}
	}
	co.AccountDetermination.APReconciliation = g.apReconciliation(p, t.BUKRS)
	for _, tx := range g.e.T007A {
		if tx.BUKRS == t.BUKRS {
			co.TaxCodes = append(co.TaxCodes, config.TaxCode{Code: tx.MWSKZ, Description: tx.TEXT1, RatePercent: tx.KBETR, Account: sap.Alpha(tx.KONTS)})
		}
	}
	co.InvoiceTolerance = g.invoiceTolerance(p, t.BUKRS)
	for _, b := range g.e.T012K {
		if b.BUKRS == t.BUKRS {
			co.HouseBank = config.HouseBank{Name: b.HBKID, IBAN: b.IBAN, BIC: b.SWIFT}
			if co.AccountDetermination.Bank == "" {
				co.AccountDetermination.Bank = sap.Alpha(b.HKONT)
			}
			break
		}
	}
	return co
}

// chartOfAccounts is the company's G/L accounts; an account whose type the
// options cannot tell is left out with a note.
func (g *gen) chartOfAccounts(p, company string) []config.Account {
	var out []config.Account
	for _, a := range g.e.SKB1 {
		if a.BUKRS != company {
			continue
		}
		num := sap.Alpha(a.SAKNR)
		typ, err := g.opts.accountType(num)
		if err != nil {
			g.note(p+".chart_of_accounts["+num+"]", err.Error())
			continue
		}
		out = append(out, config.Account{Number: num, Name: a.TXT50, Type: typ, VendorReconciliation: a.MITKZ == "K"})
	}
	return out
}

// apReconciliation is the account most vendors in this company post to.
func (g *gen) apReconciliation(p, company string) string {
	counts := map[string]int{}
	for _, v := range g.e.LFB1 {
		if v.BUKRS == company {
			counts[sap.Alpha(v.AKONT)]++
		}
	}
	best := ""
	for acct, n := range counts {
		if n > counts[best] || (n == counts[best] && acct < best) {
			best = acct
		}
	}
	if len(counts) > 1 {
		g.note(p+".account_determination.ap_reconciliation", fmt.Sprintf("vendors use %d reconciliation accounts; %s is the most common", len(counts), best))
	}
	return best
}

// invoiceTolerance is the company's price tolerance (T169G key PP), or 2%
// and 5.00 with a note when SAP has none. A percentage that does not parse
// keeps the 2% default and one with a fraction is cut to whole percent,
// each with a note.
func (g *gen) invoiceTolerance(p, company string) config.Tolerance {
	tolerance := config.Tolerance{PricePercent: 2, PriceAbsolute: "5.00"}
	found := false
	for _, tol := range g.e.T169G {
		if tol.BUKRS != company || tol.TOLSL != "PP" {
			continue
		}
		tolerance, found = config.Tolerance{PricePercent: 2, PriceAbsolute: tol.WERTO}, true
		pct, exact, problem := wholePercent(tol.PROZ)
		switch {
		case problem != "":
			g.note(p+".invoice_tolerance.price_percent", fmt.Sprintf("T169G key PP percentage %q %s; used 2%%", tol.PROZ, problem))
			continue
		case !exact:
			g.note(p+".invoice_tolerance.price_percent", fmt.Sprintf("T169G key PP percentage %q is not a whole percent; used %d%%", tol.PROZ, pct))
		}
		tolerance.PricePercent = pct
	}
	if !found {
		g.note(p+".invoice_tolerance", "T169G key PP not found; used 2% / 5.00")
	}
	return tolerance
}

// numberRanges gives every company a range per document type in each
// fiscal year the extract's journal covers, so the draft can number
// migrated and new documents.
func (g *gen) numberRanges() []config.NumberRange {
	years, types := map[string]bool{}, map[string]string{}
	for k, v := range documentTypes {
		types[k] = v
	}
	for _, l := range g.e.ACDOCA {
		years[l.GJAHR] = true
		types["journal_"+strings.ToLower(l.BLART)] = ""
	}
	if len(years) == 0 {
		g.note("number_ranges", "the extract has no journal; add number ranges for the fiscal years to migrate")
	}
	var out []config.NumberRange
	for _, t := range g.e.T001 {
		for _, y := range sortedKeys(years) {
			year, _ := strconv.Atoi(y)
			for _, dt := range sortedKeys(types) {
				out = append(out, config.NumberRange{Company: t.BUKRS, DocumentType: dt, FiscalYear: year, Prefix: types[dt], First: 100000, Last: 999999})
			}
		}
	}
	return out
}

// roles notes the roles the document routes approvals to, and the SAP
// roles with their users, to provision in the identity context.
func (g *gen) roles(c *config.Config) {
	g.note("roles", "provision these identity roles before activating: "+strings.Join(c.RoleNames(), ", "))
	users := map[string][]string{}
	for _, u := range g.e.AGR_USERS {
		if !slices.Contains(users[u.AGR_NAME], u.UNAME) {
			users[u.AGR_NAME] = append(users[u.AGR_NAME], u.UNAME)
		}
	}
	for _, d := range g.e.AGR_DEFINE {
		g.note("roles["+d.AGR_NAME+"]", fmt.Sprintf("SAP role %q (users: %s): map its permissions in identity", d.TEXT, strings.Join(users[d.AGR_NAME], ", ")))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
