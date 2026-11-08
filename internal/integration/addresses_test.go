package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestVendorAddresses: a vendor master's street and city (LFA1 STRAS,
// ORT01, with PSTLZ and REGIO; pending the SAP specialist) load as its
// supplier's legal address, which the order document prints (ADR-0054 As
// built). An extract without them loads the supplier without an address;
// one the platform cannot keep is warned of and left out; neither fails
// the run.
func TestVendorAddresses(t *testing.T) {
	type address struct {
		purpose, line1, city, country string
		postal, region                *string
	}
	text := func(s string) *string { return &s }
	for _, c := range []struct {
		name    string
		vendor  func(v *sap.LFA1)
		want    *address
		warning string // the quality gate's warning about the vendor, "" for none
	}{
		{"street, city, postal code and region", func(v *sap.LFA1) {
			v.STRAS, v.ORT01, v.PSTLZ, v.REGIO = " Hauptstrasse 5 ", "Berlin", "10115", "BE"
		}, &address{"legal", "Hauptstrasse 5", "Berlin", "DE", text("10115"), text("BE")}, ""},
		{"street and city", func(v *sap.LFA1) { v.STRAS, v.ORT01 = "Hauptstrasse 5", "Berlin" },
			&address{"legal", "Hauptstrasse 5", "Berlin", "DE", nil, nil}, ""},
		{"none in the extract", func(*sap.LFA1) {}, nil, ""},
		{"a street without a city", func(v *sap.LFA1) { v.STRAS = "Hauptstrasse 5" }, nil, "a street but no city (ORT01)"},
		{"a city without a street", func(v *sap.LFA1) { v.ORT01 = "Berlin" }, nil, "a city but no street (STRAS)"},
		{"a postal code alone", func(v *sap.LFA1) { v.PSTLZ = "10115" }, nil, "a postal code or region but no street or city"},
		{"a street too long", func(v *sap.LFA1) { v.STRAS, v.ORT01 = strings.Repeat("s", 201), "Berlin" }, nil, "STRAS longer than an address holds"},
		{"a postal code too long", func(v *sap.LFA1) { v.STRAS, v.ORT01, v.PSTLZ = "Hauptstrasse 5", "Berlin", strings.Repeat("9", 21) }, nil,
			"PSTLZ longer than an address holds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t) // a tenant of its own: an earlier run's vendor is extended, not created again
			x := tiny()
			c.vendor(&x.LFA1[0])
			file := strings.ReplaceAll(c.name, " ", "-") + ".json"
			write(t, e.dir, file, x)
			_, got := e.act(t, e.run(t, e.connector(t, file), "documents", false, e.DE.String()), "start")
			if got["status"] != "loaded" {
				t.Fatalf("run status: got %v, want loaded: %v", got["status"], got["report"])
			}
			quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
			if c.warning != "" {
				want := fmt.Sprintf(`key:0000000001 message:vendor "Tiny GmbH" has %s: it loads without an address severity:warning table:LFA1`, c.warning)
				if !strings.Contains(quality, want) {
					t.Errorf("quality gate: got %s, want it to report %q", quality, want)
				}
			} else if strings.Contains(quality, "loads without an address") {
				t.Errorf("quality gate: got %s, want no address warning", quality)
			}
			var a address
			err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT purpose, line1, city, country_code, postal_code, region FROM partner.address
				WHERE tenant_id = $1 AND valid_from = current_date AND valid_to IS NULL
					AND partner_id = (SELECT entity_id FROM integration.external_ref WHERE tenant_id = $1 AND object = 'LFA1' AND external_key = '0000000001')`,
				e.Tenant.ID).Scan(&a.purpose, &a.line1, &a.city, &a.country, &a.postal, &a.region)
			switch {
			case c.want == nil && err == nil:
				t.Errorf("address: got %+v, want none", a)
			case c.want == nil && !errors.Is(err, pgx.ErrNoRows):
				t.Fatal(err)
			case c.want != nil && err != nil:
				t.Fatalf("address: got %v, want %+v", err, *c.want)
			case c.want != nil:
				got, want := show(a.purpose, a.line1, a.city, a.country, a.postal, a.region),
					show(c.want.purpose, c.want.line1, c.want.city, c.want.country, c.want.postal, c.want.region)
				if got != want {
					t.Errorf("address: got %s, want %s", got, want)
				}
			}
		})
	}
}

// deref is s, or "<nil>".
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// show prints an address's fields for a failure message.
func show(purpose, line1, city, country string, postal, region *string) string {
	return fmt.Sprintf("%s: %s, %s %s, %s, %s", purpose, line1, deref(postal), city, deref(region), country)
}
