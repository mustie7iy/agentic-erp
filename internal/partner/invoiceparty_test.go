package partner_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestInvoicePartyNotFound: a partner the tenant does not have is not
// found, and outside a request nothing is read (the rest is the
// einvoicing context's TestRender*, which renders through it).
func TestInvoicePartyNotFound(t *testing.T) {
	ten := pgtest.NewTenant(t)
	r := partner.InvoiceParties{Keys: apitest.FieldKeys()}
	if _, _, err := r.Party(context.Background(), uuid.New(), "AE_TRN", time.Now()); err == nil {
		t.Error("outside a request: got nil error, want one")
	}
	err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		_, ok, err := r.Party(ctx, uuid.New(), "AE_TRN", time.Now())
		if ok {
			t.Error("an unknown partner: got found, want not found")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
