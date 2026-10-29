package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
