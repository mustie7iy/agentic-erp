package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// A migrated item's fixed payment method (ADR-0038 As built, a migrated
// item's fixed payment method; product owner 2026-11-10). An SAP item can
// fix its payment method (BSEG/ACDOCA-ZLSCH), which wins over the payment
// run's order and over the vendor's allowed methods (LFB1-ZWELS; the SAP
// specialist, po:2026-11-09, A4(c) [M]). The load keeps it with the
// vendor's open item, in either mode, through the ledger's posting
// (ledger.PostingLine.PaymentMethod), so a payment run pays the item by
// that method alone, as it pays an invoice entered here that fixes one.
// SAP's one-character method is the company's method of the same code, as
// a vendor's methods are (vendorMethods): the configuration's methods are
// drafted from T042E's. Only an open ordinary item takes one: a cleared
// item is never paid again, and no run pays a down payment (ADR-0037).
// That the vendor's own list holds the method is not asked: SAP pays the
// item by it anyway, and so does the platform (po:2026-11-10).

// fixedMethod is the payment method a vendor line fixes that the load
// takes over: its ZLSCH when SAP has not cleared it and it is no special
// G/L item, else "". Whether the line is a vendor's is the caller's to
// tell.
func fixedMethod(l sap.ACDOCA) string {
	if l.AUGBL != "" || l.UMSKZ != "" {
		return ""
	}
	return strings.TrimSpace(l.ZLSCH)
}

// fixedMethods checks the payment methods SAP's open vendor items fix
// (ZLSCH): each a payment method of the company's configuration, as the
// gate asks of a vendor's methods (vendorMethods) and payables asks of an
// invoice entered here, so the run is refused rather than the item loaded
// without it and paid another way. A method on an open special G/L item
// (a down payment) is not taken over, which is reported per company.
func (q *qualityCheck) fixedMethods() {
	special := map[string]int{}
	for _, l := range q.e.ACDOCA {
		m := strings.TrimSpace(l.ZLSCH)
		if m == "" || l.AUGBL != "" || !q.t.supplierLine(l) {
			continue
		}
		switch {
		case l.UMSKZ != "":
			special[l.RBUKRS]++
		case !q.t.methods[l.RBUKRS][m]:
			q.add(SevError, "ACDOCA", documentKey(l)+"/"+l.DOCLN, "payment method %q the open item of vendor %s fixes (ZLSCH) is not a payment method of "+
				"company %s in the configuration", m, l.LIFNR, l.RBUKRS)
		}
	}
	for _, cc := range sortedKeys(special) {
		q.add(SevWarning, "ACDOCA", cc, "open special G/L items of company %s that name a payment method (ZLSCH): %d. The method is not taken over: "+
			"no payment run pays a down payment", cc, special[cc])
	}
}

// fixMethod gives a vendor line's posting the payment method its item
// fixes, if any.
func (l *loader) fixMethod(pl *ledger.PostingLine, line sap.ACDOCA) {
	if m := fixedMethod(line); m != "" && l.reconciliation(line.RBUKRS, line.RACCT) {
		pl.PaymentMethod = &m
	}
}

// fixedItems are the open items of a vendor that fix one payment method:
// how many, and their amounts in company currency.
type fixedItems struct {
	count int
	terms []money.Minor
}

// fixedByMethod are open vendor items that fix a payment method, by
// method and vendor key ("vendor <LIFNR>", or a partner that is none).
type fixedByMethod map[string]map[string]fixedItems

func (f fixedByMethod) add(method, vendor string, n int, amt money.Minor) {
	if f[method] == nil {
		f[method] = map[string]fixedItems{}
	}
	items := f[method][vendor]
	f[method][vendor] = fixedItems{count: items.count + n, terms: append(items.terms, amt)}
}

// addLine counts an open ordinary vendor item of an amount, if it fixes
// a method; the load leaves out a line of amount 0.
func (f fixedByMethod) addLine(l sap.ACDOCA, vendor string, amt money.Minor) {
	if m := fixedMethod(l); m != "" && amt != 0 {
		f.add(m, vendor, 1, amt)
	}
}

// fixedMethods compares the open payable items that fix a payment method
// (SAP ZLSCH), per method and vendor: their number and their total in
// company currency. The open payables agree whether or not an item's
// method came over; these show an item loaded without it, which a run
// would pay by the company's run order, or with another. Shown when
// either side has any, so a scope without them reports as before.
func (rc *reconciler) fixedMethods(ctx context.Context, s companyScope, fixed fixedByMethod) error {
	open, err := rc.e.Reports.OpenItemsByMethod(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	platform := fixedByMethod{}
	for id, methods := range open {
		key, err := rc.vendorKey(ctx, s, id)
		if err != nil {
			return err
		}
		for m, items := range methods {
			platform.add(m, key, items.Count, items.LC)
		}
	}
	for _, m := range sortedKeys(unionKeys(fixed, platform)) {
		rc.fixedMethod(s, m, fixed[m], platform[m])
	}
	return nil
}

// fixedMethod is fixedMethods for one payment method.
func (rc *reconciler) fixedMethod(s companyScope, method string, sapItems, platformItems map[string]fixedItems) {
	what := "open items fixing payment method " + method
	value := func(n int, total money.Minor) string { return strconv.Itoa(n) + " items, " + s.format(total) }
	var sapCount, platformCount, differ int
	var sapTerms, platformTerms []money.Minor
	for _, vendor := range sortedKeys(unionKeys(sapItems, platformItems)) {
		in, on := sapItems[vendor], platformItems[vendor]
		inTotal := rc.total("ACDOCA", s.cc+"/"+vendor, fmt.Sprintf("the %s of %s", what, vendor), in.terms)
		onTotal := rc.total("platform", s.cc+"/"+vendor, fmt.Sprintf("the platform's %s of %s", what, vendor), on.terms)
		sapCount, platformCount = sapCount+in.count, platformCount+on.count
		sapTerms, platformTerms = append(sapTerms, inTotal), append(platformTerms, onTotal)
		if in.count != on.count || inTotal != onTotal {
			differ++
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: s.cc, Key: "payment method " + method + " items of " + vendor,
				SAP: value(in.count, inTotal), Platform: value(on.count, onTotal)})
		}
	}
	sapTotal := rc.total("ACDOCA", s.cc, "the "+what+" of company "+s.cc, sapTerms)
	platformTotal := rc.total("platform", s.cc, "the platform's "+what+" of company "+s.cc, platformTerms)
	name := "Open items fixing payment method " + method
	rc.add(s.cc, name, strconv.Itoa(sapCount), strconv.Itoa(platformCount), sapCount == platformCount)
	rc.add(s.cc, name+" total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, name+": vendors with differences", "0", strconv.Itoa(differ), differ == 0)
}
