package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/identity"
)

// TestPeopleOutsideRequest: principals' names are read only in a
// request's unit of work; the names, kinds and statuses and the database
// failures are tested through the period close's tasks (internal/closing).
func TestPeopleOutsideRequest(t *testing.T) {
	if _, err := (identity.People{}).Named(context.Background(), []uuid.UUID{uuid.New()}); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
