package changes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// repoAt bootstraps a repository in a known directory so tests can tamper with it.
func repoAt(t *testing.T, cfg *config.Config) (*config.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := config.OpenRepo(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, fs, err := r.Bootstrap(testkit.YAML(t, cfg), "user:architect1"); err != nil {
		t.Fatalf("%v %v", err, fs)
	}
	return r, dir
}

func termsChange(t *testing.T, base *config.Config) []byte {
	t.Helper()
	c := *base
	c.PaymentTerms = append([]config.PaymentTerm(nil), base.PaymentTerms...)
	c.PaymentTerms[0].NetDays = 21
	return testkit.YAML(t, &c)
}

func TestAttachEvidenceTamperedRepository(t *testing.T) {
	store := testkit.Store(t)
	suite, _ := regress.Capture(store, testkit.Config(t), 1, 0, testkit.Now)

	// The active version file was edited outside the change process.
	r, dir := repoAt(t, testkit.Config(t))
	cr, _ := r.Propose("analyst1", "terms", "", termsChange(t, testkit.Config(t)))
	os.WriteFile(filepath.Join(dir, "versions", "v0001.yaml"), []byte("version: 1\n"), 0o644)
	if _, err := AttachEvidence(r, cr.ID, store, suite, "", "", testkit.Now); err == nil || !strings.Contains(err.Error(), "modified outside") {
		t.Fatalf("tampered active version: %v", err)
	}

	// The stored proposal was corrupted.
	r2, dir2 := repoAt(t, testkit.Config(t))
	cr2, _ := r2.Propose("analyst1", "terms", "", termsChange(t, testkit.Config(t)))
	state := filepath.Join(dir2, "state.json")
	b, _ := os.ReadFile(state)
	i := strings.Index(string(b), `"proposed": "`)
	corrupt := string(b[:i]) + `"proposed": "version: [",` + string(b[strings.Index(string(b[i:]), `",`)+i+2:])
	os.WriteFile(state, []byte(corrupt), 0o644)
	r2, err := config.OpenRepo(dir2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AttachEvidence(r2, cr2.ID, store, suite, "", "", testkit.Now); err == nil {
		t.Fatal("corrupt proposal must not be simulated")
	}
}

func TestAttachEvidenceReplayAndSuiteFailures(t *testing.T) {
	store := testkit.Store(t)
	full := testkit.Config(t)
	suite, _ := regress.Capture(store, full, 1, 0, testkit.Now)

	// The active configuration does not cover a company the data has.
	partial := testkit.Config(t)
	partial.Companies = partial.Companies[:1]
	r, _ := repoAt(t, partial)
	cr, _ := r.Propose("analyst1", "terms", "", termsChange(t, partial))
	if _, err := AttachEvidence(r, cr.ID, store, suite, "", "", testkit.Now); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("replay failure: %v", err)
	}

	// The proposal drops a company the suite covers; no invoices fall in the replay window.
	r2, _ := repoAt(t, full)
	cr2, err := r2.Propose("analyst1", "drop 2000", "", testkit.YAML(t, partial))
	if err != nil || cr2.Status == config.CRInvalid {
		t.Fatalf("propose: %v %+v", err, cr2)
	}
	if _, err := AttachEvidence(r2, cr2.ID, store, suite, "2030-01-01", "", testkit.Now); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("suite failure under the proposal: %v", err)
	}

	// A rejected change cannot receive evidence.
	r3, _ := repoAt(t, full)
	cr3, _ := r3.Propose("analyst1", "terms", "", termsChange(t, full))
	r3.Reject(cr3.ID, "owner1", "no")
	if _, err := AttachEvidence(r3, cr3.ID, store, suite, "", "", testkit.Now); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejected change: %v", err)
	}
}
