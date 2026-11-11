package integration

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Check is one reconciliation control: a figure computed independently
// from the SAP extract and from the platform, which must agree.
type Check struct {
	CompanyCode string `json:"company_code"`
	Name        string `json:"name"`
	SAP         string `json:"sap"`
	Platform    string `json:"platform"`
	Pass        bool   `json:"pass"`
}

// Diff is a difference behind a failed check: amounts in minor units, a
// vendor's bank account as bankAccount writes it, or an order item's
// quantity invoiced in SAP (invoicedQuantities).
type Diff struct {
	CompanyCode string `json:"company_code"`
	Key         string `json:"key"` // "cost center <KOKRS>/<KOSTL>" or "profit center <KOKRS>/<PRCTR>" (their periods, ADR-0050 §10), "cost center total <KOKRS>/<KOSTL>", "profit center total <KOKRS>/<PRCTR>" or "segment total <SEGMENT>" (migrated totals, T56h4), account number, "vendor <LIFNR>", "down payments of vendor <LIFNR>", "partner <id> (<name>)" or "down payments of partner <id> (<name>)" (a platform supplier that is no migrated vendor), "bank account of vendor <LIFNR>", "order item <EBELN>/<EBELP>", "GR/IR of order item <EBELN>/<EBELP>", "open items of vendor <LIFNR>" or "open items of partner <id> (<name>)", "<currency> items of vendor <LIFNR>" or "<currency> items of partner <id> (<name>)" (open items in a foreign document currency), "trading partner <company code>" or "trading partner <id>" (open payables by trading partner: by the SAP company code of the vendors' trading partner, or by the id of a legal entity no vendor of the extract names), "payment method <method> items of vendor <LIFNR>" or "payment method <method> items of partner <id> (<name>)" (open items that fix that payment method: their number and total)
	SAP         string `json:"sap"`
	Platform    string `json:"platform"`
}

// Reconciliation is the reconciliation part of a run's report. Issues are
// SAP values it could not use: amounts that do not parse, sums out of the
// range of an amount, or journal lines of other ledgers than the leading
// one (ledgerIssue), which it refuses without computing a control. A
// reconciliation run extracts again without the quality gate, so it
// reports them as the gate does, and fails.
type Reconciliation struct {
	Checks  []Check  `json:"checks"`
	Diffs   []Diff   `json:"diffs"`
	Missing []string `json:"missing_documents,omitempty"` // SAP documents of the scope not on the platform
	Issues  []Issue  `json:"issues,omitempty"`
	Passed  bool     `json:"passed"`
}

// reconcile proves a load. The SAP side is recomputed from the raw extract,
// not from what was loaded, so a load bug shows up as a difference instead
// of being reproduced on both sides. Journal totals are the legal entity's
// for the fiscal year; counts cover the records this or earlier runs
// loaded for the scope's SAP keys.
func (e *Endpoints) reconcile(ctx context.Context, ext *sap.Extract, t target, r refs) (Reconciliation, error) {
	if issue, ok := ledgerIssue(ext); ok { // extracted again without the gate: refused, never summed twice
		return Reconciliation{Issues: []Issue{issue}}, nil
	}
	rc := &reconciler{e: e, ext: ext, t: t, r: r, vendorOf: map[uuid.UUID]string{}, recon: map[string]bool{}, pnl: map[string]bool{},
		partnerOf: map[string]string{}}
	for _, v := range ext.LFA1 {
		rc.partnerOf[v.LIFNR] = sap.CompanyID(v.VBUND)
	}
	for key, id := range r {
		if lifnr, ok := strings.CutPrefix(key, "LFA1/"); ok {
			rc.vendorOf[id] = sap.Alpha(lifnr)
		}
	}
	for _, a := range ext.SKB1 {
		rc.recon[a.BUKRS+"/"+a.SAKNR], rc.pnl[a.BUKRS+"/"+a.SAKNR] = a.MITKZ == "K", a.XBILK != "X"
	}
	codes := make([]string, 0, len(ext.T001))
	currency := map[string]string{}
	for _, c := range ext.T001 {
		codes, currency[c.BUKRS] = append(codes, c.BUKRS), c.WAERS
	}
	slices.Sort(codes)
	for _, cc := range codes {
		if err := rc.company(ctx, cc, currency[cc]); err != nil {
			return rc.rec, err
		}
	}
	rc.rec.Passed = !slices.ContainsFunc(rc.rec.Checks, func(c Check) bool { return !c.Pass }) && !hasErrors(rc.rec.Issues)
	return rc.rec, nil
}

// reconciler holds a reconciliation in progress and the lookups shared by
// every company code. A failed step leaves the checks recorded so far in
// rec, which is returned with the error.
type reconciler struct {
	e        *Endpoints
	ext      *sap.Extract
	t        target
	r        refs
	rec      Reconciliation
	vendorOf map[uuid.UUID]string // partner id -> SAP vendor number
	recon    map[string]bool      // company/account -> vendor reconciliation account
	pnl      map[string]bool      // company/account -> profit and loss account (SKB1-XBILK blank)
	states   *centerStates        // the centers runs loaded, read once (readCenters)
	// partnerOf is each vendor's trading partner, the company ID its master
	// names (LFA1-VBUND, sap.CompanyID), "" for none.
	partnerOf map[string]string
}

// companyScope is one company code under reconciliation.
type companyScope struct {
	cc     string
	le     uuid.UUID
	cur    string
	format func(money.Minor) string
}

func (rc *reconciler) add(cc, name, sapValue, platformValue string, pass bool) {
	rc.rec.Checks = append(rc.rec.Checks, Check{CompanyCode: cc, Name: name, SAP: sapValue, Platform: platformValue, Pass: pass})
}

// refuse records a value the reconciliation cannot use, as the quality
// gate records an error.
func (rc *reconciler) refuse(table, key, format string, args ...any) {
	rc.rec.Issues = append(rc.rec.Issues, Issue{Severity: SevError, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
}

// total adds terms exactly (money.Sum). A total out of range is refused
// and counts as zero: the reconciliation fails on the issue, never on a
// wrapped figure that might agree.
func (rc *reconciler) total(table, key, what string, terms []money.Minor) money.Minor {
	t, err := money.Sum(terms...)
	if err != nil {
		rc.refuse(table, key, "%s add up to more than an amount holds", what)
	}
	return t
}

// totals is total for each key's terms, described by what.
func (rc *reconciler) totals(table, cc string, terms map[string][]money.Minor, what func(string) string) map[string]money.Minor {
	out := make(map[string]money.Minor, len(terms))
	for _, k := range sortedKeys(terms) {
		out[k] = rc.total(table, cc+"/"+k, what(k), terms[k])
	}
	return out
}

// company runs the controls of one company code in report order.
func (rc *reconciler) company(ctx context.Context, cc, cur string) error {
	exp := rc.t.exponents[cur]
	s := companyScope{cc: cc, le: rc.t.entities[cc].ID, cur: cur, format: func(v money.Minor) string { return v.Plain(exp) + " " + cur }}
	j, err := rc.trialBalance(ctx, s)
	if err != nil {
		return err
	}
	if err := rc.openPayables(ctx, s, j.open); err != nil {
		return err
	}
	if err := rc.openPayableItems(ctx, s, j.openItems); err != nil {
		return err
	}
	if err := rc.downPayments(ctx, s, j.downPay); err != nil {
		return err
	}
	if err := rc.tradingPartners(ctx, s, j.byPartner); err != nil {
		return err
	}
	if err := rc.foreignItems(ctx, s); err != nil {
		return err
	}
	if err := rc.fixedMethods(ctx, s, j.fixed); err != nil {
		return err
	}
	if err := rc.vendors(ctx, s); err != nil {
		return err
	}
	st, err := rc.centers(ctx, s)
	if err != nil {
		return err
	}
	if err := rc.centerTotals(ctx, s, st); err != nil {
		return err
	}
	if err := rc.bankAccounts(ctx, s); err != nil {
		return err
	}
	if err := rc.purchaseOrders(ctx, s); err != nil {
		return err
	}
	if err := rc.parkedInvoices(ctx, s); err != nil {
		return err
	}
	if err := rc.invoicedQuantities(ctx, s); err != nil {
		return err
	}
	return rc.grir(ctx, s)
}

// sapJournal is the SAP side of the journal controls of a company code.
type sapJournal struct {
	tb, open  map[string]money.Minor // per account; per "vendor <LIFNR>"
	openItems map[string]int         // the number of open items (not of amount 0) per "vendor <LIFNR>"
	downPay   map[string]money.Minor // open down payments (special G/L A) per "vendor <LIFNR>"
	byPartner map[string]money.Minor // open items, down payments included, per "trading partner <company code>" (sapTradingPartner)
	fixed     fixedByMethod          // open items that fix a payment method (ZLSCH), by method and "vendor <LIFNR>"
	debits    money.Minor
	documents map[string]bool
}

// journal adds up the company's journal lines from the extract: per
// account, debits, and open items per vendor, the down payments (special
// G/L A) apart. The load's quality gate
// refused amounts and sums out of range, but a reconciliation run extracts
// again without it, so they are checked here too.
func (rc *reconciler) journal(s companyScope) sapJournal {
	tb, open, downPay, byPartner := map[string][]money.Minor{}, map[string][]money.Minor{}, map[string][]money.Minor{}, map[string][]money.Minor{}
	var debits []money.Minor
	documents, openItems := map[string]bool{}, map[string]int{}
	fixed := fixedByMethod{}
	for _, l := range rc.ext.ACDOCA {
		if l.RBUKRS != s.cc {
			continue
		}
		documents[documentKey(l)] = true
		amt, err := rc.t.amount(l.HSL, s.cur)
		if err != nil {
			rc.refuse("ACDOCA", documentKey(l)+"/"+l.DOCLN, "%v", err)
			continue
		}
		account := sap.Alpha(l.RACCT)
		tb[account] = append(tb[account], amt)
		debits = append(debits, max(amt, 0))
		if l.LIFNR != "" && l.AUGBL == "" && rc.recon[s.cc+"/"+l.RACCT] {
			vendor := "vendor " + sap.Alpha(l.LIFNR)
			if key, ok := rc.sapTradingPartner(s, l.LIFNR); ok {
				byPartner[key] = append(byPartner[key], amt)
			}
			if l.UMSKZ == specialDownPayment {
				downPay[vendor] = append(downPay[vendor], amt)
			} else {
				open[vendor] = append(open[vendor], amt)
				if amt != 0 {
					openItems[vendor]++
				}
				fixed.addLine(l, vendor, amt)
			}
		}
	}
	return sapJournal{
		tb:        rc.totals("ACDOCA", s.cc, tb, func(a string) string { return "the lines of account " + a }),
		open:      rc.totals("ACDOCA", s.cc, open, func(v string) string { return "the open items of " + v }),
		downPay:   rc.totals("ACDOCA", s.cc, downPay, func(v string) string { return "the open down payments of " + v }),
		byPartner: rc.totals("ACDOCA", s.cc, byPartner, func(p string) string { return "the open items of " + p }),
		debits:    rc.total("ACDOCA", s.cc, "the debits of company "+s.cc, debits),
		documents: documents,
		openItems: openItems,
		fixed:     fixed,
	}
}

// trialBalance compares the trial balance per account, and debits when
// every document came over. It returns the SAP side, whose open payables
// and down payments per vendor come out of the same pass over the journal.
func (rc *reconciler) trialBalance(ctx context.Context, s companyScope) (sapJournal, error) {
	j := rc.journal(s)
	sapTB, sapDebits, documents := j.tb, j.debits, j.documents
	totals, err := rc.e.Reports.Totals(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return j, err
	}
	tb := diffs(s.cc, sapTB, totals.ByAccount, s.format)
	rc.rec.Diffs = append(rc.rec.Diffs, tb...)
	rc.add(s.cc, "Trial balance: accounts with differences", "0", strconv.Itoa(len(tb)), len(tb) == 0)
	if !rc.t.balances {
		rc.add(s.cc, "Total debits posted", s.format(sapDebits), s.format(totals.Debits), sapDebits == totals.Debits)
		for _, doc := range sortedKeys(documents) {
			if _, ok := rc.r["ACDOCA/"+doc]; !ok && len(rc.rec.Missing) < 100 {
				rc.rec.Missing = append(rc.rec.Missing, doc)
			}
		}
	}
	return j, nil
}

// openPayables compares open payables per vendor, the down payments
// (special G/L items) apart, as a payment run nets the one and not the
// other (ADR-0037, T72c): a down payment loaded as an ordinary item fails
// both comparisons.
func (rc *reconciler) openPayables(ctx context.Context, s companyScope, sapOpen map[string]money.Minor) error {
	open, err := rc.e.Reports.OpenPayables(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	return rc.openItems(ctx, s, "Open payables", "", sapOpen, open)
}

// openPayableItems compares the number of open payable items per vendor
// (review:batch22). A clearing SAP recorded and the platform did not
// leaves both items open, which nets away in the open payables; the
// count shows it, keyed "open items of vendor <LIFNR>" (or of a partner
// that is no migrated vendor). The load leaves out lines of amount 0, so
// they are not counted.
func (rc *reconciler) openPayableItems(ctx context.Context, s companyScope, sapItems map[string]int) error {
	counts, err := rc.e.Reports.OpenPayableItems(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	platform, keys := map[string]int{}, map[string]bool{}
	for id, n := range counts {
		key, err := rc.vendorKey(ctx, s, id)
		if err != nil {
			return err
		}
		platform[key], keys[key] = platform[key]+n, true
	}
	for key := range sapItems {
		keys[key] = true
	}
	var sapTotal, platformTotal, differ int
	for _, key := range sortedKeys(keys) {
		sapTotal, platformTotal = sapTotal+sapItems[key], platformTotal+platform[key]
		if sapItems[key] != platform[key] {
			differ++
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: s.cc, Key: "open items of " + key, SAP: strconv.Itoa(sapItems[key]),
				Platform: strconv.Itoa(platform[key])})
		}
	}
	rc.add(s.cc, "Open payable items", strconv.Itoa(sapTotal), strconv.Itoa(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, "Open payable items: vendors with differences", "0", strconv.Itoa(differ), differ == 0)
	return nil
}

// downPayments compares the open down payments per vendor, when either
// side has any, so a scope without them reports as before.
func (rc *reconciler) downPayments(ctx context.Context, s companyScope, sapOpen map[string]money.Minor) error {
	open, err := rc.e.Reports.OpenDownPayments(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	if len(sapOpen) > 0 || len(open) > 0 {
		return rc.openItems(ctx, s, "Open down payments", "down payments of ", sapOpen, open)
	}
	return nil
}

// openItems compares open items per vendor, SAP's by "vendor <LIFNR>" and
// the platform's per partner, as the checks name's total and vendors
// with differences, each difference keyed prefix + "vendor <LIFNR>", or
// prefix + "partner <id> (<name>)" for a platform supplier that is no
// migrated vendor, which SAP's side cannot name.
func (rc *reconciler) openItems(ctx context.Context, s companyScope, name, prefix string, sapOpen map[string]money.Minor,
	open map[uuid.UUID]money.Minor) error {
	byVendor := map[string][]money.Minor{}
	for id, amt := range open {
		key, err := rc.vendorKey(ctx, s, id)
		if err != nil {
			return err
		}
		byVendor[key] = append(byVendor[key], amt)
	}
	what := strings.ToLower(name)
	platformOpen := rc.totals("platform", s.cc, byVendor, func(v string) string { return "the platform's " + what + " of " + v })
	ap := diffs(s.cc, sapOpen, platformOpen, s.format)
	for i := range ap {
		ap[i].Key = prefix + ap[i].Key
	}
	rc.rec.Diffs = append(rc.rec.Diffs, ap...)
	sapTotal := rc.total("ACDOCA", s.cc, "the "+what+" of company "+s.cc, slices.Collect(maps.Values(sapOpen)))
	platformTotal := rc.total("platform", s.cc, "the platform's "+what+" of company "+s.cc, slices.Collect(maps.Values(platformOpen)))
	rc.add(s.cc, name+" total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, name+": vendors with differences", "0", strconv.Itoa(len(ap)), len(ap) == 0)
	return nil
}

// vendorKey names a partner with open items as the report does: by its SAP
// vendor number when the migration created it, else by its id and name.
func (rc *reconciler) vendorKey(ctx context.Context, s companyScope, id uuid.UUID) (string, error) {
	if lifnr, ok := rc.vendorOf[id]; ok {
		return "vendor " + lifnr, nil
	}
	// A partner is never deleted and this one holds open items, so it is
	// found: ok is always true.
	sup, _, err := rc.e.Suppliers.Supplier(ctx, id, s.le)
	if err != nil {
		return "", err
	}
	return "partner " + id.String() + " (" + sup.Name + ")", nil
}

// vendors compares the vendors set up in the legal entity.
func (rc *reconciler) vendors(ctx context.Context, s companyScope) error {
	sapVendors, platformVendors := 0, 0
	for _, v := range rc.ext.LFB1 {
		if v.BUKRS != s.cc {
			continue
		}
		sapVendors++
		if id, ok := rc.r["LFA1/"+v.LIFNR]; ok {
			// A partner not found is not set up (SetUp is false), so it is
			// not counted, which the comparison then reports.
			sup, _, err := rc.e.Suppliers.Supplier(ctx, id, s.le)
			if err != nil {
				return err
			}
			if sup.SetUp {
				platformVendors++
			}
		}
	}
	rc.add(s.cc, "Vendors", strconv.Itoa(sapVendors), strconv.Itoa(platformVendors), sapVendors == platformVendors)
	return nil
}

// purchaseOrders compares purchase order count and value.
func (rc *reconciler) purchaseOrders(ctx context.Context, s companyScope) error {
	sapPOs := 0
	var orders []uuid.UUID
	var values []money.Minor
	inCC := map[string]bool{}
	for _, h := range rc.ext.EKKO {
		if h.BUKRS == s.cc {
			sapPOs++
			inCC[h.EBELN] = true
			if id, ok := rc.r["EKKO/"+h.EBELN]; ok {
				orders = append(orders, id)
			}
		}
	}
	for _, i := range rc.ext.EKPO {
		if !inCC[i.EBELN] {
			continue
		}
		// The load's quality gate refused lines without a value
		// (purchaseOrderItem); a reconciliation run extracts again without it.
		v, why := lineValue(i, rc.t, s.cur)
		for _, w := range why {
			rc.refuse("EKPO", i.EBELN+"/"+i.EBELP, "%s", w)
		}
		values = append(values, v)
	}
	sapPOValue := rc.total("EKPO", s.cc, "the purchase order lines of company "+s.cc, values)
	n, value, err := rc.e.OrderValues.Value(ctx, orders)
	if err != nil {
		return err
	}
	rc.add(s.cc, "Purchase orders", strconv.Itoa(sapPOs), strconv.Itoa(n), sapPOs == n)
	rc.add(s.cc, "Purchase order value", s.format(sapPOValue), s.format(value), sapPOValue == value)
	return nil
}

// parkedChecks name the parked documents' checks by kind. Invoices are
// always checked; the other kinds when either side has one, so a scope
// without corrections reports as before (T72b).
var parkedChecks = []struct{ kind, name string }{
	{payables.KindInvoice, "Parked supplier invoices"},
	{payables.KindCreditMemo, "Parked supplier credit memos"},
	{payables.KindSubsequentDebit, "Parked subsequent debits"},
	{payables.KindSubsequentCredit, "Parked subsequent credits"},
}

// parkedInvoices compares parked supplier documents by kind (posted ones
// are carried by their journal entries): SAP's by RBKP-XRECH and
// RSEG-TBTKZ (parkedKinds), the platform's by the kind they were entered
// as, so a credit memo loaded as an invoice fails two checks.
func (rc *reconciler) parkedInvoices(ctx context.Context, s companyScope) error {
	kinds := parkedKinds(rc.ext)
	sapParked := map[string]int{}
	var invoices []uuid.UUID
	for _, h := range rc.ext.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		kind, ok := kinds[key]
		if h.BUKRS != s.cc || !ok {
			continue
		}
		sapParked[kind]++
		if id, ok := rc.r["RBKP/"+key]; ok {
			invoices = append(invoices, id)
		}
	}
	parked, err := rc.e.InvoiceCounts.Count(ctx, invoices)
	if err != nil {
		return err
	}
	for i, c := range parkedChecks {
		if i == 0 || sapParked[c.kind] != 0 || parked[c.kind] != 0 {
			rc.add(s.cc, c.name, strconv.Itoa(sapParked[c.kind]), strconv.Itoa(parked[c.kind]), sapParked[c.kind] == parked[c.kind])
		}
	}
	return nil
}

// invoicedQuantities compares, per order item of the company, the net
// quantity SAP's posted invoice documents invoiced (credit memos
// negative, subsequent debit/credit items none) with what the platform took
// over (T72b2), and lists the order items that differ. The check is shown
// when either side has a quantity, so a scope without posted invoices
// reports as before. SAP's side is the extract's, every posted document
// on the company's orders, whether or not the load took it over yet.
func (rc *reconciler) invoicedQuantities(ctx context.Context, s companyScope) error {
	orders := map[string]bool{}
	var ids []uuid.UUID
	ebeln := map[uuid.UUID]string{}
	for _, h := range rc.ext.EKKO {
		if h.BUKRS != s.cc {
			continue
		}
		orders[h.EBELN] = true
		if id, ok := rc.r["EKKO/"+h.EBELN]; ok {
			ids, ebeln[id] = append(ids, id), h.EBELN
		}
	}
	sapQty := rc.sapInvoiced(s.cc, orders)
	taken, err := rc.e.Invoiced.MigratedQuantities(ctx, ids)
	if err != nil {
		return err
	}
	ebelp := itemsByNo(rc.ext)
	platform := map[string]*big.Rat{}
	for id, items := range taken {
		for no, qty := range items {
			order := ebeln[id]
			platform["order item "+order+"/"+ebelp[order+"/"+strconv.Itoa(int(no))]] = money.ParseRat(qty)
		}
	}
	if len(sapQty) == 0 && len(platform) == 0 {
		return nil
	}
	n := 0
	for _, key := range sortedKeys(unionKeys(sapQty, platform)) {
		a, b := orZero(sapQty[key]), orZero(platform[key])
		if a.Cmp(b) != 0 {
			n++
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: s.cc, Key: key, SAP: money.PlainQuantity(a), Platform: money.PlainQuantity(b)})
		}
	}
	rc.add(s.cc, "Quantities invoiced in SAP: order items with differences", "0", strconv.Itoa(n), n == 0)
	return nil
}

// sapInvoiced is the net quantity the posted documents invoiced per item
// of the company's orders, keyed "order item EBELN/EBELP". A
// document whose kind or quantities cannot be told is refused, as the
// quality gate refuses it.
func (rc *reconciler) sapInvoiced(cc string, orders map[string]bool) map[string]*big.Rat {
	_, issues := postedDocuments(rc.ext, cc)
	for _, i := range issues {
		rc.refuse(i.Table, i.Key, "%s", i.Message)
	}
	// Every company's documents, on the company's orders: the platform's
	// side counts what was taken over on the orders, whichever document.
	out := map[string]*big.Rat{}
	for key, q := range invoicedNet(rc.ext) {
		if order, _, _ := strings.Cut(key, "/"); orders[order] {
			out["order item "+key] = q
		}
	}
	return out
}

func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

func unionKeys[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// extend is an item's net value: quantity times price per price unit,
// rounded half away from zero (procurement's rule), from the quantity's
// decimal string, which has up to six decimals. Only lineValue calls it,
// having refused quantities that are not plain positive decimals, prices
// that do not parse and price units other than empty (1) or a positive
// number, so its only error is money.ErrOverflow.
func extend(qty, price, priceUnit string, t target, currency string) (money.Minor, error) {
	p, _ := t.amount(price, currency) // checked by lineValue
	unit, err := strconv.ParseInt(priceUnit, 10, 64)
	if err != nil || unit < 1 {
		unit = 1
	}
	return money.Scale(p, qty, unit)
}

func diffs(cc string, sapM, platformM map[string]money.Minor, format func(money.Minor) string) []Diff {
	keys := map[string]bool{}
	for k := range sapM {
		keys[k] = true
	}
	for k := range platformM {
		keys[k] = true
	}
	var out []Diff
	for _, k := range sortedKeys(keys) {
		if sapM[k] != platformM[k] {
			out = append(out, Diff{CompanyCode: cc, Key: k, SAP: format(sapM[k]), Platform: format(platformM[k])})
		}
	}
	return out
}
