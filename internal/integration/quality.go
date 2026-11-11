package integration

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Issue severities.
const (
	SevError   = "error"   // blocks the load
	SevWarning = "warning" // migrates, but needs a business decision
)

// Issue is a finding about the source data or a record the load refused.
type Issue struct {
	Severity string `json:"severity"`
	Table    string `json:"table"`
	Key      string `json:"key"`
	Message  string `json:"message"`
}

func hasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, func(i Issue) bool { return i.Severity == SevError })
}

// target is what the platform holds for a run's scope: the legal entity of
// each company code with its chart, the configured payment terms, number
// ranges and tax codes, and the minor-unit digits of every currency.
type target struct {
	entities  map[string]tenancy.Entity                 // by company code
	charts    map[string]map[string]ledger.ChartAccount // by company code, then account number
	terms     map[string]bool
	ranges    map[string]bool // company/document type/fiscal year
	exponents map[string]int
	// internal says the extract's amounts are in SAP's internal form, and
	// decimals are the decimals its TCURX gives the currencies it lists
	// (amounts.go).
	internal bool
	decimals map[string]int
	taxes    map[string]map[string]config.TaxCode // by company code, then tax code
	// methods are the payment methods of each company code's configuration
	// (ADR-0038), which its vendors' LFB1-ZWELS must name.
	methods map[string]map[string]bool
	// categories are the configuration's cost center categories
	// (controlling.cost_center_categories), which a migrated cost center's
	// KOSAR must name (ADR-0050 §10).
	categories map[string]bool
	year       int32
	balances   bool // balances mode
	// posted are the currencies earlier runs posted the extract's
	// documents in, by document key (postedCurrencies), which a document
	// SAP cleared together with them posts in too (foreignDocuments).
	posted map[string]string
	// today is the day the run loads, the cutover date of the master data
	// it migrates: a vendor's central address valid on it is the one taken
	// (po:2026-11-09, A1).
	today time.Time
}

func (t target) hasRange(company, documentType string) bool {
	return t.ranges[fmt.Sprintf("%s/%s/%d", company, documentType, t.year)]
}

// sapDate converts SAP YYYYMMDD to a date.
func sapDate(d string) (time.Time, error) {
	t, err := time.Parse("20060102", d)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid SAP date %q", d)
	}
	return t, nil
}

// normalizeRef is platform.normalize_ref: duplicate supplier references
// differ only in case and punctuation.
func normalizeRef(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			return r
		}
		return -1
	}, s)
}

// duplicateInvoices are the parked invoices that repeat an earlier
// invoice's supplier reference for the same vendor. Payables refuses them,
// so they are not migrated, and reconciliation leaves them out on the SAP
// side too.
func duplicateInvoices(e *sap.Extract) map[string]string {
	seen := map[string]string{}
	out := map[string]string{}
	for _, h := range e.RBKP {
		if h.RBSTAT != "A" {
			continue
		}
		key, ref := h.LIFNR+"/"+normalizeRef(h.XBLNR), h.BELNR+"/"+h.GJAHR
		if first, ok := seen[key]; ok {
			out[ref] = first
			continue
		}
		seen[key] = ref
	}
	return out
}

// checkQuality profiles an extract against the platform before anything is
// loaded. Problems found here are cheap to fix in SAP or in the
// configuration; problems found after cutover are not.
func checkQuality(e *sap.Extract, t target) []Issue {
	q := &qualityCheck{e: e, t: t, addresses: addressesOf(e)}
	q.foreign, q.conflicts = foreignDocuments(e, t)
	q.amountForm()
	q.companies()
	q.accounts()
	q.centers()
	q.vendors()
	q.vendorCompanies()
	q.vendorBanks()
	q.purchaseOrders()
	q.movements()
	q.ledgers()
	q.journal()
	q.reconciliationAccounts()
	q.lineCenters()
	q.downPayments()
	q.grirChecks()
	q.documentCurrencies()
	q.leftOutLines()
	q.supplierInvoices()
	return q.out
}

// qualityCheck carries the lookups that later checks build on (company
// currencies, accounts, vendors) and collects issues in the order the
// checks run, which is the order the report shows.
type qualityCheck struct {
	e        *sap.Extract
	t        target
	out      []Issue
	currency map[string]string   // by company code
	account  map[string]sap.SKB1 // by company code/account
	vendor   map[string]sap.LFA1 // by vendor number
	// addresses are the extract's central addresses (ADRC).
	addresses addresses
	// vendorAccounts are the company/account of each vendor's
	// reconciliation account (LFB1-AKONT), specialAccounts those T074 names
	// as special G/L accounts (specialGL).
	vendorAccounts, specialAccounts map[string]bool
	// positions are the extract's GR/IR positions (grirChecks), whose
	// open ones documentCurrencies checks too.
	positions map[string]*grirPosition
	// foreign are the documents the load posts in their foreign document
	// currency, and conflicts those it cannot post in a currency that
	// clears with earlier runs' documents (foreignDocuments).
	foreign, conflicts map[string]string
}

func (q *qualityCheck) add(sev, table, key, format string, args ...any) {
	q.out = append(q.out, Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
}

// companies checks each company code's currency against its legal entity.
func (q *qualityCheck) companies() {
	q.currency = map[string]string{}
	for _, c := range q.e.T001 {
		q.currency[c.BUKRS] = c.WAERS
		if le := q.t.entities[c.BUKRS]; c.WAERS != le.LocalCurrency {
			q.add(SevError, "T001", c.BUKRS, "company code currency %q is not the legal entity's local currency %s", c.WAERS, le.LocalCurrency)
		}
	}
}

// accounts checks each G/L account against the legal entity's chart.
func (q *qualityCheck) accounts() {
	q.account = map[string]sap.SKB1{}
	for _, a := range q.e.SKB1 {
		q.account[a.BUKRS+"/"+a.SAKNR] = a
		got, ok := q.t.charts[a.BUKRS][sap.Alpha(a.SAKNR)]
		switch {
		case !ok:
			q.add(SevError, "SKB1", a.BUKRS+"/"+a.SAKNR, "account %s is not in the legal entity's chart of accounts; add it through a configuration change", sap.Alpha(a.SAKNR))
		case (a.MITKZ == "K") != (got.ReconciliationKind == "supplier"):
			q.add(SevError, "SKB1", a.BUKRS+"/"+a.SAKNR, "account %s is a vendor reconciliation account on one side only", sap.Alpha(a.SAKNR))
		}
	}
}

// vendors checks the vendor masters and flags names that look duplicated.
func (q *qualityCheck) vendors() {
	q.vendor = map[string]sap.LFA1{}
	byName := map[string][]string{}
	for _, v := range q.e.LFA1 {
		q.vendor[v.LIFNR] = v
		if strings.TrimSpace(v.NAME1) == "" {
			q.add(SevError, "LFA1", v.LIFNR, "vendor has no name")
		}
		if strings.TrimSpace(v.STCD1) == "" {
			q.add(SevWarning, "LFA1", v.LIFNR, "vendor %q has no tax number", v.NAME1)
		}
		if _, _, why := vendorAddress(v, q.addresses, q.t.today); why != "" {
			q.add(SevWarning, "LFA1", v.LIFNR, "vendor %q has %s: it loads without an address", v.NAME1, why)
		}
		key := strings.ToUpper(strings.Join(strings.Fields(v.NAME1), " "))
		byName[key] = append(byName[key], v.LIFNR)
	}
	for _, name := range sortedKeys(byName) {
		if ids := byName[name]; len(ids) > 1 {
			q.add(SevWarning, "LFA1", strings.Join(ids, ","), "possible duplicate vendors named %q", name)
		}
	}
}

// vendorCompanies checks each vendor's company data: master, reconciliation
// account and payment terms.
func (q *qualityCheck) vendorCompanies() {
	q.vendorAccounts, q.specialAccounts = map[string]bool{}, map[string]bool{}
	for _, r := range q.e.T074 {
		q.specialAccounts[r.BUKRS+"/"+sap.Alpha(r.SKONT)] = true
	}
	for _, v := range q.e.LFB1 {
		q.vendorAccounts[v.BUKRS+"/"+sap.Alpha(v.AKONT)] = true
		if _, ok := q.vendor[v.LIFNR]; !ok {
			q.add(SevError, "LFB1", v.LIFNR, "company data without vendor master (LFA1)")
		}
		if a, ok := q.account[v.BUKRS+"/"+v.AKONT]; !ok || a.MITKZ != "K" {
			q.add(SevError, "LFB1", v.LIFNR, "reconciliation account %s is not a vendor reconciliation account of company %s", v.AKONT, v.BUKRS)
		}
		if !q.t.terms[v.ZTERM] {
			q.add(SevError, "LFB1", v.LIFNR, "payment terms %q are not in the configuration", v.ZTERM)
		}
		q.vendorMethods(v)
	}
}

// vendorMethods checks a vendor's payment methods (LFB1-ZWELS): each a
// method of the company's configuration, listed once, as the partner
// context requires of a supplier's company data (ADR-0038). The gate
// refuses the run rather than load the vendor without them.
func (q *qualityCheck) vendorMethods(v sap.LFB1) {
	seen := map[string]bool{}
	for _, m := range paymentMethods(v.ZWELS) {
		switch {
		case seen[m]:
			q.add(SevError, "LFB1", v.LIFNR, "payment method %q is listed twice in LFB1-ZWELS %q", m, v.ZWELS)
		case !q.t.methods[v.BUKRS][m]:
			q.add(SevError, "LFB1", v.LIFNR, "payment method %q (LFB1-ZWELS) is not a payment method of company %s in the configuration", m, v.BUKRS)
		}
		seen[m] = true
	}
}

// paymentMethods are the methods LFB1-ZWELS lists, one character each,
// in its order; blanks separate nothing and are skipped.
func paymentMethods(zwels string) []string {
	var out []string
	for _, r := range zwels {
		if r != ' ' {
			out = append(out, string(r))
		}
	}
	return out
}

// purchaseOrders checks order headers, then their items against the header.
func (q *qualityCheck) purchaseOrders() {
	poHeaders := map[string]sap.EKKO{}
	for _, h := range q.e.EKKO {
		poHeaders[h.EBELN] = h
		if _, ok := q.vendor[h.LIFNR]; !ok {
			q.add(SevError, "EKKO", h.EBELN, "vendor %s not in extract", h.LIFNR)
		}
		if _, err := sapDate(h.BEDAT); err != nil {
			q.add(SevError, "EKKO", h.EBELN, "%v", err)
		}
	}
	for _, i := range q.e.EKPO {
		q.purchaseOrderItem(i, poHeaders[i.EBELN]) // extracts keep the items of the scope's orders only
	}
}

func (q *qualityCheck) purchaseOrderItem(i sap.EKPO, h sap.EKKO) {
	key := i.EBELN + "/" + i.EBELP
	if _, ok := q.t.charts[h.BUKRS][sap.Alpha(i.SAKTO)]; !ok {
		q.add(SevError, "EKPO", key, "G/L account %s is not in the chart of company %s", sap.Alpha(i.SAKTO), h.BUKRS)
	}
	_, why := lineValue(i, q.t, h.WAERS)
	for _, w := range why {
		q.add(SevError, "EKPO", key, "%s", w)
	}
}

// lineValue is a purchase order line's net value, or why it has none: the
// quantity, price or price unit does not parse, or the value is out of
// range. The quality gate and the reconciliation both refuse such lines.
func lineValue(i sap.EKPO, t target, currency string) (money.Minor, []string) {
	var why []string
	if err := money.PositiveQuantity(i.MENGE); err != nil {
		why = append(why, err.Error())
	}
	if _, err := t.amount(i.NETPR, currency); err != nil {
		why = append(why, err.Error())
	}
	if n, err := strconv.Atoi(i.PEINH); i.PEINH != "" && (err != nil || n < 1) {
		why = append(why, fmt.Sprintf("price unit %q is not a positive whole number", i.PEINH))
	}
	if why != nil {
		return 0, why
	}
	v, err := extend(i.MENGE, i.NETPR, i.PEINH, t, currency)
	if err != nil { // money.ErrOverflow: the inputs are checked above
		return 0, []string{fmt.Sprintf("net value of %s at %s per %s is out of range", i.MENGE, i.NETPR, cmp.Or(i.PEINH, "1"))}
	}
	return v, nil
}

// movements warns about goods movements other than receipts.
func (q *qualityCheck) movements() {
	for _, m := range q.e.MSEG {
		if m.BWART != "101" {
			q.add(SevWarning, "MSEG", m.MBLNR+"/"+m.MJAHR+"/"+m.ZEILE, "movement type %s is not migrated; received quantities count receipts (101) only", m.BWART)
		}
	}
}

// ledgers refuses an extract holding journal lines of other ledgers than
// the leading one (ledgerIssue).
func (q *qualityCheck) ledgers() {
	if issue, ok := ledgerIssue(q.e); ok {
		q.out = append(q.out, issue)
	}
}

// journalScan accumulates what the Universal Journal lines add up to.
// sizes holds each company's line amounts without sign: their sum bounds
// every sum the load and the reconciliation form of the company's lines
// (balances per account, debits, open and cleared items).
type journalScan struct {
	docLines     map[string][]money.Minor
	sizes        map[string][]money.Minor
	openByVendor map[string]bool
	noRange      map[string]bool
}

// journal checks the Universal Journal: every document must balance, hit
// known accounts and have a number range for its document type.
func (q *qualityCheck) journal() {
	s := journalScan{docLines: map[string][]money.Minor{}, sizes: map[string][]money.Minor{}, openByVendor: map[string]bool{}, noRange: map[string]bool{}}
	for _, l := range q.e.ACDOCA {
		q.journalLine(l, &s)
	}
	for _, key := range sortedKeys(s.docLines) {
		if total, err := money.Sum(s.docLines[key]...); err != nil || total != 0 {
			q.add(SevError, "ACDOCA", key, "document does not balance (%s)", offBy(total, err))
		}
	}
	for _, cc := range sortedKeys(s.sizes) {
		if _, err := money.Sum(s.sizes[cc]...); err != nil {
			q.add(SevError, "ACDOCA", cc, "the journal lines of company %s add up to more than an amount holds", cc)
		}
	}
	for _, id := range sortedKeys(s.openByVendor) {
		if v := q.vendor[id]; v.SPERR == "X" {
			q.add(SevWarning, "LFA1", id, "vendor %q is posting-blocked but has open payables", v.NAME1)
		}
	}
	if q.t.balances {
		for _, c := range q.e.T001 {
			if !q.t.hasRange(c.BUKRS, "journal_ob") {
				q.add(SevError, "T001", c.BUKRS, "the configuration has no journal_ob number range for the opening entry in %d", q.t.year)
			}
		}
	}
}

func (q *qualityCheck) journalLine(l sap.ACDOCA, s *journalScan) {
	key := l.RBUKRS + "/" + l.GJAHR + "/" + l.BELNR
	amt, err := q.t.amount(l.HSL, q.currency[l.RBUKRS])
	if err != nil {
		q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "%v", err)
		return
	}
	s.docLines[key] = append(s.docLines[key], amt)
	s.sizes[l.RBUKRS] = append(s.sizes[l.RBUKRS], amt.Abs()) // parsed amounts are never -2^63
	a, ok := q.account[l.RBUKRS+"/"+l.RACCT]
	if !ok {
		q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "account %s missing from chart of accounts", l.RACCT)
	} else if a.MITKZ == "K" && l.LIFNR == "" {
		q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "posting to vendor reconciliation account %s without vendor", l.RACCT)
	}
	if l.LIFNR != "" {
		if _, known := q.vendor[l.LIFNR]; !known {
			q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "vendor %s not in extract", l.LIFNR)
		}
		if l.AUGBL == "" && ok && a.MITKZ == "K" {
			s.openByVendor[l.LIFNR] = true
		}
	}
	if _, err := sapDate(l.BUDAT); err != nil {
		q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "%v", err)
	}
	q.specialGL(l, key+"/"+l.DOCLN)
	if documentType := "journal_" + strings.ToLower(l.BLART); !q.t.balances && !q.t.hasRange(l.RBUKRS, documentType) && !s.noRange[l.RBUKRS+documentType] {
		s.noRange[l.RBUKRS+documentType] = true
		q.add(SevError, "ACDOCA", key, "the configuration has no %s number range for company %s in %d", documentType, l.RBUKRS, q.t.year)
	}
}

// openItem reports whether a journal line is an open item: a vendor item
// SAP has not cleared, on a supplier reconciliation account (an ordinary
// payable or a down payment, as the load gives it its partner), or a line
// of a GR/IR position the load takes over, with an amount in company
// currency (HSL) or in its document currency (hasDocumentAmount). One with
// a document amount alone is an item the load would leave out, posting
// no line of no company code amount, so documentCurrencies refuses it
// (review:batch31). A line whose HSL does not parse is journalLine's to
// refuse.
func (q *qualityCheck) openItem(l sap.ACDOCA) bool {
	amt, err := q.t.amount(l.HSL, q.currency[l.RBUKRS])
	if err != nil || amt == 0 && !q.hasDocumentAmount(l) {
		return false
	}
	account := q.t.charts[l.RBUKRS][sap.Alpha(l.RACCT)]
	if account.ReconciliationKind == "supplier" {
		return l.AUGBL == ""
	}
	p := q.positions[l.EBELN+"/"+l.EBELP]
	return account.GRIR && p != nil && p.open()
}

// supplierInvoices checks that each parked invoice document has a kind
// (documentKind) and that its header agrees with its items and tax, and
// that each posted one has a direction and quantities to take over
// (postedInvoiced).
func (q *qualityCheck) supplierInvoices() {
	itemTotals := q.invoiceItemTotals()
	dups := duplicateInvoices(q.e)
	items := invoiceItems(q.e)
	for _, h := range q.e.RBKP {
		q.supplierInvoice(h, items[h.BELNR+"/"+h.GJAHR], itemTotals, dups)
	}
}

// invoiceItemTotals sums each invoice's items in the invoice currency.
func (q *qualityCheck) invoiceItemTotals() map[string]money.Minor {
	invCurrency := map[string]string{}
	for _, h := range q.e.RBKP {
		invCurrency[h.BELNR+"/"+h.GJAHR] = h.WAERS
	}
	itemTotals := map[string]money.Minor{}
	for _, it := range q.e.RSEG {
		amt, err := q.t.amount(it.WRBTR, invCurrency[it.BELNR+"/"+it.GJAHR])
		if err != nil {
			q.add(SevError, "RSEG", it.BELNR+"/"+it.BUZEI, "%v", err)
			continue
		}
		itemTotals[it.BELNR+"/"+it.GJAHR] += amt
	}
	return itemTotals
}

func (q *qualityCheck) supplierInvoice(h sap.RBKP, items []sap.RSEG, itemTotals map[string]money.Minor, dups map[string]string) {
	key := h.BELNR + "/" + h.GJAHR
	gross, err1 := q.t.amount(h.RMWWR, h.WAERS)
	tax, err2 := q.t.amount(h.WMWST1, h.WAERS)
	_, err3 := sapDate(h.BLDAT)
	switch {
	case err1 != nil || err2 != nil || err3 != nil:
		q.add(SevError, "RBKP", key, "unparseable amounts %q / %q or date %q", h.RMWWR, h.WMWST1, h.BLDAT)
		return
	case h.RBSTAT == "5":
		// Posted in SAP: its journal entry carries it, and the load takes
		// over the quantities it invoiced, which need its kind (T72b2).
		if _, issue := postedInvoiced(h, items); issue != nil {
			q.out = append(q.out, *issue)
		}
		return
	case h.RBSTAT != "A":
		q.add(SevWarning, "RBKP", key, "invoice status %q is not migrated", h.RBSTAT)
		return
	case dups[key] != "":
		q.add(SevWarning, "RBKP", key, "supplier reference %q repeats invoice %s; it is not migrated", h.XBLNR, dups[key])
		return
	}
	if _, issue := documentKind(h, items); issue != nil {
		q.out = append(q.out, *issue)
		return
	}
	if net := itemTotals[key]; net+tax != gross {
		q.add(SevWarning, "RBKP", key, "gross %d != items %d + tax %d minor units (it will be held at matching)", gross, net, tax)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// offBy renders a document's imbalance, which can be out of range.
func offBy(total money.Minor, err error) string {
	if err != nil {
		return "its lines add up to more than an amount holds"
	}
	return "off by " + strconv.FormatInt(int64(total), 10) + " minor units"
}
