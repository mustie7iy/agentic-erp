package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/governance"
)

func TestTierNamesAndDecimals(t *testing.T) {
	if TierCosmetic.String() != "tier 1 (cosmetic)" || TierOperational.String() != "tier 2 (operational)" || TierPostingRelated.String() != "tier 3 (posting-relevant)" {
		t.Fatal("tier names")
	}
	if decimalsFor("JPY") != 0 || decimalsFor("KWD") != 3 || decimalsFor("USD") != 2 {
		t.Fatal("decimals")
	}
	if c := (Change{Path: "p", New: "x"}); !strings.HasPrefix(c.String(), "+") {
		t.Fatal("added change")
	}
	if c := (Change{Path: "p", Old: "a", New: "b"}); !strings.HasPrefix(c.String(), "~") {
		t.Fatal("modified change")
	}
	// Diff against nothing lists every setting as added.
	changes, err := Diff(nil, fixture(t))
	if err != nil || len(changes) == 0 {
		t.Fatal("diff against nil")
	}
}

// repoWithCR bootstraps a repo and files an approved tier-2 change.
func repoWithCR(t *testing.T) (*Repo, string, *ChangeRequest) {
	t.Helper()
	r, dir := newRepo(t)
	c := fixture(t)
	c.PaymentTerms[0].NetDays = 20
	cr, err := r.Propose("analyst1", "terms", "", yamlOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if cr, err = r.Approve(cr.ID, "owner1", ""); err != nil {
		t.Fatal(err)
	}
	return r, dir, cr
}

func TestOpenRepoWithNullChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"active":0,"changes":null}`), 0o644)
	r, err := OpenRepo(dir, nil)
	if err != nil || r.state.Changes == nil {
		t.Fatalf("null changes must become an empty map: %v", err)
	}
}

func TestRepoWithoutAuditLog(t *testing.T) {
	r, err := OpenRepo(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Bootstrap(fixtureYAML(t), "user:architect1"); err != nil {
		t.Fatalf("a repository without an audit log still works: %v", err)
	}
}

func TestAuditFailurePanics(t *testing.T) {
	dir := t.TempDir()
	audit, _ := governance.NewAuditLog(filepath.Join(dir, "logs", "audit.jsonl"))
	r, _ := OpenRepo(filepath.Join(dir, "repo"), audit)
	os.WriteFile(filepath.Join(dir, "logs"), []byte("file, not a directory"), 0o644)
	defer func() {
		if recover() == nil {
			t.Fatal("an unauditable configuration change must not succeed silently")
		}
	}()
	r.Bootstrap(fixtureYAML(t), "user:architect1")
}

func TestVersionFileErrors(t *testing.T) {
	// Missing version file.
	r, dir := newRepo(t)
	os.Remove(filepath.Join(dir, "versions", "v0001.yaml"))
	if _, _, err := r.Active(); err == nil {
		t.Fatal("missing version file must error")
	}
	// Bootstrap cannot create the versions directory.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "versions"), nil, 0o644)
	r2, _ := OpenRepo(dir2, nil)
	if _, _, err := r2.Bootstrap(fixtureYAML(t), "x"); err == nil {
		t.Fatal("bootstrap must report a write failure")
	}
	// Bootstrap cannot write the version file.
	dir3 := t.TempDir()
	os.MkdirAll(filepath.Join(dir3, "versions", "v0001.yaml.tmp"), 0o755)
	r3, _ := OpenRepo(dir3, nil)
	if _, _, err := r3.Bootstrap(fixtureYAML(t), "x"); err == nil {
		t.Fatal("bootstrap must report a write failure")
	}
}

func TestActivateAndRollbackWriteFailures(t *testing.T) {
	r, dir, cr := repoWithCR(t)
	block := filepath.Join(dir, "versions", "v0002.yaml.tmp")
	os.MkdirAll(block, 0o755)
	if _, err := r.Activate(cr.ID, "architect1"); err == nil {
		t.Fatal("activation must report a write failure")
	}
	os.RemoveAll(block)
	if _, err := r.Activate(cr.ID, "architect1"); err != nil {
		t.Fatalf("activation after the failure: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, "versions", "v0003.yaml.tmp"), 0o755)
	if _, err := r.Rollback(1, "architect1", "x"); err == nil {
		t.Fatal("rollback must report a write failure")
	}
}

func TestTamperedVersionsBlockDecisions(t *testing.T) {
	r, dir, cr := repoWithCR(t)
	path := filepath.Join(dir, "versions", "v0001.yaml")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, append(b, []byte("# edited\n")...), 0o644)
	if _, err := r.Approve(cr.ID, "controller1", ""); err == nil {
		t.Fatal("decisions need a trustworthy active version")
	}
	if _, err := r.Activate(cr.ID, "architect1"); err == nil {
		t.Fatal("activation needs a trustworthy active version")
	}
	os.WriteFile(path, b, 0o644)

	// Rolling back to a tampered older version is refused.
	r.Activate(cr.ID, "architect1")
	os.WriteFile(path, append(b, []byte("# edited\n")...), 0o644)
	if _, err := r.Rollback(1, "architect1", "x"); err == nil || !strings.Contains(err.Error(), "modified outside") {
		t.Fatalf("rollback to a tampered version: %v", err)
	}
}

// Editing a change request in state.json outside the process is caught at activation.
func TestEditedStateIsCaughtAtActivation(t *testing.T) {
	edit := func(t *testing.T, proposed string) error {
		t.Helper()
		_, dir, cr := repoWithCR(t)
		path := filepath.Join(dir, "state.json")
		var st map[string]any
		b, _ := os.ReadFile(path)
		json.Unmarshal(b, &st)
		st["changes"].(map[string]any)[cr.ID].(map[string]any)["proposed"] = proposed
		b, _ = json.Marshal(st)
		os.WriteFile(path, b, 0o644)
		r, err := OpenRepo(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.SetClock(func() time.Time { return clock })
		_, err = r.Activate(cr.ID, "architect1")
		return err
	}
	if err := edit(t, "version: ["); err == nil {
		t.Fatal("unparseable proposal must not activate")
	}
	bad := fixture(t)
	bad.Version = 2
	if err := edit(t, string(yamlOf(t, bad))); err == nil || !strings.Contains(err.Error(), "no longer validates") {
		t.Fatalf("invalid proposal must not activate: %v", err)
	}
}

func TestSaveFailure(t *testing.T) {
	r, dir, cr := repoWithCR(t)
	os.RemoveAll(dir)
	os.WriteFile(dir, []byte("file where the repository was"), 0o644)
	if _, err := r.AddEvidence(cr.ID, Evidence{Kind: "simulation"}); err == nil {
		t.Fatal("state save failure must be reported")
	}
}
