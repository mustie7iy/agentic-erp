// Package sapconfig generates a draft configuration document (03-data-model
// §3.5) from a client's SAP customizing tables (mapped as in §3.17), so
// years of setup are migrated instead of re-keyed. The draft is a proposal
// for a change request: anything without an SAP equivalent gets a documented
// default and a note for the reviewers. What it maps follows the ADRs of
// the settings it fills (country packs ADR-0048, house banks ADR-0038,
// centers ADR-0050). Roles and users are not part of the
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
	// ExternalAmounts says the extract's amounts are in SAP's external
	// form (an OData source, or a table export asked for in it); without
	// it they are in SAP's internal form, as table exports write them, and
	// the draft converts them with the extract's TCURX (amounts.go).
	ExternalAmounts bool
	// Cutover is the cutover date (YYYYMMDD), at which the draft takes each
	// account's cost element category from the CSKB slice valid then (the
	// SAP specialist, po:2026-11-09, A12(d)). It is an input, as the
	// extract does not say when the scope cuts over; without it the draft
	// takes the latest slice, with a note.
	Cutover string
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
	g := newGen(e, opts)
	return g.generate(), g.notes
}

func newGen(e *sap.Extract, opts Options) *gen {
	return &gen{e: e, opts: opts, areas: areasOf(e)}
}

func (g *gen) generate() *config.Config {
	opts := g.opts
	g.decimals, g.problems = sap.CurrencyDecimals(g.e.TCURX)
	g.tcurxNotes()
	c := &config.Config{Version: 1, Output: config.Output{PurchaseOrderChannel: "email"}}
	for _, t := range g.e.T001 {
		c.Companies = append(c.Companies, g.company(t))
	}
	c.PaymentTerms = g.paymentTerms()
	approver := opts.RequisitionApprover
	g.note(none, "purchasing.requisition_approval", "SAP release strategies are not in the extract; generated two tiers (up to 10,000: "+approver+
		"; above: "+approver+" then process_owner)")
	tiers := []config.ApprovalTier{
		{UpTo: "10000.00", Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}}},
		{Steps: []config.ApprovalStep{{Role: approver, Approvals: 1}, {Role: "process_owner", Approvals: 1}}},
	}
	g.note(none, "purchasing.order_release_approval", "SAP release strategies for purchase orders are not in the extract; generated the requisition tiers")
	g.note(none, "purchasing.contract_expiry_notice_days", "no SAP equivalent in the extract; contracts are alerted 30 days before they end")
	c.Purchasing = config.Purchasing{RequisitionApproval: tiers, OrderReleaseApproval: slices.Clone(tiers), ContractExpiryNoticeDays: 30}
	c.Payments = config.Payments{ReleaseRole: opts.ReleaseRole, ReleaseApprovals: 1, BankChangeHoldDays: 5, VendorApproverRole: opts.VendorApproverRole}
	g.note(none, "payments.bank_change_hold_days", "no SAP equivalent; payments to a vendor are held for 5 days after a bank change")
	c.NumberRanges = g.numberRanges()
	g.roles(c)
	return c
}

type gen struct {
	e     *sap.Extract
	opts  Options
	notes []Note
	areas map[string][]string // controlling areas by company code (areasOf)
	// decimals are TCURX's decimals of the currencies it lists, and
	// problems its rows the conversion cannot use (sap.CurrencyDecimals).
	decimals map[string]int
	problems []sap.TCURXProblem
	// objects are the customizing objects read, and defaults the notes on
	// settings no SAP object gives, for the fit-gap report (fitgap.go).
	objects  map[objRef]*object
	defaults []Note
}

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
		g.note(none, "payment_terms", "T052 not in extract; added NT30 as a placeholder")
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
	o := g.named(g.read("T052", t.ZTERM), t.TEXT1)
	if _, err := strconv.Atoi(strings.TrimSpace(t.ZTAG1)); err != nil {
		g.note(o, p+".net_days", fmt.Sprintf("T052 days %q are not a whole number; set the term's days (left at %d, which fails validation)", t.ZTAG1, unknownDays))
		return term
	}
	if calendar := strings.Trim(t.ZMONA1+t.ZFAEL1+t.ZMONA2+t.ZFAEL2+t.ZMONA3+t.ZFAEL3, "0 "); calendar != "" {
		g.note(o, p, fmt.Sprintf("T052 counts in months or fixed days (ZMONA/ZFAEL), which the configuration's days cannot hold; set the term (net days left at %d, which fails validation)",
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
			g.note(o, p+field, fmt.Sprintf("T052 ZTAG%d/ZPRZ%d %q/%q are not whole days and a percent; set the term (net days left at %d, which fails validation)",
				i+1, i+1, pos[0], pos[1], unknownDays))
			return term
		case i > 0 && days == 0:
			return g.netUntilDiscount(o, p, term)
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
func (g *gen) netUntilDiscount(o objRef, p string, term config.PaymentTerm) config.PaymentTerm {
	last := term.Discounts[len(term.Discounts)-1]
	term.NetDays = last.Days
	g.note(o, p+".net_days", fmt.Sprintf("T052 grants %s%% until the net due date (day %d); the net days must come after the last discount: set them",
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
	o := g.named(g.read("T001", t.BUKRS), t.BUTXT)
	g.note(o, p+".fiscal_year_start_month", "fiscal year variant not in extract; assumed calendar year (K4)")
	g.timeZone(o, p, &co)
	g.note(o, p+".auto_post_limit", "no SAP equivalent; invoices above "+g.opts.AutoPostLimit+" always go to a person")
	g.note(o, p+".manual_entry_approval_limit", "SAP releases parked documents by amount in its FI workflow, not in the extract; manual entries above "+
		g.opts.ManualEntryApprovalLimit+" need the controller's approval")
	co.ChartOfAccounts = g.chartOfAccounts(p, t.BUKRS)
	g.costElements(o, p, t.BUKRS, co.ChartOfAccounts)
	g.accountDetermination(p, &co)
	co.AccountDetermination.APReconciliation = g.apReconciliation(o, p, t.BUKRS)
	co.AccountDetermination.DownPayment = g.downPayment(p+".account_determination.down_payment", t.BUKRS)
	for _, tx := range g.e.T007A {
		if tx.BUKRS == t.BUKRS {
			g.named(g.read("T007A", tx.BUKRS+"/"+tx.MWSKZ), tx.TEXT1)
			co.TaxCodes = append(co.TaxCodes, config.TaxCode{Code: tx.MWSKZ, Description: tx.TEXT1, RatePercent: tx.KBETR, Account: sap.Alpha(tx.KONTS)})
		}
	}
	co.InvoiceTolerance = g.invoiceTolerance(o, p, t.BUKRS, t.WAERS)
	co.HouseBank, co.HouseBanks = g.houseBanks(p, &co)
	co.PaymentMethods = g.paymentMethods(o, p, t.LAND1, &co)
	g.defaultAssignments(p, &co)
	return co
}

// accountDetermination takes the company's T030 rows: of VST and BNK the
// last row's account (an earlier row naming another is left out), of WRX,
// PRD, SKE, SKT and BO1 the first's (oneAccount). SKT, the cash discount
// granted to customers (an expense), is cash_discount_granted, and BO1,
// the revenue of a supplier's subsequent settlement (its rebates), is
// supplier_rebate: both key meanings are from memory, for the specialist
// (ADR-0048 As built batch 93). A row of any other transaction key is read
// and left out: the configuration's account determination holds none of
// its kind.
func (g *gen) accountDetermination(p string, co *config.Company) {
	ad := &co.AccountDetermination
	type row struct {
		o       objRef
		account string
	}
	taken := map[string][]row{} // VST and BNK: their rows in order
	for _, r := range g.e.T030 {
		if r.BUKRS != co.Code {
			continue
		}
		account := sap.Alpha(r.KONTS)
		o := g.read("T030", r.BUKRS+"/"+r.KTOSL+"/"+account)
		switch r.KTOSL {
		case "VST":
			ad.InputTax, taken[r.KTOSL] = account, append(taken[r.KTOSL], row{o, account})
		case "BNK":
			ad.Bank, taken[r.KTOSL] = account, append(taken[r.KTOSL], row{o, account})
		case "WRX":
			ad.GRIR = g.oneAccount(o, p+".account_determination.gr_ir", r, ad.GRIR)
		case "PRD":
			ad.PriceDifference = g.oneAccount(o, p+".account_determination.price_difference", r, ad.PriceDifference)
		case "SKE":
			ad.CashDiscount = g.oneAccount(o, p+".account_determination.cash_discount", r, ad.CashDiscount)
		case "SKT":
			ad.CashDiscountGranted = g.oneAccount(o, p+".account_determination.cash_discount_granted", r, ad.CashDiscountGranted)
		case "BO1":
			ad.SupplierRebate = g.oneAccount(o, p+".account_determination.supplier_rebate", r, ad.SupplierRebate)
		default:
			g.gap(o, p+".account_determination", "T030 transaction key "+r.KTOSL+" is none the configuration's account determination holds "+
				"(it takes VST, BNK, WRX, PRD, SKE, SKT and BO1); left out")
		}
	}
	for _, key := range sortedKeys(taken) {
		rows := taken[key]
		last := rows[len(rows)-1].account
		for _, r := range rows {
			if r.account != last {
				g.gap(r.o, p+".account_determination", "a later T030 "+key+" row names account "+last+", which is taken; left out")
			}
		}
	}
}

// timeZone sets the company's zone from the options, or notes that the
// draft lacks it: the extract has none to take, and a guess from the
// country would be wrong for a country of several zones (Canada).
func (g *gen) timeZone(o objRef, p string, co *config.Company) {
	co.TimeZone = g.opts.TimeZones[co.Code]
	if co.TimeZone == "" {
		g.note(o, p+".time_zone", "not in the SAP extract and not given (erp config generate -time-zone "+co.Code+
			"=ZONE); set the IANA zone the company's business dates are in: the draft does not validate without it")
		return
	}
	g.note(o, p+".time_zone", co.TimeZone+", as given: the SAP extract has no time zone for the company code")
}

// houseBanks is the company's main house bank, its first T012K row, and
// the further ones, each on its own G/L account (ADR-0038). The main bank
// gives the account determination its bank account where T030 has none,
// and keeps its own G/L account where T030 names another. SAP keeps
// several accounts under one house bank (HKTID, not in the extract); only
// the first is taken, with a note. Each bank is in its account's currency
// (houseBankCurrency).
func (g *gen) houseBanks(p string, co *config.Company) (config.HouseBank, []config.HouseBank) {
	var main config.HouseBank
	var further []config.HouseBank
	ad := &co.AccountDetermination
	rows := map[string]int{} // T012K rows read per house bank
	for _, b := range g.e.T012K {
		if b.BUKRS != co.Code {
			continue
		}
		rows[b.HBKID]++
		o := g.read("T012K", houseBankKey(b, rows[b.HBKID]))
		hb := config.HouseBank{Name: b.HBKID, IBAN: b.IBAN, BIC: b.SWIFT, GLAccount: sap.Alpha(b.HKONT)}
		g.canadianAccount(o, p+".house_banks["+b.HBKID+"]", b, &hb)
		if rows[b.HBKID] == 1 { // a dropped account's currency is not the bank's
			g.houseBankCurrency(o, p+".house_banks["+b.HBKID+"]", b, co.Currency, &hb)
		}
		switch {
		case rows[b.HBKID] > 1:
			g.leaveOut(o, p+".house_banks["+b.HBKID+"]", "house bank "+b.HBKID+" has several accounts in T012K; only the first is taken")
			continue
		case len(rows) > 1:
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
	}
	return main, further
}

// houseBankKey is a T012K row's key in the fit-gap report: company code
// and house bank, and for a further account of the bank its place among
// the bank's rows, as the extract gives no account ID (HKTID).
func houseBankKey(b sap.T012K, n int) string {
	key := b.BUKRS + "/" + b.HBKID
	if n > 1 {
		key += fmt.Sprintf(" (account %d)", n)
	}
	return key
}

// unknownCurrency marks a house bank whose T012K row has no currency
// (WAERS). The company's currency would be a guess that, for a
// foreign-currency account, could never be corrected: a currency never
// changes under a bank's code (ADR-0038, T53a). So the draft carries a
// value config.Validate refuses ("not in the currency reference data" on
// the bank's currency) until a reviewer sets the account's currency.
const unknownCurrency = "UNKNOWN"

// houseBankCurrency gives the house bank its account's currency, T012K
// WAERS (ADR-0038, T53a): none for the company's own, which a bank without
// one keeps; another as it is, which validation refuses unless the
// reference data knows it and the bank's ledger accounts are revalued at
// period end (ADR-0042), which the extract cannot tell, so it is noted for
// the reviewers. An account without WAERS is not restricted by SAP, whose
// binding currency is then its G/L account's (SKB1 WAERS of HKONT; the SAP
// specialist, po:2026-11-09, A5 [M]), taken with a note; unknownCurrency,
// noted, when neither gives one, even for a company without a currency of
// its own, which must not make an account without one look like the
// company's.
func (g *gen) houseBankCurrency(o objRef, path string, b sap.T012K, companyCurrency string, hb *config.HouseBank) {
	waers, from := strings.TrimSpace(b.WAERS), "T012K WAERS"
	if waers == "" {
		if waers = g.ledgerCurrency(b.BUKRS, b.HKONT); waers == "" {
			hb.Currency = unknownCurrency
			g.note(o, path+".currency", "T012K gives the account no currency (WAERS), nor does its G/L account "+sap.Alpha(b.HKONT)+
				" (SKB1 WAERS); set it (left at "+unknownCurrency+", which fails validation): a bank's currency never changes once it is created")
			return
		}
		from = "SKB1 WAERS of its G/L account " + sap.Alpha(b.HKONT)
		g.note(o, path+".currency", fmt.Sprintf("T012K gives the account no currency (WAERS), so SAP does not restrict it and its G/L account's "+
			"binds it: %s, from %s (SAP specialist, to verify on the pilot system; FI-BAM's account master FCLM_BAM_AMD may cross-check it)", waers, from))
	}
	if waers == strings.TrimSpace(companyCurrency) {
		return
	}
	hb.Currency = waers
	g.note(o, path+".currency", fmt.Sprintf("the account is in %s (%s), not the company's %s: flag the bank's ledger accounts "+
		"revalue_foreign_currency and give each its unrealised gain and loss accounts in fx_valuation.accounts (SAP's valuation "+
		"accounts are not in the extract); the draft does not validate until then", waers, from, companyCurrency))
}

// ledgerCurrency is the currency of G/L account account of company code
// company (SKB1 WAERS), "" when the extract gives none.
func (g *gen) ledgerCurrency(company, account string) string {
	for _, a := range g.e.SKB1 {
		if a.BUKRS == company && sap.Alpha(a.SAKNR) == sap.Alpha(account) {
			return strings.TrimSpace(a.WAERS)
		}
	}
	return ""
}

// canadianAccount gives a Canadian house bank, which has no IBAN, its
// account by institution, transit and account number (ADR-0038, T72d):
// the routing from the bank key in the Canadian Payments Association's
// electronic form, 0 + institution + transit (cabank.MemberID), or in the
// paper form transit-institution (cabank.SplitBankKey; po:2026-11-09, A6).
// A bank key in another form is left out with a note, and validation then
// refuses the bank until a reviewer enters the routing.
func (g *gen) canadianAccount(o objRef, path string, b sap.T012K, hb *config.HouseBank) {
	if b.IBAN != "" || b.BANKS != "CA" {
		return
	}
	hb.AccountNumber = b.BANKN
	institution, transit, ok := cabank.SplitBankKey(b.BANKL)
	if !ok {
		g.note(o, path, fmt.Sprintf("bank key %q is not a Canadian routing number as 0 + institution (3 digits) + transit (5 digits), or transit-institution "+
			"(12345-001); enter institution_number and transit_number", b.BANKL))
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
		o := g.named(g.read("SKB1", company+"/"+num), a.TXT50)
		typ, err := g.opts.accountType(num)
		if err != nil {
			g.leaveOut(o, p+".chart_of_accounts["+num+"]", err.Error())
			continue
		}
		out = append(out, config.Account{Number: num, Name: a.TXT50, Type: typ, VendorReconciliation: a.MITKZ == "K"})
	}
	return out
}

// oneAccount is the account of the first T030 row of a transaction key:
// SAP keeps WRX and PRD per valuation modifier and class, the
// configuration one per company, so rows naming another account are noted.
func (g *gen) oneAccount(o objRef, path string, r sap.T030, taken string) string {
	account := sap.Alpha(r.KONTS)
	if taken == "" {
		return account
	}
	if account != taken {
		g.leaveOut(o, path, fmt.Sprintf("T030 %s names accounts %s and %s; %s taken", r.KTOSL, taken, account, taken))
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
		if r.BUKRS != company {
			continue
		}
		o := g.read("T074", r.BUKRS+"/"+r.KOART+"/"+r.UMSKZ+"/"+sap.Alpha(r.HKONT))
		if r.KOART != "K" || r.UMSKZ != "A" {
			g.gap(o, path, "T074 account type "+r.KOART+", special G/L indicator "+r.UMSKZ+": only vendor down payments (K, A) are taken; left out")
			continue
		}
		account := sap.Alpha(r.SKONT)
		switch {
		case taken == "":
			taken = account
		case account != taken:
			g.leaveOut(o, path, fmt.Sprintf("T074 names down payment accounts %s and %s; %s taken", taken, account, taken))
		}
	}
	return taken
}

// apReconciliation is the account most vendors in this company post to.
func (g *gen) apReconciliation(o objRef, p, company string) string {
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
		g.note(o, p+".account_determination.ap_reconciliation", fmt.Sprintf("vendors use %d reconciliation accounts; %s is the most common", len(counts), best))
	}
	return best
}

// invoiceTolerance is the company's price tolerance (T169G key PP), or 2%
// and 5.00 with a note when SAP has none. A percentage that does not parse
// keeps the 2% default and one with a fraction is cut to whole percent,
// and an amount that cannot be converted from SAP's internal form keeps
// the 5.00 default, each with a note.
func (g *gen) invoiceTolerance(co objRef, p, company, cur string) config.Tolerance {
	tolerance := config.Tolerance{PricePercent: 2, PriceAbsolute: "5.00"}
	found := false
	for _, tol := range g.e.T169G {
		if tol.BUKRS != company {
			continue
		}
		o := g.read("T169G", tol.BUKRS+"/"+tol.TOLSL)
		if tol.TOLSL != "PP" {
			g.gap(o, p+".invoice_tolerance", "T169G tolerance key "+tol.TOLSL+": the configuration takes the price tolerance, key PP, alone; left out")
			continue
		}
		tolerance, found = config.Tolerance{PricePercent: 2, PriceAbsolute: "5.00"}, true
		if werto, problem := g.amount(tol.WERTO, cur); problem != "" {
			g.note(o, p+".invoice_tolerance.price_absolute", fmt.Sprintf("T169G key PP amount %q %s; used 5.00", tol.WERTO, problem))
		} else {
			tolerance.PriceAbsolute = werto
		}
		pct, exact, problem := wholePercent(tol.PROZ)
		switch {
		case problem != "":
			g.note(o, p+".invoice_tolerance.price_percent", fmt.Sprintf("T169G key PP percentage %q %s; used 2%%", tol.PROZ, problem))
			continue
		case !exact:
			g.note(o, p+".invoice_tolerance.price_percent", fmt.Sprintf("T169G key PP percentage %q is not a whole percent; used %d%%", tol.PROZ, pct))
		}
		tolerance.PricePercent = pct
	}
	if !found {
		g.note(co, p+".invoice_tolerance", "T169G key PP not found; used 2% / 5.00")
	}
	return tolerance
}

// numberRanges gives every company a range per document type in each
// fiscal year the extract's journal covers, so the draft can number
// migrated and new documents. Only journal entries are numbered per
// fiscal year; any other document's number is unique across years
// (config.RangeRepeats), so in a year before the latest its prefix
// carries the year (SI2025-), and the latest year, the one new documents
// are numbered in, keeps the plain prefix.
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
		g.note(none, "number_ranges", "the extract has no journal; add number ranges for the fiscal years to migrate")
	}
	var out []config.NumberRange
	sorted := sortedKeys(years)
	for _, t := range g.e.T001 {
		for i, y := range sorted {
			year, _ := strconv.Atoi(y) // not a number: year 0, which validation of the draft refuses
			for _, dt := range sortedKeys(types) {
				prefix := types[dt]
				if i < len(sorted)-1 && !config.NumberedPerYear(dt) {
					prefix += y + "-"
				}
				out = append(out, config.NumberRange{Company: t.BUKRS, DocumentType: dt, FiscalYear: year, Prefix: prefix, First: 100000, Last: 999999})
			}
		}
	}
	return out
}

// roles notes the roles the document routes approvals to, and the SAP
// roles with their users, to provision in the identity context.
func (g *gen) roles(c *config.Config) {
	g.note(none, "roles", "provision these identity roles before activating: "+strings.Join(c.RoleNames(), ", "))
	users := map[string][]string{}
	for _, u := range g.e.AGR_USERS {
		if !slices.Contains(users[u.AGR_NAME], u.UNAME) {
			users[u.AGR_NAME] = append(users[u.AGR_NAME], u.UNAME)
		}
	}
	for _, d := range g.e.AGR_DEFINE {
		g.leaveOut(g.named(g.read("AGR_DEFINE", d.AGR_NAME), d.TEXT), "roles["+d.AGR_NAME+"]", fmt.Sprintf("SAP role %q (users: %s): map its permissions in identity", d.TEXT, strings.Join(users[d.AGR_NAME], ", ")))
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
