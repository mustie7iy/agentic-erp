package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// refusingBlocks is the partner port of a loader test: it refuses every
// posting block.
type refusingBlocks struct{ Partners }

func (refusingBlocks) Block(context.Context, uuid.UUID, string) error {
	return problem.Conflict("partner.invalid_status", "The business partner is blocked; block requires status active.")
}

// TestRefusedBlockNamesTheVendor: a posting block the partner context
// refuses is reported under the SAP vendor's number, which the reader of
// the report knows, not the partner the load created.
func TestRefusedBlockNamesTheVendor(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) }) // nothing was written: the test only opens savepoints
	l := &loader{e: &Endpoints{Partners: refusingBlocks{}}, tx: tx, blocks: []block{{lifnr: "0000000007", partner: uuid.New()}}}
	if err := l.blocked(ctx); err != nil {
		t.Fatalf("blocked: got %v, want the refusal reported", err)
	}
	if len(l.issues) != 1 {
		t.Fatalf("issues: got %v, want 1", l.issues)
	}
	if got := l.issues[0].Table + " " + l.issues[0].Key; got != "LFA1 0000000007" {
		t.Errorf("refusal: got %s, want LFA1 0000000007", got)
	}
	if l.counts.SuppliersBlocked != 0 {
		t.Errorf("suppliers blocked: got %d, want 0", l.counts.SuppliersBlocked)
	}
}

// failingSuppliers is the supplier port of a reconciler test: it fails
// every read.
type failingSuppliers struct {
	Suppliers
	err error
}

func (f failingSuppliers) Supplier(context.Context, uuid.UUID, uuid.UUID) (partner.Supplier, bool, error) {
	return partner.Supplier{}, false, f.err
}

// TestVendorKeyFails: the open items of a partner that is no migrated
// vendor are named by the supplier's name, so a failed read of it fails
// the reconciliation rather than leaving the name out.
func TestVendorKeyFails(t *testing.T) {
	down := errors.New("down")
	rc := &reconciler{e: &Endpoints{Suppliers: failingSuppliers{err: down}}, vendorOf: map[uuid.UUID]string{}}
	err := rc.openItems(context.Background(), companyScope{cc: "2000"}, "Open payables", "", nil, map[uuid.UUID]money.Minor{uuid.New(): 100})
	if !errors.Is(err, down) {
		t.Errorf("open items: got %v, want the failed read", err)
	}
}

// itemCounts is the reports port of a reconciler test: it reports open
// payable items of one partner.
type itemCounts struct {
	Reports
	partner uuid.UUID
}

func (r itemCounts) OpenPayableItems(context.Context, uuid.UUID, int16) (map[uuid.UUID]int, error) {
	return map[uuid.UUID]int{r.partner: 1}, nil
}

// TestOpenItemCountVendorKeyFails: open payable items are counted per
// vendor, so a failed read of a non-migrated supplier's name fails the
// reconciliation as it does for the open payables.
func TestOpenItemCountVendorKeyFails(t *testing.T) {
	down := errors.New("down")
	rc := &reconciler{e: &Endpoints{Suppliers: failingSuppliers{err: down}, Reports: itemCounts{partner: uuid.New()}}, vendorOf: map[uuid.UUID]string{}}
	if err := rc.openPayableItems(context.Background(), companyScope{cc: "2000"}, nil); !errors.Is(err, down) {
		t.Errorf("open payable items: got %v, want the failed read", err)
	}
}

// foreignItems is the reports port of a reconciler test: it reports the
// open items of one partner in USD.
type foreignItems struct {
	Reports
	partner uuid.UUID
	sum     money.Minor
}

func (r foreignItems) OpenForeignItems(context.Context, uuid.UUID, int16) (map[uuid.UUID]map[string]money.Minor, error) {
	return map[uuid.UUID]map[string]money.Minor{r.partner: {"USD": r.sum}}, nil
}

// TestForeignItemsVendorKeyFails: the open items in a foreign currency of
// a partner that is no migrated vendor are named by the supplier's name,
// so a failed read of it fails the reconciliation.
func TestForeignItemsVendorKeyFails(t *testing.T) {
	down := errors.New("down")
	rc := &reconciler{e: &Endpoints{Suppliers: failingSuppliers{err: down}, Reports: foreignItems{partner: uuid.New(), sum: -100}},
		ext: &sap.Extract{}, vendorOf: map[uuid.UUID]string{}}
	if err := rc.foreignItems(context.Background(), companyScope{cc: "2000", cur: "EUR"}); !errors.Is(err, down) {
		t.Errorf("open items in USD: got %v, want the failed read", err)
	}
}

// TestForeignItemsDiffer: a vendor's open USD items that differ are listed
// in USD under "USD items of vendor <LIFNR>" (without its leading zeros) and fail both checks, and a
// document amount that does not parse is refused.
func TestForeignItemsDiffer(t *testing.T) {
	vendor := uuid.New()
	line := func(docln, wsl string) sap.ACDOCA {
		return sap.ACDOCA{RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000011", DOCLN: docln, RACCT: "0000211000", LIFNR: "0000000001", RWCUR: "USD", WSL: wsl}
	}
	rc := &reconciler{e: &Endpoints{Reports: foreignItems{partner: vendor, sum: -9000}},
		ext: &sap.Extract{ACDOCA: []sap.ACDOCA{line("000001", "-100.00"), line("000002", "x")}}, t: target{exponents: map[string]int{"USD": 2, "EUR": 2}},
		vendorOf: map[uuid.UUID]string{vendor: "1"}, recon: map[string]bool{"2000/0000211000": true}}
	if err := rc.foreignItems(context.Background(), companyScope{cc: "2000", cur: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(rc.rec.Diffs), "[{2000 USD items of vendor 1 -100.00 USD -90.00 USD}]"; got != want {
		t.Errorf("differences: got %s, want %s", got, want)
	}
	for i, want := range []string{"Open items in USD total -100.00 USD -90.00 USD false", "Open items in USD: vendors with differences 0 1 false"} {
		if c := rc.rec.Checks[i]; fmt.Sprint(c.Name, " ", c.SAP, " ", c.Platform, " ", c.Pass) != want {
			t.Errorf("check %d: got %v, want %s", i, c, want)
		}
	}
	if len(rc.rec.Issues) != 1 || rc.rec.Issues[0].Key != "2000/2026/0100000011/000002" {
		t.Errorf("issues: got %v, want the document amount that does not parse refused", rc.rec.Issues)
	}
}
