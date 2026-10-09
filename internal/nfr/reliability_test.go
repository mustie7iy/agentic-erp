package nfr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/delivery/simulate"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/p2p"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// A crash during save leaves the previous snapshot intact: writes go to a
// temporary file that is renamed into place.
func TestStoreSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	s := testkit.Store(t)
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: a half-written temp file next to the snapshot.
	os.WriteFile(path+".tmp", []byte(`{"seq": 1, "compan`), 0o644)
	loaded, err := core.LoadStore(path)
	if err != nil || len(loaded.Journal) != len(s.Journal) {
		t.Fatalf("snapshot must survive a crashed write: %v", err)
	}
	// A save that cannot complete reports an error and keeps the old file.
	os.Mkdir(filepath.Join(dir, "blocked.json.tmp"), 0o755)
	if err := s.Save(filepath.Join(dir, "blocked.json")); err == nil {
		t.Fatal("failed save must report an error")
	}
	if _, err := core.LoadStore(path); err != nil {
		t.Fatal("original snapshot damaged")
	}
	os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not json"), 0o644)
	if _, err := core.LoadStore(filepath.Join(dir, "corrupt.json")); err == nil {
		t.Fatal("a corrupt snapshot must be an error, not empty data")
	}
}

// If the audit log cannot be written, the action fails loudly instead of
// happening unrecorded.
func TestAuditFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	audit, _ := governance.NewAuditLog(filepath.Join(dir, "logs", "audit.jsonl"))
	cfg := testkit.Config(t)
	svc := &p2p.Service{Store: testkit.Store(t), Config: func() (*config.Config, error) { return cfg, nil }, Audit: audit}
	os.WriteFile(filepath.Join(dir, "logs"), []byte("not a directory"), 0o644)
	defer func() {
		if recover() == nil {
			t.Fatal("an unauditable action must not succeed silently")
		}
	}()
	svc.BlockVendor("mdclerk1", "100002", "x")
}

// The same inputs always give the same results.
func TestDeterminism(t *testing.T) {
	s := testkit.Store(t)
	before, after := testkit.Config(t), testkit.Config(t)
	after.Company("1000").AutoPostLimit = "100000.00"
	r1, _ := simulate.Replay(s, before, after, "", "", "2026-10-09")
	r2, _ := simulate.Replay(s, before, after, "", "", "2026-10-09")
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("simulation is not deterministic")
	}
	s1, _ := regress.Capture(s, before, 1, 0, testkit.Now)
	s2, _ := regress.Capture(s, before, 1, 0, testkit.Now)
	b1, _ := json.Marshal(s1.Cases)
	b2, _ := json.Marshal(s2.Cases)
	if string(b1) != string(b2) {
		t.Fatal("suite capture is not deterministic")
	}
	a, b := testkit.Store(t), testkit.Store(t)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("migration is not deterministic")
	}
}

// A store snapshot round-trips every record type created in Phase 1.
func TestSnapshotRoundTripsPhase1Records(t *testing.T) {
	svc, cfg := p2pService(t)
	req, _ := svc.CreateRequisition("buyer1", p2p.RequisitionInput{CompanyCode: "1000", VendorID: "100001", Lines: []core.RequisitionLine{{Description: "x", Quantity: 1000, UnitPrice: 100, GLAccount: "520000"}}})
	svc.DecideRequisition("purchmgr1", req.ID, true, "")
	svc.CreatePurchaseOrder("buyer1", req.ID, "")
	svc.RequestBankChange("mdclerk1", "100001", "GB82WEST12345698765432", "x")
	run, _ := svc.ProposePaymentRun("treas1", "1000", "2027-06-30")
	svc.DecidePaymentRun("treasmgr1", run.ID, true, "")
	inv := agent.NewService(svc.Store, nil)
	inv.Config = cfg
	path := filepath.Join(t.TempDir(), "s.json")
	if err := svc.Store.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Requisitions) != 1 || len(loaded.Workflows) != len(svc.Store.Workflows) || len(loaded.MDChanges) != 1 ||
		len(loaded.PaymentRuns) != 1 || len(loaded.Outputs) != 1 || loaded.PaymentRuns[run.ID].BankFile == "" {
		t.Fatal("phase 1 records lost in the snapshot")
	}
	ja, _ := json.Marshal(svc.Store)
	jb, _ := json.Marshal(loaded)
	if string(ja) != string(jb) {
		t.Fatal("snapshot round trip changed data")
	}
}
