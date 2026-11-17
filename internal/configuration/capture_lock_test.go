package configuration_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCaptureLeavesTheActiveVersionFree: a capture reads every company's
// invoices of its window, so it does not lock the active version for that
// long; it completes while another transaction holds the version row (an
// activation under way), and its numbering alone is serialized.
func TestCaptureLeavesTheActiveVersionFree(t *testing.T) {
	s := fixture.New(t, append(perms, "config.regression.manage")...)
	deps := fixture.Deps()
	deps.Regressor = &fakeRegressor{cases: []configuration.RegressionCase{caseOf(1, "2000", "auto_post")}}
	bg := context.Background()
	holder, err := pgtest.Admin(t).Begin(bg)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer holder.Rollback(bg)
	if _, err := holder.Exec(bg, `SELECT FROM config.active_version WHERE tenant_id = $1 FOR UPDATE`, s.Tenant.ID); err != nil {
		t.Fatalf("hold the active version: %v", err)
	}
	var numbers []string
	for range 2 {
		if err := apitest.Committed(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
			suite, err := configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "while activating"})
			numbers = append(numbers, suite.Number)
			return err
		}); err != nil {
			t.Fatalf("capture while the active version is held: %v", err)
		}
	}
	if numbers[0] == numbers[1] {
		t.Errorf("suite numbers: got %v, want two different", numbers)
	}
}
