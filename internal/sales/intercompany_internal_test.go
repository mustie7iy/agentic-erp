package sales

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestRecordIntercompanyOutsideRequest: an intercompany billing's audit
// record needs the request's audit buffer, which only the unit of work
// gives; without it the record fails, and with it the billing.
func TestRecordIntercompanyOutsideRequest(t *testing.T) {
	if err := recordIntercompany(context.Background(), IntercompanyBilling{Currency: "EUR", Exponent: 2}, tenancy.Entity{Code: "1000"}); err == nil {
		t.Error("outside a request: got nil error, want the audit buffer's")
	}
}
