// Package sapconfig generates a draft configuration from a client's SAP
// customizing tables, so years of setup are migrated instead of re-keyed.
// The draft is a proposal: anything without an SAP equivalent gets a
// documented default and a note for the reviewers.
package sapconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Options carry the client-specific mapping decisions.
type Options struct {
	// GovernanceRoles maps client SAP roles to the platform's governance roles.
	GovernanceRoles map[string]string
	// TcodePermissions maps SAP transactions to platform permissions.
	TcodePermissions map[string][]string
	Mapping          migration.Mapping
	AutoPostLimit    string
}

// DefaultOptions suit a standard S/4HANA procure-to-pay role design.
func DefaultOptions() Options {
	return Options{
		GovernanceRoles: map[string]string{
			"Z_PROCESS_OWNER":      "process_owner",
			"Z_CONTROLLER":         "controller",
			"Z_SOLUTION_ARCHITECT": "solution_architect",
			"Z_MDG_APPROVER":       "vendor_master_approver",
		},
		TcodePermissions: map[string][]string{
			"MIRO":    {governance.ActInvoicePost, governance.ActInvoiceReview},
			"MIR7":    {governance.ActInvoiceCapture},
			"ME51N":   {governance.ActRequisition},
			"ME54N":   {governance.ActRequisitionApp},
			"ME21N":   {governance.ActPOCreate},
			"ME31K":   {governance.ActContractManage},
			"MIGO":    {governance.ActGoodsReceipt},
			"XK01":    {governance.ActVendorCreate},
			"XK02":    {governance.ActVendorChange, governance.ActVendorBank},
			"F110":    {governance.ActPaymentPropose},
			"BNK_APP": {governance.ActPaymentRun},
			"FBL1N":   {governance.ActAnalyticsQuery},
			"FBL3N":   {governance.ActAnalyticsQuery},
			"STMS":    {governance.ActConfigActivate},
		},
		Mapping:       migration.DefaultMapping(),
		AutoPostLimit: "50000.00",
	}
}

// governancePermissions are granted to the governance roles regardless of
// their SAP transactions.
var governancePermissions = map[string][]string{
	"process_owner":          {governance.ActConfigApprove, governance.ActRequisitionApp},
	"controller":             {governance.ActConfigApprove, governance.ActAnalyticsQuery},
	"solution_architect":     {governance.ActConfigApprove, governance.ActConfigActivate},
	"vendor_master_approver": {governance.ActVendorApprove},
}

// Note explains a generated setting reviewers should look at.
type Note struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (n Note) String() string { return n.Path + ": " + n.Message }

// Generate builds a draft configuration. Validate the result: SAP role
// designs often contain segregation-of-duties conflicts, and those surface
// as validation errors to be resolved before activation.
func Generate(e *sap.Extract, opts Options) (*config.Config, []Note) {
	g := &gen{e: e, opts: opts}
	c := &config.Config{Version: 1, SoDConflicts: governance.DefaultSoDConflicts(), Output: config.Output{PurchaseOrderChannel: "email"}}
	for _, t := range e.T001 {
		c.Companies = append(c.Companies, g.company(t))
	}
	for _, t := range e.T052 {
		days := 0
		fmt.Sscanf(t.ZTAG1, "%d", &days)
		c.PaymentTerms = append(c.PaymentTerms, config.PaymentTerm{Key: t.ZTERM, Description: t.TEXT1, NetDays: days})
	}
	if len(c.PaymentTerms) == 0 {
		c.PaymentTerms = []config.PaymentTerm{{Key: "NT30", Description: "Net 30 days", NetDays: 30}}
		g.note("payment_terms", "T052 not in extract; added NT30 as a placeholder")
	}
	c.Roles, c.Users = g.roles()
	c.Agents = defaultAgents()
	c.Purchasing = config.Purchasing{RequisitionApproval: g.requisitionTiers(c)}
	c.Payments = config.Payments{
		ReleaseRole: g.roleWith(c, governance.ActPaymentRun), ReleaseApprovals: 1, BankChangeHoldDays: 5,
		VendorApproverRole: g.roleWith(c, governance.ActVendorApprove),
	}
	g.note("payments.bank_change_hold_days", "no SAP equivalent; payments to a vendor are held for 5 days after a bank change")
	for _, gr := range []string{"process_owner", "controller", "solution_architect"} {
		if !slices.ContainsFunc(c.Users, func(u config.User) bool { return slices.Contains(u.Roles, gr) }) {
			g.note("users", fmt.Sprintf("no user holds governance role %s; configuration changes need one", gr))
		}
	}
	return c, g.notes
}

type gen struct {
	e     *sap.Extract
	opts  Options
	notes []Note
}

func (g *gen) note(path, msg string) { g.notes = append(g.notes, Note{path, msg}) }

func (g *gen) company(t sap.T001) config.Company {
	p := fmt.Sprintf("companies[%s]", t.BUKRS)
	co := config.Company{
		Code: t.BUKRS, Name: t.BUTXT, Currency: t.WAERS, Country: t.LAND1, FiscalYearStartMonth: 1,
		AutoPostLimit: g.opts.AutoPostLimit, OverDeliveryPercent: 0,
	}
	g.note(p+".fiscal_year_start_month", "fiscal year variant not in extract; assumed calendar year (K4)")
	g.note(p+".auto_post_limit", "no SAP equivalent; invoices above "+g.opts.AutoPostLimit+" always go to a person")
	for _, a := range g.e.SKB1 {
		if a.BUKRS != t.BUKRS {
			continue
		}
		num := strings.TrimLeft(a.SAKNR, "0")
		typ, err := g.opts.Mapping.AccountType(num)
		if err != nil {
			g.note(p+".chart_of_accounts["+num+"]", err.Error())
			continue
		}
		co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: num, Name: a.TXT50, Type: typ, VendorReconciliation: a.MITKZ == "K"})
	}
	alpha := func(s string) string { return strings.TrimLeft(s, "0") }
	for _, r := range g.e.T030 {
		if r.BUKRS != t.BUKRS {
			continue
		}
		switch r.KTOSL {
		case "VST":
			co.AccountDetermination.InputTax = alpha(r.KONTS)
		case "BNK":
			co.AccountDetermination.Bank = alpha(r.KONTS)
		}
	}
	// AP reconciliation: the account most vendors in this company post to.
	counts := map[string]int{}
	for _, v := range g.e.LFB1 {
		if v.BUKRS == t.BUKRS {
			counts[alpha(v.AKONT)]++
		}
	}
	best := ""
	for acct, n := range counts {
		if n > counts[best] || (n == counts[best] && acct < best) {
			best = acct
		}
	}
	co.AccountDetermination.APReconciliation = best
	if len(counts) > 1 {
		g.note(p+".account_determination.ap_reconciliation", fmt.Sprintf("vendors use %d reconciliation accounts; %s is the most common", len(counts), best))
	}
	for _, tx := range g.e.T007A {
		if tx.BUKRS == t.BUKRS {
			co.TaxCodes = append(co.TaxCodes, config.TaxCode{Code: tx.MWSKZ, Description: tx.TEXT1, RatePercent: tx.KBETR, Account: alpha(tx.KONTS)})
		}
	}
	co.InvoiceTolerance = config.Tolerance{PricePercent: 2, PriceAbsolute: "5.00"}
	found := false
	for _, tol := range g.e.T169G {
		if tol.BUKRS == t.BUKRS && tol.TOLSL == "PP" {
			pct := 0
			fmt.Sscanf(tol.PROZ, "%d", &pct)
			co.InvoiceTolerance = config.Tolerance{PricePercent: pct, PriceAbsolute: tol.WERTO}
			found = true
		}
	}
	if !found {
		g.note(p+".invoice_tolerance", "T169G key PP not found; used 2% / 5.00")
	}
	for _, b := range g.e.T012K {
		if b.BUKRS == t.BUKRS {
			co.HouseBank = config.HouseBank{Name: b.HBKID, IBAN: b.IBAN, BIC: b.SWIFT}
			if co.AccountDetermination.Bank == "" {
				co.AccountDetermination.Bank = alpha(b.HKONT)
			}
			break
		}
	}
	return co
}

func (g *gen) roles() ([]config.Role, []config.User) {
	var roles []config.Role
	nameFor := func(agr string) string {
		if n, ok := g.opts.GovernanceRoles[agr]; ok {
			return n
		}
		return agr
	}
	unmapped := map[string]bool{}
	for _, d := range g.e.AGR_DEFINE {
		r := config.Role{Name: nameFor(d.AGR_NAME), Description: d.TEXT}
		for _, t := range g.e.AGR_TCODES {
			if t.AGR_NAME != d.AGR_NAME {
				continue
			}
			perms, ok := g.opts.TcodePermissions[t.TCODE]
			if !ok {
				unmapped[t.TCODE] = true
			}
			for _, perm := range perms {
				if !slices.Contains(r.Permissions, perm) {
					r.Permissions = append(r.Permissions, perm)
				}
			}
		}
		for _, perm := range governancePermissions[r.Name] {
			if !slices.Contains(r.Permissions, perm) {
				r.Permissions = append(r.Permissions, perm)
			}
		}
		slices.Sort(r.Permissions)
		roles = append(roles, r)
	}
	// Roles for people who propose configuration changes: kept separate from
	// approval roles to satisfy segregation of duties.
	roles = append(roles, config.Role{Name: "config_analyst", Description: "Proposes configuration changes", Permissions: []string{governance.ActConfigPropose}})
	for _, t := range sortedKeys(unmapped) {
		g.note("roles", fmt.Sprintf("transaction %s has no platform equivalent and was not mapped", t))
	}
	byUser := map[string][]string{}
	for _, u := range g.e.AGR_USERS {
		id := strings.ToLower(u.UNAME)
		role := nameFor(u.AGR_NAME)
		if !slices.Contains(byUser[id], role) {
			byUser[id] = append(byUser[id], role)
		}
	}
	var users []config.User
	for _, id := range sortedKeys(byUser) {
		users = append(users, config.User{ID: id, Name: strings.ToUpper(id), Roles: byUser[id]})
	}
	return roles, users
}

func (g *gen) roleWith(c *config.Config, perm string) string {
	for _, r := range c.Roles {
		if slices.Contains(r.Permissions, perm) {
			return r.Name
		}
	}
	g.note("payments", "no role holds "+perm)
	return ""
}

func (g *gen) requisitionTiers(c *config.Config) []config.ApprovalTier {
	approver := ""
	for _, r := range c.Roles {
		if slices.Contains(r.Permissions, governance.ActRequisitionApp) && r.Name != "process_owner" {
			approver = r.Name
			break
		}
	}
	if approver == "" {
		approver = "process_owner"
	}
	g.note("purchasing.requisition_approval", "SAP release strategies are not in the extract; generated two tiers (up to 10,000: "+approver+"; above: "+approver+" then process_owner)")
	return []config.ApprovalTier{
		{UpTo: "10000.00", Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}, {Role: "process_owner", Approvals: 1}}},
	}
}

func defaultAgents() []config.Agent {
	return []config.Agent{
		{ID: "agent:ap-exceptions", Description: "Investigates invoices that fail matching", Permissions: []string{governance.ActInvoiceHold, governance.ActInvoiceReview}, Autonomy: 0},
		{ID: "agent:invoice-capture", Description: "Reads supplier invoices from PDF and email", Permissions: []string{governance.ActInvoiceCapture}, Autonomy: 0},
		{ID: "agent:configuration", Description: "Proposes configuration changes", Permissions: []string{governance.ActConfigPropose}, Autonomy: 0},
		{ID: "agent:migration", Description: "Runs migration rehearsals and explains differences", Permissions: []string{governance.ActMigrationRun}, Autonomy: 0},
		{ID: "agent:test", Description: "Builds and runs regression suites", Permissions: []string{}, Autonomy: 0},
		{ID: "agent:analytics", Description: "Answers questions with on-demand dashboards", Permissions: []string{governance.ActAnalyticsQuery}, Autonomy: 0},
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
