package integration

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
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

// Diff is a difference behind a failed check, in minor units.
type Diff struct {
	CompanyCode string `json:"company_code"`
	Key         string `json:"key"` // account number, or "vendor <LIFNR>"
	SAP         string `json:"sap"`
	Platform    string `json:"platform"`
}

// Reconciliation is the reconciliation part of a run's report. Issues are
// SAP values it could not use: amounts that do not parse, or sums out of
// the range of an amount. A reconciliation run extracts again without the
// quality gate, so it reports them as the gate does, and fails.
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
	rc := &reconciler{e: e, ext: ext, t: t, r: r, vendorOf: map[uuid.UUID]string{}, recon: map[string]bool{}}
	for key, id := range r {
		if lifnr, ok := strings.CutPrefix(key, "LFA1/"); ok {
			rc.vendorOf[id] = sap.Alpha(lifnr)
		}
	}
	for _, a := range ext.SKB1 {
		rc.recon[a.BUKRS+"/"+a.SAKNR] = a.MITKZ == "K"
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
	sapOpen, err := rc.trialBalance(ctx, s)
	if err != nil {
		return err
	}
	if err := rc.openPayables(ctx, s, sapOpen); err != nil {
		return err
	}
	if err := rc.vendors(ctx, s); err != nil {
		return err
	}
	if err := rc.purchaseOrders(ctx, s); err != nil {
		return err
	}
	return rc.parkedInvoices(ctx, s)
}

// sapJournal is the SAP side of the journal controls of a company code.
type sapJournal struct {
	tb, open  map[string]money.Minor // per account; per "vendor <LIFNR>"
	debits    money.Minor
	documents map[string]bool
}

// journal adds up the company's journal lines from the extract: per
// account, debits, and open items per vendor. The load's quality gate
// refused amounts and sums out of range, but a reconciliation run extracts
// again without it, so they are checked here too.
func (rc *reconciler) journal(s companyScope) sapJournal {
	tb, open := map[string][]money.Minor{}, map[string][]money.Minor{}
	var debits []money.Minor
	documents := map[string]bool{}
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
			open[vendor] = append(open[vendor], amt)
		}
	}
	return sapJournal{
		tb:        rc.totals("ACDOCA", s.cc, tb, func(a string) string { return "the lines of account " + a }),
		open:      rc.totals("ACDOCA", s.cc, open, func(v string) string { return "the open items of " + v }),
		debits:    rc.total("ACDOCA", s.cc, "the debits of company "+s.cc, debits),
		documents: documents,
	}
}

// trialBalance compares the trial balance per account, and debits when
// every document came over. It returns SAP's open payables per vendor,
// which come out of the same pass over the journal.
func (rc *reconciler) trialBalance(ctx context.Context, s companyScope) (map[string]money.Minor, error) {
	j := rc.journal(s)
	sapTB, sapOpen, sapDebits, documents := j.tb, j.open, j.debits, j.documents
	totals, err := rc.e.Reports.Totals(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return nil, err
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
	return sapOpen, nil
}

// openPayables compares open payables per vendor.
func (rc *reconciler) openPayables(ctx context.Context, s companyScope, sapOpen map[string]money.Minor) error {
	open, err := rc.e.Reports.OpenPayables(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	byVendor := map[string][]money.Minor{}
	for id, amt := range open {
		key := "partner " + id.String()
		if lifnr, ok := rc.vendorOf[id]; ok {
			key = "vendor " + lifnr
		}
		byVendor[key] = append(byVendor[key], amt)
	}
	platformOpen := rc.totals("platform", s.cc, byVendor, func(v string) string { return "the platform's open items of " + v })
	ap := diffs(s.cc, sapOpen, platformOpen, s.format)
	rc.rec.Diffs = append(rc.rec.Diffs, ap...)
	sapTotal := rc.total("ACDOCA", s.cc, "the open payables of company "+s.cc, slices.Collect(maps.Values(sapOpen)))
	platformTotal := rc.total("platform", s.cc, "the platform's open payables of company "+s.cc, slices.Collect(maps.Values(platformOpen)))
	rc.add(s.cc, "Open payables total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, "Open payables: vendors with differences", "0", strconv.Itoa(len(ap)), len(ap) == 0)
	return nil
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

// parkedInvoices compares parked supplier invoices (posted ones are
// carried by their journal entries).
func (rc *reconciler) parkedInvoices(ctx context.Context, s companyScope) error {
	dups := duplicateInvoices(rc.ext)
	sapParked := 0
	var invoices []uuid.UUID
	for _, h := range rc.ext.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		if h.BUKRS != s.cc || h.RBSTAT != "A" || dups[key] != "" {
			continue
		}
		sapParked++
		if id, ok := rc.r["RBKP/"+key]; ok {
			invoices = append(invoices, id)
		}
	}
	parked, err := rc.e.InvoiceCounts.Count(ctx, invoices)
	if err != nil {
		return err
	}
	rc.add(s.cc, "Parked supplier invoices", strconv.Itoa(sapParked), strconv.Itoa(parked), sapParked == parked)
	return nil
}

// extend is an item's net value: quantity times price per price unit,
// rounded half away from zero (procurement's rule). Only lineValue calls
// it, having refused prices that do not parse and price units other than
// empty (1) or a positive number; a quantity can still be positive yet
// finer than a Qty holds.
func extend(qty, price, priceUnit string, t target, currency string) (money.Minor, error) {
	q, err := money.ParseQty(qty)
	if err != nil {
		return 0, fmt.Errorf("quantity %q: %w", qty, err)
	}
	p, _ := t.amount(price, currency) // checked by lineValue
	unit, err := strconv.ParseInt(priceUnit, 10, 64)
	if err != nil || unit < 1 {
		unit = 1
	}
	return money.Extend(q, p, unit)
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
