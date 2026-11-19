package taxfiling_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
)

// TestInvoiceNumber: a company's FTA registration number is opened for a
// tax invoice dated within its periods, not for one dated before its
// first period or after its last; outside a request nothing is read.
func TestInvoiceNumber(t *testing.T) {
	r := taxfiling.InvoiceNumbers{Keys: apitest.FieldKeys()}
	if _, _, _, err := r.Number(context.Background(), uuid.New(), "FTA", time.Now()); err == nil {
		t.Error("outside a request: got nil error, want one")
	}
	ten := pgtest.NewTenant(t)
	le := uuid.New()
	pgtest.Exec(t, `INSERT INTO tax.registration (tenant_id, id, code, legal_entity_id, authority, number_cipher, number_fingerprint, number_last4,
			return_type, frequency, first_period_start, last_period_end, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'VAT', $2, 'FTA', $3, repeat('a', 64), '0003', 'VAT201', 'quarterly', '2026-04-01', '2026-12-31', $4, $4)`,
		ten.ID, le, apitest.Seal(t, ten.ID, "100234567800003"), ten.Human.ID)
	for _, c := range []struct{ day, want string }{
		{"2026-03-31", ""}, {"2026-04-01", "100234567800003"}, {"2026-12-31", "100234567800003"}, {"2027-01-01", ""},
	} {
		t.Run(c.day, func(t *testing.T) {
			day, _ := time.Parse(time.DateOnly, c.day) // a constant date
			var got string
			if err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
				var err error
				got, _, _, err = r.Number(ctx, le, "FTA", day)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
