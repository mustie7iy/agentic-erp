// Package sapconfig generates a draft configuration document (03-data-model
// §3.5) from a client's SAP customizing tables (mapped as in §3.17), so
// years of setup are migrated instead of re-keyed. The draft is a proposal
// for a change request: anything without an SAP equivalent gets a documented
// default and a note for the reviewers. Roles and users are not part of the
// document (the identity context owns them); the notes list the SAP roles to
// provision there.
package sapconfig

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/cabank"
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
	// ManualEntryApprovalLimit is each company's limit for manual journal
	// entries posted without approval (T35).
	ManualEntryApprovalLimit string
	// TimeZones are the companies' IANA time zones by company code, which
	// the configuration requires (product owner 2026-10-31). They are an
	// input, not read from SAP: the extract's company code (T001) carries
	// no time zone. A company without one is drafted without it, with a
	// note, and the draft does not validate until a reviewer sets it.
	TimeZones map[string]string
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
		AutoPostLimit: "50000.00", ManualEntryApprovalLimit: "10000.00",
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
type Note = config.Note

// documentTypes get number ranges in every company and fiscal year of the
// extract, besides one journal range per SAP document type it holds.
var documentTypes = map[string]string{"journal_sa": "", "journal_ab": "", "journal_kr": "", "journal_we": "", "journal_kg": "", "journal_zp": "", "journal_ob": "",
	"requisition": "PR", "purchase_order": "PO", "contract": "CT", "goods_receipt": "GR", "supplier_invoice": "SI", "payment_run": "PY",
	"partner_change": "", "down_payment_request": "DP"}

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
	tiers := []config.ApprovalTier{
		{UpTo: "10000.00", Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}, {Role: "process_owner", Approvals: 1}}},
	}
	g.note("purchasing.order_release_approval", "SAP release strategies for purchase orders are not in the extract; generated the requisition tiers")
	g.note("purchasing.contract_expiry_notice_days", "no SAP equivalent in the extract; contracts are alerted 30 days before they end")
	c.Purchasing = config.Purchasing{RequisitionApproval: tiers, OrderReleaseApproval: slices.Clone(tiers), ContractExpiryNoticeDays: 30}
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

func (g *gen) note(path, msg string) { g.notes = append(g.notes, Note{Path: path, Message: msg}) }

// unknownDays marks a payment term whose SAP days could not be read. Any
// guess would be wrong in a way that pays early or late (0 days would
// propose every invoice under the term on its document date), so the
// draft carries a value config.Validate refuses ("must be 0-365" on
// payment_terms[KEY].net_days) until a reviewer sets the real one.
const unknownDays = -1

// paymentTerms is SAP's payment terms (T052), or a net-30 placeholder with
// a note when the extract has none.
func (g *gen) paymentTerms() []config.PaymentTerm {
	var out []config.PaymentTerm
	for _, t := range g.e.T052 {
		out = append(out, g.paymentTerm(t))
	}
	if len(out) == 0 {
		out = []config.PaymentTerm{{Key: "NT30", Description: "Net 30 days", NetDays: 30}}
		g.note("payment_terms", "T052 not in extract; added NT30 as a placeholder")
	}
	return out
}

// paymentTerm reads a T052 row: its positions ZTAG1/ZPRZ1, ZTAG2/ZPRZ2 and
// ZTAG3 are cash discount tiers while they carry a percent, and the first
// without one holds the net days (a net-only term has them in ZTAG1).
// After a tier, a position of no days (empty, or zero-filled as NUMC
// extracts write it) is unused: the last discount runs to the net due
// date (SAP's "within 14 days 2%"), which the configuration cannot hold
// (netUntilDiscount). Days are whole numbers; a term whose numbers do not
// parse, or that counts in months or fixed days (ZMONA, ZFAEL), gets
// unknownDays and a note, which fails validation until a reviewer sets
// the term.
func (g *gen) paymentTerm(t sap.T052) config.PaymentTerm {
	p := "payment_terms[" + t.ZTERM + "]"
	term := config.PaymentTerm{Key: t.ZTERM, Description: t.TEXT1, NetDays: unknownDays}
	if _, err := strconv.Atoi(strings.TrimSpace(t.ZTAG1)); err != nil {
		g.note(p+".net_days", fmt.Sprintf("T052 days %q are not a whole number; set the term's days (left at %d, which fails validation)", t.ZTAG1, unknownDays))
		return term
	}
	if calendar := strings.Trim(t.ZMONA1+t.ZFAEL1+t.ZMONA2+t.ZFAEL2+t.ZMONA3+t.ZFAEL3, "0 "); calendar != "" {
		g.note(p, fmt.Sprintf("T052 counts in months or fixed days (ZMONA/ZFAEL), which the configuration's days cannot hold; set the term (net days left at %d, which fails validation)",
			unknownDays))
		return term
	}
positions:
	for i, pos := range [][2]string{{t.ZTAG1, t.ZPRZ1}, {t.ZTAG2, t.ZPRZ2}, {t.ZTAG3, ""}} {
		days, err := strconv.Atoi(cmp.Or(strings.TrimSpace(pos[0]), "0"))
		pct, perr := percent(pos[1])
		switch {
		case err != nil || perr != nil:
			field := ".discounts"
			if i == 2 {
				field = ".net_days"
			}
			g.note(p+field, fmt.Sprintf("T052 ZTAG%d/ZPRZ%d %q/%q are not whole days and a percent; set the term (net days left at %d, which fails validation)",
				i+1, i+1, pos[0], pos[1], unknownDays))
			return term
		case i > 0 && days == 0:
			return g.netUntilDiscount(p, term)
		case pct == "": // ZTAG3 carries none, so the loop always ends here
			term.NetDays = days
			break positions
		}
		term.Discounts = append(term.Discounts, config.Discount{Days: days, Percent: pct})
	}
	return term
}

// netUntilDiscount is a term whose last discount runs to its net due
// date (SAP's "within 14 days 2%"): its net days are the tier's, which
// fails validation, with a note, until a reviewer sets them.
func (g *gen) netUntilDiscount(p string, term config.PaymentTerm) config.PaymentTerm {
	last := term.Discounts[len(term.Discounts)-1]
	term.NetDays = last.Days
	g.note(p+".net_days", fmt.Sprintf("T052 grants %s%% until the net due date (day %d); the net days must come after the last discount: set them",
		last.Percent, last.Days))
	return term
}

// percent is a T052 percent as the configuration holds it, with a
// decimal point, or "" when it is none (empty or zero); like wholePercent
// it reads the decimal comma of locale-formatted file extracts.
func percent(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1) // a lone comma is the decimal separator
	}
	if s == "" {
		return "", nil
	}
	m, err := money.ParseDecimal(s, 3)
	if err != nil || m == 0 {
		return "", err
	}
	return s, nil
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
		AutoPostLimit: g.opts.AutoPostLimit, ManualEntryApprovalLimit: g.opts.ManualEntryApprovalLimit, OverDeliveryPercent: 0,
	}
	g.note(p+".fiscal_year_start_month", "fiscal year variant not in extract; assumed calendar year (K4)")
	g.timeZone(p, &co)
	g.note(p+".auto_post_limit", "no SAP equivalent; invoices above "+g.opts.AutoPostLimit+" always go to a person")
	g.note(p+".manual_entry_approval_limit", "SAP releases parked documents by amount in its FI workflow, not in the extract; manual entries above "+
		g.opts.ManualEntryApprovalLimit+" need the controller's approval")
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
		case "WRX":
			co.AccountDetermination.GRIR = g.oneAccount(p+".account_determination.gr_ir", r, co.AccountDetermination.GRIR)
		case "PRD":
			co.AccountDetermination.PriceDifference = g.oneAccount(p+".account_determination.price_difference", r, co.AccountDetermination.PriceDifference)
		case "SKE":
			co.AccountDetermination.CashDiscount = g.oneAccount(p+".account_determination.cash_discount", r, co.AccountDetermination.CashDiscount)
		}
	}
	co.AccountDetermination.APReconciliation = g.apReconciliation(p, t.BUKRS)
	co.AccountDetermination.DownPayment = g.downPayment(p+".account_determination.down_payment", t.BUKRS)
	for _, tx := range g.e.T007A {
		if tx.BUKRS == t.BUKRS {
			co.TaxCodes = append(co.TaxCodes, config.TaxCode{Code: tx.MWSKZ, Description: tx.TEXT1, RatePercent: tx.KBETR, Account: sap.Alpha(tx.KONTS)})
		}
	}
	co.InvoiceTolerance = g.invoiceTolerance(p, t.BUKRS)
	co.HouseBank, co.HouseBanks = g.houseBanks(p, t.BUKRS, &co.AccountDetermination)
	return co
}

// timeZone sets the company's zone from the options, or notes that the
// draft lacks it: the extract has none to take, and a guess from the
// country would be wrong for a country of several zones (Canada).
func (g *gen) timeZone(p string, co *config.Company) {
	co.TimeZone = g.opts.TimeZones[co.Code]
	if co.TimeZone == "" {
		g.note(p+".time_zone", "not in the SAP extract and not given (erp config generate -time-zone "+co.Code+
			"=ZONE); set the IANA zone the company's business dates are in: the draft does not validate without it")
		return
	}
	g.note(p+".time_zone", co.TimeZone+", as given: the SAP extract has no time zone for the company code")
}

// houseBanks is the company's main house bank, its first T012K row, and
// the further ones, each on its own G/L account (ADR-0038). The main bank
// gives the account determination its bank account where T030 has none,
// and keeps its own G/L account where T030 names another. SAP keeps
// several accounts under one house bank (HKTID, not in the extract); only
// the first is taken, with a note.
func (g *gen) houseBanks(p, company string, ad *config.AccountDetermination) (config.HouseBank, []config.HouseBank) {
	var main config.HouseBank
	var further []config.HouseBank
	seen := map[string]bool{}
	for _, b := range g.e.T012K {
		if b.BUKRS != company {
			continue
		}
		hb := config.HouseBank{Name: b.HBKID, IBAN: b.IBAN, BIC: b.SWIFT, GLAccount: sap.Alpha(b.HKONT)}
		g.canadianAccount(p+".house_banks["+b.HBKID+"]", b, &hb)
		switch {
		case seen[b.HBKID]:
			g.note(p+".house_banks["+b.HBKID+"]", "house bank "+b.HBKID+" has several accounts in T012K; only the first is taken")
			continue
		case len(seen) > 0:
			further = append(further, hb)
		case ad.Bank == "":
			ad.Bank, hb.GLAccount = hb.GLAccount, ""
			main = hb
		default:
			if hb.GLAccount == ad.Bank {
				hb.GLAccount = ""
			}
			main = hb
		}
		seen[b.HBKID] = true
	}
	return main, further
}

// canadianAccount gives a Canadian house bank, which has no IBAN, its
// account by institution, transit and account number (ADR-0038, T72d):
// the routing from the bank key in the Canadian Payments Association's
// electronic form, 0 + institution + transit (cabank.MemberID). A bank key
// in another form is left out with a note, and validation then refuses
// the bank until a reviewer enters the routing.
func (g *gen) canadianAccount(path string, b sap.T012K, hb *config.HouseBank) {
	if b.IBAN != "" || b.BANKS != "CA" {
		return
	}
	hb.AccountNumber = b.BANKN
	institution, transit, ok := cabank.SplitMemberID(b.BANKL)
	if !ok {
		g.note(path, fmt.Sprintf("bank key %q is not a Canadian routing number as 0 + institution (3 digits) + transit (5 digits); enter institution_number and transit_number", b.BANKL))
		return
	}
	hb.InstitutionNumber, hb.TransitNumber = institution, transit
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

// oneAccount is the account of the first T030 row of a transaction key:
// SAP keeps WRX and PRD per valuation modifier and class, the
// configuration one per company, so rows naming another account are noted.
func (g *gen) oneAccount(path string, r sap.T030, taken string) string {
	account := sap.Alpha(r.KONTS)
	if taken == "" {
		return account
	}
	if account != taken {
		g.note(path, fmt.Sprintf("T030 %s names accounts %s and %s; %s taken", r.KTOSL, taken, account, taken))
	}
	return taken
}

// downPayment is the company's alternative reconciliation account of
// vendor down payments (T074 KOART K, UMSKZ A; ADR-0037, T72c), none when
// T074 has none. SAP keeps one per reconciliation account, the platform one
// per company: the first is taken and another noted.
func (g *gen) downPayment(path, company string) string {
	taken := ""
	for _, r := range g.e.T074 {
		if r.BUKRS != company || r.KOART != "K" || r.UMSKZ != "A" {
			continue
		}
		account := sap.Alpha(r.SKONT)
		switch {
		case taken == "":
			taken = account
		case account != taken:
			g.note(path, fmt.Sprintf("T074 names down payment accounts %s and %s; %s taken", taken, account, taken))
		}
	}
	return taken
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
			year, _ := strconv.Atoi(y) // not a number: year 0, which validation of the draft refuses
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
