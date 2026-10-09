package regress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/simulate"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestCaptureCoversEveryBehavior(t *testing.T) {
	s := testkit.Store(t)
	suite, err := Capture(s, testkit.Config(t), 1, 50, testkit.Now)
	if err != nil {
		t.Fatal(err)
	}
	strata := strings.Join(suite.Strata(), "|")
	for _, want := range []string{"decision auto_post", "decision review", "decision exception", "issue price_variance",
		"issue quantity_exceeds_received", "issue possible_duplicate", "issue tax_or_total_mismatch", "near the auto-post limit", "sample"} {
		if !strings.Contains(strata, want) {
			t.Errorf("suite misses stratum %q (has %s)", want, strata)
		}
	}
	if len(suite.Cases) != 16 || suite.ConfigVersion != 1 || suite.PostingDate != "2026-10-09" {
		t.Fatalf("suite: %d cases, version %d", len(suite.Cases), suite.ConfigVersion)
	}
	small, _ := Capture(s, testkit.Config(t), 1, 3, testkit.Now)
	if len(small.Cases) != 3 {
		t.Fatalf("max cases not honored: %d", len(small.Cases))
	}
}

func TestRunDetectsChangedOutcomes(t *testing.T) {
	s := testkit.Store(t)
	suite, _ := Capture(s, testkit.Config(t), 1, 0, testkit.Now)
	same, err := suite.Run(testkit.Config(t))
	if err != nil || !same.Passed() || !strings.Contains(same.Summary(), "all 16 cases unchanged") || !same.Evidence().Passed {
		t.Fatalf("baseline run: %v %+v", err, same)
	}
	changed := testkit.Config(t)
	changed.Company("1000").InvoiceTolerance.PricePercent = 15
	res, err := suite.Run(changed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed() || len(res.Failed) == 0 || res.Evidence().Passed || !strings.Contains(res.Summary(), "price_variance") {
		t.Fatalf("a 15%% tolerance changes the price-variance case: %+v", res)
	}
	// Changing the live store after capture does not affect the suite: it runs on its snapshot.
	for _, inv := range s.Invoices {
		inv.Status = core.InvoiceRejected
	}
	again, _ := suite.Run(testkit.Config(t))
	if !again.Passed() {
		t.Fatal("suite must replay its own snapshot, not the live store")
	}
}

func TestSaveLoadLatest(t *testing.T) {
	s := testkit.Store(t)
	dir := t.TempDir()
	if _, err := Latest(dir); err == nil {
		t.Fatal("empty dir has no suite")
	}
	a, _ := Capture(s, testkit.Config(t), 1, 5, testkit.Now)
	b, _ := Capture(s, testkit.Config(t), 2, 5, testkit.Now.Add(time.Hour))
	if _, err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Save(dir); err != nil {
		t.Fatal(err)
	}
	latest, err := Latest(dir)
	if err != nil || latest.ID != b.ID || latest.ConfigVersion != 2 {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	os.WriteFile(filepath.Join(dir, "RS-zzz.json"), []byte("{"), 0o644)
	if _, err := Latest(dir); err == nil {
		t.Fatal("corrupt suite must error")
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing suite must error")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocked, nil, 0o644)
	if _, err := a.Save(filepath.Join(blocked, "sub")); err == nil {
		t.Fatal("unwritable dir must error")
	}
}

func TestErrors(t *testing.T) {
	if _, err := Capture(core.NewStore(), testkit.Config(t), 1, 5, testkit.Now); err == nil {
		t.Fatal("no invoices, no suite")
	}
	bad := testkit.Config(t)
	bad.Companies = bad.Companies[:1]
	if _, err := Capture(testkit.Store(t), bad, 1, 5, testkit.Now); err == nil {
		t.Fatal("configuration errors surface")
	}
	s := testkit.Store(t)
	suite, _ := Capture(s, testkit.Config(t), 1, 5, testkit.Now)
	if _, err := suite.Run(bad); err == nil {
		t.Fatal("configuration errors surface on run")
	}
	broken := *suite
	broken.Snapshot = []byte("{")
	if _, err := broken.Run(testkit.Config(t)); err == nil {
		t.Fatal("corrupt snapshot must error")
	}
	ghost := *suite
	ghost.Cases = append([]Case{{InvoiceID: "GHOST", Expected: simulate.Outcome{}}}, suite.Cases...)
	if _, err := ghost.Run(testkit.Config(t)); err == nil {
		t.Fatal("case missing from snapshot must error")
	}
	if near(100, 0) || !near(90, 100) || near(70, 100) {
		t.Fatal("near")
	}
}
