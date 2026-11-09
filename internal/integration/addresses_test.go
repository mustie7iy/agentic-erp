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

// TestVendorAddresses: a vendor's central address (ADRC through
// LFA1-ADRNR, po:2026-11-09, A1) loads as its supplier's legal address,
// which the order document prints (ADR-0054 As built). An extract without
// one loads the supplier without an address; one the platform cannot keep
// is warned of and left out; neither fails the run. The choice among an
// address's rows and every refusal are TestCentralAddress's.
func TestVendorAddresses(t *testing.T) {
	type address struct {
		purpose, line1, city, country string
		line2, postal, region         *string
	}
	text := func(s string) *string { return &s }
	addressed := func(rows ...sap.ADRC) func(x *sap.Extract) {
		return func(x *sap.Extract) {
			x.LFA1[0].ADRNR = "0000004711"
			for _, r := range rows {
				r.ADDRNUMBER = "0000004711"
				x.ADRC = append(x.ADRC, r)
			}
		}
	}
	for _, c := range []struct {
		name    string
		extract func(x *sap.Extract)
		want    *address
		warning string // the quality gate's warning about the vendor, "" for none
	}{
		{"street, house number, further lines, city, postal code, region and country", addressed(sap.ADRC{
			DATE_FROM: "00010101", DATE_TO: "99991231", STREET: " Hauptstrasse ", HOUSE_NUM1: "5", HOUSE_NUM2: "a", STR_SUPPL1: "Hinterhaus",
			LOCATION: "2. OG", CITY1: "Berlin", POST_CODE1: "10115", REGION: "BE", COUNTRY: "DE"}),
			&address{"legal", "Hauptstrasse 5 a", "Berlin", "DE", text("Hinterhaus, 2. OG"), text("10115"), text("BE")}, ""},
		{"the house number in the street and the vendor's country", addressed(sap.ADRC{STREET: "Hauptstrasse 5", CITY1: "Berlin"}),
			&address{"legal", "Hauptstrasse 5", "Berlin", "DE", nil, nil, nil}, ""},
		{"none in the extract", func(*sap.Extract) {}, nil, ""},
		{"an address no row of which is valid today", addressed(sap.ADRC{DATE_TO: "20191231", STREET: "Altweg 1", CITY1: "Bonn"}), nil,
			"no central address (ADRC 0000004711) valid on "},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t) // a tenant of its own: an earlier run's vendor is extended, not created again
			x := tiny()
			c.extract(x)
			file := strings.ReplaceAll(c.name, " ", "-") + ".json"
			write(t, e.dir, file, x)
			_, got := e.act(t, e.run(t, e.connector(t, file), "documents", false, e.DE.String()), "start")
			if got["status"] != "loaded" {
				t.Fatalf("run status: got %v, want loaded: %v", got["status"], got["report"])
			}
			quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
			if c.warning != "" {
				want := fmt.Sprintf(`key:0000000001 message:vendor "Tiny GmbH" has %s`, c.warning)
				if !strings.Contains(quality, want) {
					t.Errorf("quality gate: got %s, want it to report %q", quality, want)
				}
			} else if strings.Contains(quality, "loads without an address") {
				t.Errorf("quality gate: got %s, want no address warning", quality)
			}
			var a address
			err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT purpose, line1, city, country_code, line2, postal_code, region FROM partner.address
				WHERE tenant_id = $1 AND valid_from = current_date AND valid_to IS NULL
					AND partner_id = (SELECT entity_id FROM integration.external_ref WHERE tenant_id = $1 AND object = 'LFA1' AND external_key = '0000000001')`,
				e.Tenant.ID).Scan(&a.purpose, &a.line1, &a.city, &a.country, &a.line2, &a.postal, &a.region)
			switch {
			case c.want == nil && err == nil:
				t.Errorf("address: got %+v, want none", a)
			case c.want == nil && !errors.Is(err, pgx.ErrNoRows):
				t.Fatal(err)
			case c.want != nil && err != nil:
				t.Fatalf("address: got %v, want %+v", err, *c.want)
			case c.want != nil:
				got, want := show(a.purpose, a.line1, a.city, a.country, a.line2, a.postal, a.region),
					show(c.want.purpose, c.want.line1, c.want.city, c.want.country, c.want.line2, c.want.postal, c.want.region)
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
func show(purpose, line1, city, country string, line2, postal, region *string) string {
	return fmt.Sprintf("%s: %s, %s, %s %s, %s, %s", purpose, line1, deref(line2), deref(postal), city, deref(region), country)
}
