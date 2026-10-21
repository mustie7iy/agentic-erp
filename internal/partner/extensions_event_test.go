package partner_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestEventsMaskCustomFields: BusinessPartnerCreated and
// BusinessPartnerUpdated carry the partner's custom fields and their
// schema version, the sensitive field masked; its values are in no partner
// event.
func TestEventsMaskCustomFields(t *testing.T) {
	e := setup(t)
	secret, later := uuid.NewString(), uuid.NewString()
	p, c := e.create(t, strings.Replace(e.supplier("Geheim", 0), `{"cost_object_hint": "MKT"}`,
		`{"cost_object_hint": "MKT", "routing": "`+secret+`"}`, 1))
	e.mustApply(t, id(c))
	u, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(id(p), `{"extensions": {"segment": "B2B", "routing": "`+later+`"}}`), "")
	// A change requested before T58c carries no schema version: the event
	// names the partner's, as the update keeps it.
	pgtest.Unguarded(t, `UPDATE partner.partner_change SET payload = payload - 'extensions_schema' WHERE id = $1`, id(u))
	e.mustApply(t, id(u))
	for name, want := range map[string]map[string]any{
		"business_partner_created": {"cost_object_hint": "MKT", "routing": extensions.Masked},
		"business_partner_updated": {"cost_object_hint": "MKT", "segment": "B2B", "routing": extensions.Masked},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.events(t, name)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			ext, _ := got[0]["extensions"].(map[string]any)
			for k, v := range want {
				if ext[k] != v {
					t.Errorf("%s: got %v, want %v", k, ext[k], v)
				}
			}
			if got[0]["extensions_schema"] != float64(1) {
				t.Errorf("extensions_schema: got %v, want 1", got[0]["extensions_schema"])
			}
		})
	}
	apitest.NeverPublished(t, e.ten, "partner", secret)
	apitest.NeverPublished(t, e.ten, "partner", later)
}
