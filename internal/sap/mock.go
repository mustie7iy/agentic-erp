package sap

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// GenerateMock builds a deterministic, internally consistent SAP extract for
// development without an SAP system. It covers three company codes (US,
// German, and Canadian: 3000, the pilot's), vendors,
// POs, goods receipts, posted and parked invoices, payments with clearing,
// and a few deliberate data-quality problems that real migrations hit:
//   - vendor 0000100005 has no tax number
//   - vendors 0000100007 and 0000100008 are duplicates by name
//   - vendor 0000100012 is posting-blocked but still has an open item
//
// Parked invoices in company 1000 exercise each invoice-matching outcome:
// clean, price variance, quantity over goods receipt, duplicate reference,
// above the auto-post limit, and a gross/tax mismatch. Its first three
// orders' invoices are followed by a parked credit memo, subsequent debit
// and subsequent credit (corrections, T72b).
//
// Company 3000 is generated as 1000 is, after the others, so their data is
// the same with or without it: its vendors are Canadian ones, with their
// bank accounts by institution, transit and account number, and two of
// 1000's, and its house bank is Canadian, without an IBAN (T72d), with a
// second account in USD, on its own ledger account 0000102000, beside
// exchange gain and loss accounts its revaluation can name (T012K WAERS,
// ADR-0038 T53a). Every company pays by bank transfer through its house
// banks (T042Z, T042E, T042A, T042I), and 3000's vendors list it as their
// payment method (LFB1-ZWELS). Being
// generated as 1000 is, it has the same scenarios and corrections, the
// corrections numbered on after 1000's. It also has an open down payment
// to the supplier of its second order, whose invoice SAP posted and did not
// pay (special G/L A on its down payment account 0000159000, T074, with
// input tax V1 in SAP's net display; T72c), and an order SAP valued
// through GR/IR, received and not yet invoiced: an open credit on its GR/IR
// account 0000219100 (T030 WRX) naming the order item (T72f), and an
// invoice in USD it has not paid, an open item in a foreign document
// currency (foreignInvoice). Its other orders' receipts post nothing to
// SAP's journal, as 1000's and 2000's do.
//
// Revenue follows a monthly trend that grows about 7% a year, so extracts
// for consecutive years support year-over-year analytics.
func GenerateMock(seed uint64, fiscalYear string) *Extract {
	return GenerateMockThrough(seed, fiscalYear, 12)
}

// GenerateMockThrough is GenerateMock with revenue postings only up to and
// including throughMonth, for a fiscal year that is still in progress.
func GenerateMockThrough(seed uint64, fiscalYear string, throughMonth int) *Extract {
	g := &mockGen{
		through: throughMonth,
		r:       rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), // #nosec G404 -- deterministic mock data, not a secret
		e:       &Extract{},
		gj:      fiscalYear,
		fi:      map[string]int{},
		ref:     map[string]string{},
	}
	g.masterData()
	g.company("1000", 8, g.vendorsFor("1000"), true)
	g.company("2000", 19, g.vendorsFor("2000"), false)
	g.company("3000", 5, g.vendorsFor("3000"), true)
	g.downPayment("3000", 5)
	g.receivedNotInvoiced("3000")
	g.foreignInvoice("3000")
	g.customizing()
	return g.e
}

type mockGen struct {
	r     *rand.Rand
	e     *Extract
	gj    string
	fi    map[string]int // FI document counter per company
	poSeq int
	ivSeq int
	cmSeq int // corrections, numbered apart so the invoices keep their numbers
	grSeq int
	ref   map[string]string // PO -> vendor reference of its posted invoice

	through int // last month with revenue postings
}

// mockDecimals is the digits of the mock's amounts: they are generated in
// hundredths, and its companies are in USD, EUR and CAD, two-digit currencies
// in ref.currency. SAP extracts write an amount with its currency's digits.
const mockDecimals = 2

var mockCompanies = []T001{
	{BUKRS: "1000", BUTXT: "Acme Corp US", WAERS: "USD", LAND1: "US"},
	{BUKRS: "2000", BUTXT: "Acme GmbH", WAERS: "EUR", LAND1: "DE"},
	{BUKRS: "3000", BUTXT: "Maple Industries Canada", WAERS: "CAD", LAND1: "CA"},
}

var mockAccounts = []SKB1{
	{SAKNR: "0000100000", TXT50: "Bank - Operating", XBILK: "X"},
	{SAKNR: "0000140000", TXT50: "Input Tax", XBILK: "X"},
	{SAKNR: "0000211000", TXT50: "Accounts Payable - Domestic", XBILK: "X", MITKZ: "K"},
	{SAKNR: "0000300000", TXT50: "Share Capital", XBILK: "X"},
	{SAKNR: "0000400000", TXT50: "Sales Revenue"},
	{SAKNR: "0000510000", TXT50: "Raw Materials Consumed"},
	{SAKNR: "0000520000", TXT50: "Office Supplies"},
	{SAKNR: "0000530000", TXT50: "IT Services"},
	{SAKNR: "0000540000", TXT50: "Freight Inbound"},
	{SAKNR: "0000610000", TXT50: "Salaries and Wages"},
}

var expenseAccounts = []string{"0000510000", "0000520000", "0000530000", "0000540000"}

var vendorNames = []string{
	"Initech Components", "Umbrella Logistics", "Stark Industrial", "Wayne Supply Co", "Soylent Packaging",
	"Hooli Cloud Services", "Globex Industrial Supply", "Globex Industrial Supply", "Cyberdyne Systems",
	"Wonka Raw Materials", "Tyrell Office Products", "Massive Dynamic", "Vandelay Imports",
	"Oceanic Freight", "Pied Piper IT", "Dunder Paper", "Gringotts Metals", "Aperture Labs Supply",
	"Nakatomi Trading", "Monarch Chemicals", "Prestige Worldwide", "Sirius Cybernetics", "Blue Sun Corp",
	"Weyland Materials",
}

const blockedVendor = "0000100012"

// downPaymentAccount is company 3000's alternative reconciliation account
// of down payments (special G/L A, T074): an asset on the vendors'
// reconciliation (MITKZ K).
const downPaymentAccount = "0000159000"

// grirAccount and priceDifferenceAccount are company 3000's GR/IR
// clearing account (T030 WRX), a liability, and its price difference
// account (T030 PRD), an expense.
const (
	grirAccount            = "0000219100"
	priceDifferenceAccount = "0000591000"
)

// usdBankAccount is the ledger account of company 3000's USD house bank
// account, MUSD.
const usdBankAccount = "0000102000"

func vendorID(i int) string { return fmt.Sprintf("%010d", 100001+i) }

func (g *mockGen) masterData() {
	g.e.T001 = append(g.e.T001, mockCompanies...)
	for _, c := range mockCompanies {
		for _, a := range mockAccounts {
			a.BUKRS = c.BUKRS
			g.e.SKB1 = append(g.e.SKB1, a)
		}
	}
	g.e.SKB1 = append(g.e.SKB1, SKB1{BUKRS: "3000", SAKNR: downPaymentAccount, TXT50: "Down payments made", XBILK: "X", MITKZ: "K"},
		SKB1{BUKRS: "3000", SAKNR: grirAccount, TXT50: "GR/IR clearing", XBILK: "X"}, SKB1{BUKRS: "3000", SAKNR: priceDifferenceAccount, TXT50: "Price differences"},
		SKB1{BUKRS: "3000", SAKNR: usdBankAccount, TXT50: "Bank - Operating USD", XBILK: "X"},
		SKB1{BUKRS: "3000", SAKNR: "0000480000", TXT50: "Exchange rate gains"}, SKB1{BUKRS: "3000", SAKNR: "0000680000", TXT50: "Exchange rate losses"})
	terms := []string{"NT30", "NT45", "NT14", "NT60"}
	for i, name := range vendorNames {
		id := vendorID(i)
		v := LFA1{LIFNR: id, NAME1: name, LAND1: "US", STCD1: fmt.Sprintf("%02d-%07d", 10+i, 1000000+i*7919)}
		if i >= 16 {
			v.LAND1 = "DE"
			v.STCD1 = fmt.Sprintf("DE%09d", 100000000+i*104729)
		}
		if id == "0000100005" {
			v.STCD1 = ""
		}
		if id == blockedVendor {
			v.SPERR = "X"
		}
		g.e.LFA1 = append(g.e.LFA1, v)
		g.e.LFBK = append(g.e.LFBK, LFBK{LIFNR: id, BANKS: v.LAND1, BANKN: fmt.Sprintf("%012d", 400000000000+int64(i)*7777)})
		if i < 18 {
			g.e.LFB1 = append(g.e.LFB1, LFB1{LIFNR: id, BUKRS: "1000", AKONT: "0000211000", ZTERM: terms[i%len(terms)]})
		}
		if i >= 14 {
			g.e.LFB1 = append(g.e.LFB1, LFB1{LIFNR: id, BUKRS: "2000", AKONT: "0000211000", ZTERM: terms[(i+1)%len(terms)]})
		}
	}
	g.canadianVendors(terms)
}

// canadianVendorNames are company 3000's own vendors, numbered from
// 0000300001.
var canadianVendorNames = []string{
	"Laurentian Paper", "Prairie Freight", "Rideau IT Services", "Fundy Chemicals", "Muskoka Office Supply", "Cabot Components",
}

// canadianShared are vendors of company 1000 that company 3000 buys from
// too: Tyrell Office Products, and Massive Dynamic, posting-blocked, for
// 3000's blocked-vendor scenario.
var canadianShared = []string{"0000100011", blockedVendor}

// transfer is the mock's one payment method, a bank transfer (T042Z,
// mockPaymentMethods), which company 3000's vendors are paid by
// (LFB1-ZWELS); the other companies' vendors list none, as SAP allows.
const transfer = "T"

// canadianVendors adds company 3000's vendors: Canadian ones with a
// Canadian bank account (BANKS CA; BANKL the routing as 0 + institution +
// transit, cabank.MemberID; BANKN the account number), and canadianShared,
// each paid by bank transfer (ZWELS). It draws nothing from the generator.
func (g *mockGen) canadianVendors(terms []string) {
	for i, name := range canadianVendorNames {
		id := fmt.Sprintf("%010d", 300001+i)
		g.e.LFA1 = append(g.e.LFA1, LFA1{LIFNR: id, NAME1: name, LAND1: "CA", STCD1: fmt.Sprintf("%09d", 800000000+i*7919)})
		institution, transit := []string{"001", "002", "003", "004", "006", "010"}[i], fmt.Sprintf("%05d", 10+i*111)
		g.e.LFBK = append(g.e.LFBK, LFBK{LIFNR: id, BANKS: "CA", BANKL: "0" + institution + transit, BANKN: fmt.Sprintf("%07d", 5000000+(i+1)*7777)})
		g.e.LFB1 = append(g.e.LFB1, LFB1{LIFNR: id, BUKRS: "3000", AKONT: "0000211000", ZTERM: terms[i%len(terms)], ZWELS: transfer})
	}
	for _, id := range canadianShared {
		g.e.LFB1 = append(g.e.LFB1, LFB1{LIFNR: id, BUKRS: "3000", AKONT: "0000211000", ZTERM: "NT30", ZWELS: transfer})
	}
}

func (g *mockGen) vendorsFor(cc string) []string {
	var out []string
	for _, v := range g.e.LFB1 {
		if v.BUKRS == cc && v.LIFNR != blockedVendor {
			out = append(out, v.LIFNR)
		}
	}
	return out
}

func (g *mockGen) date(month, day int) string { return fmt.Sprintf("%s%02d%02d", g.gj, month, day) }

func (g *mockGen) nextFI(cc string) string {
	g.fi[cc]++
	return fmt.Sprintf("%010d", 100000000+g.fi[cc])
}

type fiLine struct {
	acct   string
	amount money.Minor
	vendor string
	umskz  string // special G/L indicator of a vendor line
	mwskz  string // tax code
	ebeln  string // purchase order and item of a line on an order's account or GR/IR
	ebelp  string
	// document is the line's amount in a foreign document currency
	// (fiDocIn); a document in its company's currency has amount there.
	document money.Minor
}

// fiDoc appends a balanced FI document in the company's currency to
// ACDOCA and returns its number.
func (g *mockGen) fiDoc(cc, budat, blart, xblnr string, lines []fiLine) string {
	for i := range lines {
		lines[i].document = lines[i].amount
	}
	return g.fiDocIn(cc, mockCurrency(cc), budat, blart, xblnr, lines)
}

// fiDocIn appends an FI document in a document currency (RWCUR), each
// line's amount its company code amount (HSL) and document its amount in
// that currency (WSL), balanced in both, and returns its number.
func (g *mockGen) fiDocIn(cc, currency, budat, blart, xblnr string, lines []fiLine) string {
	belnr := g.nextFI(cc)
	var total, totalDocument money.Minor
	for i, l := range lines {
		total, totalDocument = total+l.amount, totalDocument+l.document
		g.e.ACDOCA = append(g.e.ACDOCA, ACDOCA{
			RLDNR: "0L", RBUKRS: cc, GJAHR: g.gj, BELNR: belnr, DOCLN: fmt.Sprintf("%06d", i+1),
			RACCT: l.acct, HSL: l.amount.Plain(mockDecimals), BUDAT: budat, BLART: blart, LIFNR: l.vendor, XBLNR: xblnr,
			UMSKZ: l.umskz, MWSKZ: l.mwskz, EBELN: l.ebeln, EBELP: l.ebelp, RWCUR: currency, WSL: l.document.Plain(mockDecimals),
		})
	}
	if total != 0 || totalDocument != 0 {
		//nolint:forbidigo // an invariant of this generator: its callers build every document's lines from the same amounts, so an unbalanced one is a bug in this file, not something a seed or year can cause
		panic(fmt.Sprintf("mock FI doc %s/%s unbalanced by %s and %s in %s", cc, belnr, total, totalDocument, currency))
	}
	return belnr
}

func (g *mockGen) clear(cc, belnr, vendor, clearingDoc string) {
	for i := range g.e.ACDOCA {
		r := &g.e.ACDOCA[i]
		if r.RBUKRS == cc && r.BELNR == belnr && r.LIFNR == vendor && r.RACCT == "0000211000" {
			r.AUGBL = clearingDoc
		}
	}
}

func (g *mockGen) money(lo, hi int64) money.Minor { return money.Minor(lo + g.r.Int64N(hi-lo+1)) }

// milli is a mock quantity in thousandths of a unit, SAP MENGE's three
// decimals, kept as an integer so a partial receipt can halve it.
type milli int64

const perUnit = 1000

// String renders the quantity as SAP extracts do, without trailing zeros:
// "12.5", "3".
func (q milli) String() string {
	if q%perUnit == 0 {
		return strconv.FormatInt(int64(q/perUnit), 10)
	}
	return strings.TrimRight(fmt.Sprintf("%d.%03d", q/perUnit, q%perUnit), "0")
}

type mockPOLine struct {
	item   string
	qty    milli
	price  money.Minor
	peinh  int64
	sakto  string
	amount money.Minor
}

func (g *mockGen) company(cc string, taxPct int64, vendors []string, scenarios bool) {
	g.fiDoc(cc, g.date(1, 2), "SA", "CAPITAL", []fiLine{
		{acct: "0000100000", amount: 500000000},
		{acct: "0000300000", amount: -500000000},
	})
	for m := 1; m <= min(6, g.through); m++ {
		g.revenue(cc, m)
	}

	const numPOs = 20
	for i := range numPOs {
		g.purchaseOrder(cc, i, taxPct, vendors, scenarios)
	}

	// Second-half revenue is generated after purchasing, so months 7 to 12
	// draw nothing before the orders: a year through June or later (-through
	// 6 and above) gets the same orders and scenarios. A year through an
	// earlier month draws less first-half revenue before them, so its
	// orders differ; extracts are reproducible per seed, year and -through,
	// and the tests rely on that, not on orders that ignore -through.
	for m := 7; m <= g.through; m++ {
		g.revenue(cc, m)
	}
}

// purchaseOrder generates a company's i-th order with its receipts and
// invoice. The order of random draws is part of the mock's contract: the
// same seed must produce the same extract.
func (g *mockGen) purchaseOrder(cc string, i int, taxPct int64, vendors []string, scenarios bool) {
	vendor := vendors[g.r.IntN(len(vendors))]
	switch {
	case scenarios && i == 3:
		vendor = blockedVendor // invoiced before the vendor was blocked
	case scenarios && i == 16:
		vendor = g.e.EKKO[len(g.e.EKKO)-16].LIFNR // same vendor as this company's PO 0
	}
	g.poSeq++
	ebeln := fmt.Sprintf("45%02d%06d", g.year()%100, g.poSeq)
	month := 1 + i%6
	g.e.EKKO = append(g.e.EKKO, EKKO{EBELN: ebeln, BUKRS: cc, LIFNR: vendor, WAERS: mockCurrency(cc), BEDAT: g.date(month, 3)})

	var lines []mockPOLine
	for j := range 1 + g.r.IntN(3) {
		lines = append(lines, g.poLine(ebeln, i, j, scenarios))
	}

	partialGR := scenarios && i == 15
	for _, l := range lines {
		q := l.qty
		if partialGR {
			q = l.qty / 2
		}
		g.grSeq++
		g.e.MSEG = append(g.e.MSEG, MSEG{
			MBLNR: fmt.Sprintf("50%08d", g.grSeq), MJAHR: g.gj, ZEILE: "0001", BWART: "101",
			EBELN: ebeln, EBELP: l.item, MENGE: q.String(), BUDAT: g.date(month, 10),
		})
	}

	posted := i < 12
	g.invoice(cc, ebeln, vendor, month, i, lines, taxPct, posted, scenarios)
	if scenarios && i < len(corrections) {
		g.correction(cc, ebeln, vendor, month, corrections[i], lines[0], taxPct)
	}
}

// corrections are the parked documents that follow the posted invoices of
// the first three orders of each company with scenarios (1000 and 3000),
// one of each kind other than an invoice, by RBKP-XRECH and RSEG-TBTKZ
// (types.go).
type mockCorrection struct{ prefix, xrech, tbtkz string }

var corrections = []mockCorrection{
	{"CM", "", ""},   // credit memo: one unit of the first item returned
	{"SD", "X", "X"}, // subsequent debit: the first item revalued up
	{"SC", "", "X"},  // subsequent credit: the first item revalued down
}

// correction appends a parked correction of an order's first item. It
// draws nothing from the generator, so the rest of the extract is the same
// as without it. A credit memo credits one unit at the order price; a
// subsequent debit or credit revalues the item by 25.00, with the item's
// quantity as SAP proposes it, which moves none (SAP Help, "Subsequent
// Debits/Credits").
func (g *mockGen) correction(cc, ebeln, vendor string, month int, c mockCorrection, l mockPOLine, taxPct int64) {
	g.cmSeq++
	belnr := fmt.Sprintf("52%08d", g.cmSeq)
	qty, net := milli(perUnit), money.Minor(2500)
	if c.tbtkz == "" {
		net, _ = money.Scale(l.price, qty.String(), l.peinh) // mock prices are far from the range
	} else {
		qty = l.qty
	}
	g.e.RSEG = append(g.e.RSEG, RSEG{BELNR: belnr, GJAHR: g.gj, BUZEI: "000001", EBELN: ebeln, EBELP: l.item,
		MENGE: qty.String(), WRBTR: net.Plain(mockDecimals), TBTKZ: c.tbtkz})
	tax, _ := money.Percent(net, taxPct) // mock amounts are far from the range
	g.e.RBKP = append(g.e.RBKP, RBKP{
		BELNR: belnr, GJAHR: g.gj, BUKRS: cc, LIFNR: vendor, XBLNR: fmt.Sprintf("%s-%s-%04d", c.prefix, vendor[len(vendor)-4:], g.cmSeq),
		BLDAT: g.date(month, 20), WAERS: mockCurrency(cc), RMWWR: (net + tax).Plain(mockDecimals), WMWST1: tax.Plain(mockDecimals),
		RBSTAT: "A", XRECH: Indicator(c.xrech),
	})
}

// poLine generates item j of the i-th order, with the scenario overrides
// (fractional quantity, price unit, over-limit price), and appends it to
// EKPO.
func (g *mockGen) poLine(ebeln string, i, j int, scenarios bool) mockPOLine {
	l := mockPOLine{
		item:  fmt.Sprintf("%05d", (j+1)*10),
		qty:   milli((1 + g.r.Int64N(50)) * perUnit),
		price: g.money(1000, 200000),
		peinh: 1,
		sakto: expenseAccounts[g.r.IntN(len(expenseAccounts))],
	}
	if i == 5 && j == 0 {
		l.qty = 12500 // 12.5 units
	}
	if i == 7 && j == 0 {
		l.peinh = 100 // price per 100 units
		l.qty = 2400 * perUnit
	}
	if scenarios && i == 17 {
		l.price = g.money(2000000, 3000000) // pushes the invoice over the auto-post limit
		l.qty += 10 * perUnit
	}
	l.amount, _ = money.Scale(l.price, l.qty.String(), l.peinh) // mock quantities and prices are far from the range
	g.e.EKPO = append(g.e.EKPO, EKPO{
		EBELN: ebeln, EBELP: l.item, MATNR: fmt.Sprintf("MAT-%04d", g.r.IntN(9000)+1000),
		TXZ01: fmt.Sprintf("Item %d for %s", j+1, ebeln), MENGE: l.qty.String(),
		NETPR: l.price.Plain(mockDecimals), PEINH: fmt.Sprint(l.peinh), SAKTO: l.sakto,
	})
	return l
}

// revenue posts one month's sales (a trend growing ~7% a year, with +/-5%
// noise) and that month's payroll, so margins look like a real business.
func (g *mockGen) revenue(cc string, m int) {
	base := int64(30_000_000 + m*1_500_000) // 300,000.00 + 15,000.00 per month
	if cc == "2000" {
		base = base * 6 / 10
	}
	base = base * int64(100+7*(g.year()-2025)) / 100
	amt := money.Minor(base * (95 + g.r.Int64N(11)) / 100)
	g.fiDoc(cc, g.date(m, 28), "SA", fmt.Sprintf("SALES-%02d", m), []fiLine{
		{acct: "0000100000", amount: amt},
		{acct: "0000400000", amount: -amt},
	})
	payroll := money.Minor(base * 55 / 100)
	g.fiDoc(cc, g.date(m, 27), "SA", fmt.Sprintf("PAYROLL-%02d", m), []fiLine{
		{acct: "0000610000", amount: payroll},
		{acct: "0000100000", amount: -payroll},
	})
}

func (g *mockGen) year() int {
	y, _ := strconv.Atoi(g.gj) // the callers' fiscal year is four digits; mock data only
	return y
}

func mockCurrency(cc string) string {
	for _, c := range mockCompanies {
		if c.BUKRS == cc {
			return c.WAERS
		}
	}
	return ""
}

func (g *mockGen) invoice(cc, ebeln, vendor string, month, i int, lines []mockPOLine, taxPct int64, posted, scenarios bool) {
	g.ivSeq++
	belnr := fmt.Sprintf("51%08d", g.ivSeq)
	xblnr := fmt.Sprintf("INV-%s-%04d", vendor[len(vendor)-4:], 100+g.ivSeq)
	if scenarios && i == 16 {
		xblnr = g.ref[g.e.EKKO[len(g.e.EKKO)-17].EBELN] // reuse PO 0's invoice reference
	}

	var net money.Minor
	for k, l := range lines {
		amt := l.amount
		if scenarios && i == 14 && k == 0 {
			amt, _ = money.Percent(amt, 110) // 10% over PO price; mock amounts are far from the range
		}
		net += amt
		g.e.RSEG = append(g.e.RSEG, RSEG{
			BELNR: belnr, GJAHR: g.gj, BUZEI: fmt.Sprintf("%06d", k+1), EBELN: ebeln, EBELP: l.item,
			MENGE: l.qty.String(), WRBTR: amt.Plain(mockDecimals),
		})
	}
	tax, _ := money.Percent(net, taxPct) // mock amounts are far from the range
	gross := net + tax
	if scenarios && i == 18 {
		gross += 100 // vendor added tax incorrectly
	}
	status := "A"
	if posted {
		status = "5"
	}
	g.e.RBKP = append(g.e.RBKP, RBKP{
		BELNR: belnr, GJAHR: g.gj, BUKRS: cc, LIFNR: vendor, XBLNR: xblnr, BLDAT: g.date(month, 15),
		WAERS: mockCurrency(cc), RMWWR: gross.Plain(mockDecimals), WMWST1: tax.Plain(mockDecimals), RBSTAT: status, XRECH: Indicator("X"),
	})
	if !posted {
		return
	}
	g.ref[ebeln] = xblnr

	fl := make([]fiLine, 0, len(lines)+2)
	for _, l := range lines {
		fl = append(fl, fiLine{acct: l.sakto, amount: l.amount})
	}
	fl = append(fl, fiLine{acct: "0000140000", amount: tax}, fiLine{acct: "0000211000", amount: -gross, vendor: vendor})
	fiInv := g.fiDoc(cc, g.date(month, 16), "RE", xblnr, fl)

	if i%2 == 0 { // pay every other invoice and clear both open items
		pay := g.fiDoc(cc, g.date(month, 25), "KZ", xblnr, []fiLine{
			{acct: "0000211000", amount: gross, vendor: vendor},
			{acct: "0000100000", amount: -gross},
		})
		g.clear(cc, fiInv, vendor, pay)
		g.clear(cc, pay, vendor, pay)
	}
}

// downPayment posts an open down payment of 2,000.00 plus input tax at
// taxPct to the supplier of the company's second order, which SAP
// invoiced and did not pay (purchaseOrder: order 1 is posted, an odd one
// unpaid), in SAP's net display: Dr the down payment account as special
// G/L A on tax code V1, Dr input tax on V1, Cr bank. It draws nothing from
// the generator and comes after the company's documents, so the rest of
// the extract is the same as without it.
func (g *mockGen) downPayment(cc string, taxPct int64) {
	var orders []EKKO
	for _, h := range g.e.EKKO {
		if h.BUKRS == cc {
			orders = append(orders, h)
		}
	}
	vendor := orders[1].LIFNR
	net := money.Minor(200000)
	tax, _ := money.Percent(net, taxPct) // mock amounts are far from the range
	g.fiDoc(cc, g.date(3, 5), "KZ", "DP-"+vendor[len(vendor)-4:], []fiLine{
		{acct: downPaymentAccount, amount: net, vendor: vendor, umskz: "A", mwskz: "V1"},
		{acct: "0000140000", amount: tax, mwskz: "V1"},
		{acct: "0000100000", amount: -(net + tax)},
	})
	g.e.T074 = append(g.e.T074, T074{BUKRS: cc, KOART: "K", UMSKZ: "A", HKONT: "0000211000", SKONT: downPaymentAccount})
}

// receivedNotInvoiced adds an order of the company's first vendor that SAP
// valued through GR/IR: 8 shelving units at 150.00, received on 20 June
// (MSEG 101) and not yet invoiced, the receipt's document (WE) debiting the
// item's account and crediting the GR/IR account 1,200.00, both lines
// naming the order item. It draws nothing from the generator and comes
// after the company's documents, so the rest of the extract is the same as
// without it.
func (g *mockGen) receivedNotInvoiced(cc string) {
	var vendor string
	for _, v := range g.e.LFB1 {
		if v.BUKRS == cc && vendor == "" {
			vendor = v.LIFNR
		}
	}
	g.poSeq++
	ebeln := fmt.Sprintf("45%02d%06d", g.year()%100, g.poSeq)
	const item, account = "00010", "0000520000"
	g.e.EKKO = append(g.e.EKKO, EKKO{EBELN: ebeln, BUKRS: cc, LIFNR: vendor, WAERS: mockCurrency(cc), BEDAT: g.date(6, 2)})
	g.e.EKPO = append(g.e.EKPO, EKPO{EBELN: ebeln, EBELP: item, MATNR: "MAT-7001", TXZ01: "Shelving units", MENGE: "8", NETPR: "150.00", PEINH: "1", SAKTO: account})
	g.grSeq++
	g.e.MSEG = append(g.e.MSEG, MSEG{MBLNR: fmt.Sprintf("50%08d", g.grSeq), MJAHR: g.gj, ZEILE: "0001", BWART: "101", EBELN: ebeln, EBELP: item, MENGE: "8",
		BUDAT: g.date(6, 20)})
	g.fiDoc(cc, g.date(6, 20), "WE", "", []fiLine{
		{acct: account, amount: 120000, ebeln: ebeln, ebelp: item},
		{acct: grirAccount, amount: -120000, ebeln: ebeln, ebelp: item},
	})
	g.e.T030 = append(g.e.T030, T030{BUKRS: cc, KTOSL: "WRX", KONTS: grirAccount}, T030{BUKRS: cc, KTOSL: "PRD", KONTS: priceDifferenceAccount})
}

// foreignInvoice adds an invoice in USD to the company that SAP posted on
// 15 June and did not pay: USD 1,000.00 of office supplies from Tyrell
// Office Products (0000100011, one of 1000's vendors that 3000 buys from
// too), worth CAD 1,350.00 at 1.35, an open item in its document currency
// (ACDOCA RWCUR and WSL beside HSL, ADR-0005 As built). It draws nothing
// from the generator and comes after the company's documents, so the rest
// of the extract is the same as without it.
func (g *mockGen) foreignInvoice(cc string) {
	const vendor, account = "0000100011", "0000520000"
	g.fiDocIn(cc, "USD", g.date(6, 15), "RE", "TY-USD-1", []fiLine{
		{acct: account, amount: 135000, document: 100000},
		{acct: "0000211000", amount: -135000, document: -100000, vendor: vendor},
	})
}

// mockHouseBanks are a company's house bank MAIN in its currency: by
// IBAN, or for company 3000 a Canadian account, which has none, by its bank
// key (the routing as 0 + institution + transit) and account number; and
// company 3000's second house bank, MUSD, an account in USD at the same
// bank.
func mockHouseBanks(cc string, banks map[string][2]string) []T012K {
	if cc == "3000" {
		return []T012K{
			{BUKRS: cc, HBKID: "MAIN", HKONT: "0000100000", SWIFT: "ROYCCAT2", BANKS: "CA", BANKL: "000300001", BANKN: "5012345", WAERS: mockCurrency(cc)},
			{BUKRS: cc, HBKID: "MUSD", HKONT: usdBankAccount, SWIFT: "ROYCCAT2", BANKS: "CA", BANKL: "000300001", BANKN: "5012346", WAERS: "USD"},
		}
	}
	return []T012K{{BUKRS: cc, HBKID: "MAIN", HKONT: "0000100000", IBAN: banks[cc][0], SWIFT: banks[cc][1], WAERS: mockCurrency(cc)}}
}

// mockPaymentMethods sets the company up to pay by bank transfer: the
// method in its country (T042Z), in the company code (T042E), and each of
// its house banks ranked first for the method in its account's currency
// (T042A), posting to the bank's own G/L account (T042I).
func (g *mockGen) mockPaymentMethods(c T001) {
	if !slices.ContainsFunc(g.e.T042Z, func(z T042Z) bool { return z.LAND1 == c.LAND1 }) {
		g.e.T042Z = append(g.e.T042Z, T042Z{LAND1: c.LAND1, ZLSCH: transfer, TEXT1: "Bank transfer"})
	}
	g.e.T042E = append(g.e.T042E, T042E{ZBUKR: c.BUKRS, ZLSCH: transfer})
	for _, b := range g.e.T012K {
		if b.BUKRS == c.BUKRS {
			g.e.T042A = append(g.e.T042A, T042A{ZBUKR: c.BUKRS, ZLSCH: transfer, WAERS: b.WAERS, RANGF: "00001", HBKID: b.HBKID})
			g.e.T042I = append(g.e.T042I, T042I{ZBUKR: c.BUKRS, HBKID: b.HBKID, ZLSCH: transfer, WAERS: b.WAERS, HKTID: "00001", UKONT: b.HKONT})
		}
	}
}

// customizing adds the configuration tables a real system carries, including
// one deliberate segregation-of-duties conflict: user SUPER1 holds both
// invoice posting (MIRO) and payment release (BNK_APP).
func (g *mockGen) customizing() {
	g.e.T052 = []T052{
		{ZTERM: "NT14", ZTAG1: "14", TEXT1: "Net 14 days"},
		{ZTERM: "NT30", ZTAG1: "30", TEXT1: "Net 30 days"},
		{ZTERM: "NT45", ZTAG1: "45", TEXT1: "Net 45 days"},
		{ZTERM: "NT60", ZTAG1: "60", TEXT1: "Net 60 days"},
	}
	banks := map[string][2]string{
		"1000": {"GB33BUKB20201555555555", "BUKBGB22"},
		"2000": {"DE89370400440532013000", "COBADEFFXXX"},
	}
	for _, c := range mockCompanies {
		rate := "8"
		if c.BUKRS == "2000" {
			rate = "19"
		}
		if c.BUKRS == "3000" {
			rate = "5"
		}
		g.e.T007A = append(g.e.T007A,
			T007A{BUKRS: c.BUKRS, MWSKZ: "V1", TEXT1: "Input tax, standard rate", KBETR: rate, KONTS: "0000140000"},
			T007A{BUKRS: c.BUKRS, MWSKZ: "V0", TEXT1: "Input tax exempt", KBETR: "0", KONTS: "0000140000"})
		g.e.T030 = append(g.e.T030,
			T030{BUKRS: c.BUKRS, KTOSL: "VST", KONTS: "0000140000"},
			T030{BUKRS: c.BUKRS, KTOSL: "BNK", KONTS: "0000100000"})
		g.e.T169G = append(g.e.T169G, T169G{BUKRS: c.BUKRS, TOLSL: "PP", PROZ: "2", WERTO: "5.00"})
		g.e.T012K = append(g.e.T012K, mockHouseBanks(c.BUKRS, banks)...)
		g.mockPaymentMethods(c)
	}
	roles := []struct {
		name, text string
		tcodes     []string
		users      []string
	}{
		{"Z_AP_CLERK", "Accounts payable clerk", []string{"MIRO", "MIR7", "FBL1N"}, []string{"APCLERK1", "SUPER1"}},
		{"Z_BUYER", "Buyer", []string{"ME51N", "ME21N", "ME31K"}, []string{"BUYER1"}},
		{"Z_PURCH_MGR", "Purchasing manager", []string{"ME54N"}, []string{"PURCHMGR1"}},
		{"Z_WAREHOUSE", "Warehouse clerk", []string{"MIGO"}, []string{"WHCLERK1"}},
		{"Z_VENDOR_MAINT", "Vendor master maintenance", []string{"XK01", "XK02"}, []string{"MDCLERK1"}},
		{"Z_TREASURY", "Treasury clerk", []string{"F110"}, []string{"TREAS1"}},
		{"Z_TREASURY_MGR", "Treasury manager", []string{"BNK_APP"}, []string{"TREASMGR1", "SUPER1"}},
		{"Z_PROCESS_OWNER", "Process owner P2P", nil, []string{"OWNER1"}},
		{"Z_CONTROLLER", "Financial controller", []string{"FBL3N"}, []string{"CONTROLLER1"}},
		{"Z_SOLUTION_ARCHITECT", "Solution architect", []string{"STMS"}, []string{"ARCHITECT1"}},
		{"Z_MDG_APPROVER", "Master data approver", nil, []string{"MDMGR1"}},
	}
	for _, r := range roles {
		g.e.AGR_DEFINE = append(g.e.AGR_DEFINE, AGRDefine{AGR_NAME: r.name, TEXT: r.text})
		for _, t := range r.tcodes {
			g.e.AGR_TCODES = append(g.e.AGR_TCODES, AGRTcode{AGR_NAME: r.name, TCODE: t})
		}
		for _, u := range r.users {
			g.e.AGR_USERS = append(g.e.AGR_USERS, AGRUser{AGR_NAME: r.name, UNAME: u})
		}
	}
}
