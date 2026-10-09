package changes

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestAttachEvidenceUnblocksApproval(t *testing.T) {
	repo := testkit.Repo(t)
	store := testkit.Store(t)
	suite, err := regress.Capture(store, testkit.Config(t), 1, 0, testkit.Now)
	if err != nil {
		t.Fatal(err)
	}
	c := testkit.Config(t)
	c.Company("1000").AutoPostLimit = "100000.00"
	cr, err := repo.Propose("agent:configuration", "Raise auto-post limit to 100,000", "fewer manual approvals", testkit.YAML(t, c))
	if err != nil || cr.Status != config.CRAwaitingEvidence {
		t.Fatalf("propose: %v %+v", err, cr)
	}
	cr, err = AttachEvidence(repo, cr.ID, store, suite, "", "", testkit.Now)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Status != config.CRPendingApproval || len(cr.Evidence) != 2 {
		t.Fatalf("status %s, evidence %d", cr.Status, len(cr.Evidence))
	}
	for _, e := range cr.Evidence {
		if e.Passed || !strings.Contains(e.Summary, "change") {
			t.Errorf("%s evidence should report the changed outcome: %+v", e.Kind, e)
		}
	}
}

func TestAttachEvidenceRefusals(t *testing.T) {
	repo := testkit.Repo(t)
	store := testkit.Store(t)
	stale := testkit.Config(t)
	stale.Company("1000").InvoiceTolerance.PricePercent = 15
	staleSuite, _ := regress.Capture(store, stale, 0, 0, testkit.Now)

	c := testkit.Config(t)
	c.Company("1000").AutoPostLimit = "100000.00"
	cr, _ := repo.Propose("agent:configuration", "limit", "", testkit.YAML(t, c))
	if _, err := AttachEvidence(repo, cr.ID, store, staleSuite, "", "", testkit.Now); err == nil || !strings.Contains(err.Error(), "capture a new suite") {
		t.Fatalf("stale suite must be refused: %v", err)
	}
	if _, err := AttachEvidence(repo, "CR-9999", store, staleSuite, "", "", testkit.Now); err == nil {
		t.Fatal("missing change request")
	}
	bad := testkit.Config(t)
	bad.Company("1000").Currency = "dollars"
	inv, _ := repo.Propose("agent:configuration", "bad", "", testkit.YAML(t, bad))
	if _, err := AttachEvidence(repo, inv.ID, store, staleSuite, "", "", testkit.Now); err == nil {
		t.Fatal("invalid change request must not be simulated")
	}
	broken := *staleSuite
	broken.Snapshot = []byte("{")
	if _, err := AttachEvidence(repo, cr.ID, store, &broken, "", "", testkit.Now); err == nil {
		t.Fatal("broken suite must error")
	}
	empty, _ := config.OpenRepo(t.TempDir(), nil)
	if _, err := AttachEvidence(empty, cr.ID, store, staleSuite, "", "", testkit.Now); err == nil {
		t.Fatal("repository without the change request")
	}
}
