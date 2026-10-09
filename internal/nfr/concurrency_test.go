package nfr

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/p2p"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// These tests are meant for `go test -race`: they run real operations on a
// shared store and repository from many goroutines.

func p2pService(t *testing.T) (*p2p.Service, *config.Config) {
	t.Helper()
	cfg := testkit.Config(t)
	audit, _ := governance.NewAuditLog("")
	return &p2p.Service{Store: testkit.Store(t), Config: func() (*config.Config, error) { return cfg, nil }, Audit: audit,
		Now: func() time.Time { return testkit.Now }}, cfg
}

// Requisitions, approvals, invoice matching and analytics all at once.
func TestConcurrentMixedWorkload(t *testing.T) {
	svc, cfg := p2pService(t)
	inv := agent.NewService(svc.Store, svc.Audit)
	inv.Config = cfg
	m, _ := analytics.DefaultModel()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := svc.CreateRequisition("buyer1", p2p.RequisitionInput{CompanyCode: "1000", VendorID: "100001",
				Lines: []core.RequisitionLine{{Description: fmt.Sprintf("item %d", i), Quantity: 1_000, UnitPrice: 1_000, GLAccount: "520000"}}})
			if err != nil {
				errs <- err
				return
			}
			if _, err := svc.DecideRequisition("purchmgr1", req.ID, true, ""); err != nil {
				errs <- err
				return
			}
			if _, _, err := svc.CreatePurchaseOrder("buyer1", req.ID, ""); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Add(2)
	go func() { defer wg.Done(); agent.ProcessInvoices(context.Background(), inv, nil, io.Discard) }()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			f, err := analytics.BuildFacts(svc.Store, m, time.Now())
			if err != nil {
				errs <- err
				return
			}
			analytics.Execute(m, f, analytics.Principal{ID: "u", Companies: []string{"1000"}}, analytics.Query{Measures: []string{"revenue"}, By: []string{"fiscal_month"}})
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	ordered := 0
	for _, r := range svc.Store.Requisitions {
		if r.Status == core.ReqOrdered {
			ordered++
		}
	}
	if ordered != 20 {
		t.Fatalf("want 20 ordered requisitions, got %d", ordered)
	}
	if err := svc.Audit.Verify(); err != nil {
		t.Fatalf("audit chain under concurrency: %v", err)
	}
	ids := map[string]bool{}
	for id := range svc.Store.PurchaseOrders {
		if ids[id] {
			t.Fatal("duplicate id")
		}
		ids[id] = true
	}
}

// Ten treasury clerks propose at once: open items go into exactly one run.
func TestConcurrentPaymentProposalsNeverDoublePay(t *testing.T) {
	svc, _ := p2pService(t)
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	runs := make(chan *core.PaymentRun, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if run, err := svc.ProposePaymentRun("treas1", "1000", "2027-06-30"); err == nil {
				succeeded.Add(1)
				runs <- run
			}
		}()
	}
	wg.Wait()
	close(runs)
	if succeeded.Load() != 1 {
		t.Fatalf("exactly one proposal can claim the open items, got %d", succeeded.Load())
	}
	run := <-runs
	// Two releasers act at the same moment: the run is posted once.
	var released atomic.Int32
	cfg2, _ := svc.Config()
	cfg2.Users = append(cfg2.Users, config.User{ID: "treasmgr2", Name: "Second", Roles: []string{"Z_TREASURY_MGR"}})
	for _, u := range []string{"treasmgr1", "treasmgr2"} {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if _, err := svc.DecidePaymentRun(u, run.ID, true, ""); err == nil {
				released.Add(1)
			}
		}(u)
	}
	wg.Wait()
	if released.Load() != 1 {
		t.Fatalf("a run is released once, got %d", released.Load())
	}
	payments := 0
	for _, je := range svc.Store.Journal {
		if je.DocumentType == "ZP" {
			payments++
		}
	}
	if payments != len(run.Payments) {
		t.Fatalf("want %d payment entries, got %d", len(run.Payments), payments)
	}
}

// Many proposals and approvals against one configuration repository.
func TestConcurrentConfigurationChanges(t *testing.T) {
	repo := testkit.Repo(t)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := testkit.Config(t)
			c.PaymentTerms[0].NetDays = 15 + i
			cr, err := repo.Propose("analyst1", fmt.Sprintf("terms %d", i), "", testkit.YAML(t, c))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- cr.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate change request id %s", id)
		}
		seen[id] = true
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			repo.Approve(id, "owner1", "")
		}(id)
	}
	wg.Wait()
	if len(seen) != 20 || len(repo.ChangeRequests()) != 20 {
		t.Fatalf("want 20 change requests, got %d", len(seen))
	}
	// Activating them all concurrently: one wins, the rest are stale.
	var activated atomic.Int32
	for id := range seen {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := repo.Activate(id, "architect1"); err == nil {
				activated.Add(1)
			}
		}(id)
	}
	wg.Wait()
	if activated.Load() != 1 || len(repo.Versions()) != 2 {
		t.Fatalf("one activation wins, got %d (versions %d)", activated.Load(), len(repo.Versions()))
	}
	_ = money.Minor(0)
}
