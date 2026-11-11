package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// methodLine is a line of company 2000 on the account, for vendor
// 0000000001, fixing the payment method.
func methodLine(docln, account, hsl, zlsch string) sap.ACDOCA {
	return sap.ACDOCA{RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000030", DOCLN: docln, RACCT: account, HSL: hsl, LIFNR: "0000000001", ZLSCH: zlsch}
}

// TestFixedMethodsGate: the quality gate refuses a payment method an open
// vendor item fixes (ZLSCH) that the company's configuration lacks, as it
// refuses a vendor's; it asks nothing of a cleared item's, which is never
// paid again, or of a line that is no vendor's, and reports the open
// special G/L items naming one, whose method is not taken over.
func TestFixedMethodsGate(t *testing.T) {
	cleared := methodLine("000003", "0000211000", "-30.00", "X")
	cleared.AUGBL = "0100000031"
	special := func(docln string) sap.ACDOCA {
		l := methodLine(docln, "0000159000", "500.00", "X")
		l.UMSKZ = "A"
		return l
	}
	q := &qualityCheck{
		e: &sap.Extract{ACDOCA: []sap.ACDOCA{methodLine("000001", "0000211000", "-60.00", "T"), methodLine("000002", "0000211000", "-25.00", "X"), cleared,
			methodLine("000004", "0000520000", "115.00", "X"), methodLine("000005", "0000211000", "-10.00", " "), special("000006"), special("000007")}},
		t: target{charts: map[string]map[string]ledger.ChartAccount{"2000": {"211000": {ReconciliationKind: "supplier"}, "159000": {ReconciliationKind: "supplier"},
			"520000": {}}}, methods: map[string]map[string]bool{"2000": {"T": true}}},
	}
	q.fixedMethods()
	want := []Issue{
		{Severity: SevError, Table: "ACDOCA", Key: "2000/2026/0100000030/000002",
			Message: `payment method "X" the open item of vendor 0000000001 fixes (ZLSCH) is not a payment method of company 2000 in the configuration`},
		{Severity: SevWarning, Table: "ACDOCA", Key: "2000",
			Message: "open special G/L items of company 2000 that name a payment method (ZLSCH): 2. The method is not taken over: no payment run pays a down payment"},
	}
	if got := fmt.Sprint(q.out); got != fmt.Sprint(want) {
		t.Errorf("issues: got %s, want %s", got, fmt.Sprint(want))
	}
}

// methodItems is the reports port of a reconciler test: it reports one
// partner's open items fixing payment method T.
type methodItems struct {
	Reports
	partner uuid.UUID
	items   ledger.FixedMethodItems
}

func (r methodItems) OpenItemsByMethod(context.Context, uuid.UUID, int16) (map[uuid.UUID]map[string]ledger.FixedMethodItems, error) {
	return map[uuid.UUID]map[string]ledger.FixedMethodItems{r.partner: {"T": r.items}}, nil
}

// failingMethodItems is a reports port whose read of the open items by
// fixed method fails.
type failingMethodItems struct {
	Reports
	err error
}

func (r failingMethodItems) OpenItemsByMethod(context.Context, uuid.UUID, int16) (map[uuid.UUID]map[string]ledger.FixedMethodItems, error) {
	return nil, r.err
}

// TestFixedMethodsReadsFail: a failed read of the platform's open items by
// fixed method, or of the name of a partner that is no migrated vendor,
// fails the reconciliation.
func TestFixedMethodsReadsFail(t *testing.T) {
	down := errors.New("down")
	for name, e := range map[string]*Endpoints{
		"the open items by method":           {Reports: failingMethodItems{err: down}},
		"the name of a non-migrated partner": {Suppliers: failingSuppliers{err: down}, Reports: methodItems{partner: uuid.New()}},
	} {
		t.Run(name, func(t *testing.T) {
			rc := &reconciler{e: e, vendorOf: map[uuid.UUID]string{}}
			if err := rc.fixedMethods(context.Background(), companyScope{cc: "2000"}, fixedByMethod{}); !errors.Is(err, down) {
				t.Errorf("open items fixing a method: got %v, want the failed read", err)
			}
		})
	}
}

// TestFixedMethodsDiffer: per payment method the open items that fix it
// are compared by vendor, in number and total. An item the platform holds
// without the method SAP fixed, or with another, is listed under "payment
// method <method> items of vendor <LIFNR>" and fails the method's checks;
// a line of amount 0, which the load leaves out, a cleared item and a
// special G/L item count on neither side.
func TestFixedMethodsDiffer(t *testing.T) {
	vendor := uuid.New()
	cleared, special := methodLine("000004", "0000211000", "-7.00", "T"), methodLine("000005", "0000211000", "9.00", "T")
	cleared.AUGBL, special.UMSKZ = "0100000031", "A"
	rc := &reconciler{e: &Endpoints{Reports: methodItems{partner: vendor, items: ledger.FixedMethodItems{Count: 1, LC: -6000}}},
		ext: &sap.Extract{ACDOCA: []sap.ACDOCA{methodLine("000001", "0000211000", "-60.00", "T"), methodLine("000002", "0000211000", "-25.00", "T"),
			methodLine("000003", "0000211000", "0.00", "T"), cleared, special, methodLine("000006", "0000211000", "-40.00", "S"),
			methodLine("000007", "0000211000", "-5.00", "")}},
		t: target{exponents: map[string]int{"EUR": 2}}, vendorOf: map[uuid.UUID]string{vendor: "1"}, recon: map[string]bool{"2000/0000211000": true},
		partnerOf: map[string]string{}}
	s := companyScope{cc: "2000", cur: "EUR", format: func(v money.Minor) string { return v.Plain(2) + " EUR" }}
	if err := rc.fixedMethods(context.Background(), s, rc.journal(s).fixed); err != nil {
		t.Fatal(err)
	}
	wantDiffs := "[{2000 payment method S items of vendor 1 1 items, -40.00 EUR 0 items, 0.00 EUR} {2000 payment method T items of vendor 1 2 items, -85.00 EUR 1 items, -60.00 EUR}]"
	if got := fmt.Sprint(rc.rec.Diffs); got != wantDiffs {
		t.Errorf("differences: got %s, want %s", got, wantDiffs)
	}
	var got []string
	for _, c := range rc.rec.Checks {
		got = append(got, fmt.Sprint(c.Name, " ", c.SAP, " ", c.Platform, " ", c.Pass))
	}
	want := []string{
		"Open items fixing payment method S 1 0 false", "Open items fixing payment method S total -40.00 EUR 0.00 EUR false",
		"Open items fixing payment method S: vendors with differences 0 1 false",
		"Open items fixing payment method T 2 1 false", "Open items fixing payment method T total -85.00 EUR -60.00 EUR false",
		"Open items fixing payment method T: vendors with differences 0 1 false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("checks: got %q, want %q", got, want)
	}
}
