package testagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestCaptureWithoutConfigurationAndDefaultClock(t *testing.T) {
	a := newAgent(t, llmtest.New(t))
	a.Now = nil
	if _, _, err := a.Capture(5); err != nil {
		t.Fatalf("default clock: %v", err)
	}
	a.Repo, _ = config.OpenRepo(t.TempDir(), nil)
	if _, _, err := a.Capture(5); err == nil {
		t.Fatal("no active configuration")
	}
}

func TestRunForCorruptProposal(t *testing.T) {
	dir := t.TempDir()
	repo, _ := config.OpenRepo(dir, nil)
	repo.Bootstrap(testkit.ConfigYAML, "user:architect1")
	c := testkit.Config(t)
	c.PaymentTerms[0].NetDays = 21
	cr, _ := repo.Propose("analyst1", "terms", "", testkit.YAML(t, c))
	state := filepath.Join(dir, "state.json")
	b, _ := os.ReadFile(state)
	s := string(b)
	i := strings.Index(s, `"proposed": "`) + len(`"proposed": "`)
	j := i + strings.Index(s[i:], `",`)
	os.WriteFile(state, []byte(s[:i]+"version: ["+s[j:]), 0o644)
	repo, err := config.OpenRepo(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(t, llmtest.New(t))
	a.Repo = repo
	if _, err := a.RunFor(cr.ID); err == nil {
		t.Fatal("a corrupt proposal cannot be tested")
	}
}
