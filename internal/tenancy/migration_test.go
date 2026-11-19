package tenancy_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestOrgUnitByCodeOutsideAUnitOfWork: the lookup refuses a call without
// a request's unit of work.
func TestOrgUnitByCodeOutsideAUnitOfWork(t *testing.T) {
	if _, _, err := (tenancy.Directory{}).OrgUnitByCode(context.Background(), uuid.New(), "plant", "P1"); err == nil {
		t.Errorf("got nil error, want the call refused outside a unit of work")
	}
}
