// Package testkit builds the fixtures shared by tests across packages: a
// store migrated from the mock SAP system, the reference configuration and
// a bootstrapped configuration repository.
package testkit

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

//go:embed config.yaml
var ConfigYAML []byte

// Now is the fixed clock used by tests.
var Now = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)

// MemSource serves an in-memory extract.
type MemSource struct{ E *sap.Extract }

func (m MemSource) Name() string { return "memory" }
func (m MemSource) Extract(_ context.Context, s sap.Scope) (*sap.Extract, error) {
	return sap.FilterScope(m.E, s), nil
}

// Extract is the 2026 mock extract.
func Extract() *sap.Extract { return sap.GenerateMock(42, "2026") }

// Store migrates the 2026 mock data for companies 1000 and 2000.
func Store(t testing.TB) *core.Store {
	t.Helper()
	s := core.NewStore()
	scope := sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"}
	res, err := migration.Run(context.Background(), MemSource{Extract()}, scope, migration.Options{}, s)
	if err != nil || !res.Report.Passed {
		t.Fatalf("fixture migration failed: %v", err)
	}
	return s
}

// Config parses the reference configuration.
func Config(t testing.TB) *config.Config {
	t.Helper()
	c, err := config.Parse(ConfigYAML)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// YAML renders a configuration.
func YAML(t testing.TB, c *config.Config) []byte {
	t.Helper()
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Repo bootstraps a configuration repository in a temp dir.
func Repo(t testing.TB) *config.Repo {
	t.Helper()
	audit, _ := governance.NewAuditLog("")
	r, err := config.OpenRepo(t.TempDir(), audit)
	if err != nil {
		t.Fatal(err)
	}
	r.SetClock(func() time.Time { return Now })
	if _, fs, err := r.Bootstrap(ConfigYAML, "user:architect1"); err != nil {
		t.Fatalf("bootstrap: %v %v", err, fs)
	}
	return r
}
