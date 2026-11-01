package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// methodsTiny is tiny with its vendor's payment methods (LFB1-ZWELS).
func methodsTiny(zwels string) *sap.Extract {
	x := tiny()
	x.LFB1[0].ZWELS = zwels
	return x
}

// TestSupplierPaymentMethods: a vendor's payment methods (LFB1-ZWELS) load
// in their order as its supplier's methods in the company (ADR-0038); the
// quality gate refuses a method the company's configuration does not have,
// or one listed twice, before anything loads.
func TestSupplierPaymentMethods(t *testing.T) {
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		c.Company("2000").PaymentMethods = []config.PaymentMethod{
			{Code: "T", Description: "Bank transfer", Ranking: []config.BankRanking{{Currency: "EUR", HouseBanks: []string{"MAIN"}}}},
			{Code: "S", Description: "SEPA credit transfer", Ranking: []config.BankRanking{{Currency: "EUR", HouseBanks: []string{"MAIN"}}}}}
	}, perms...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	e := env{Env: f, dir: dir}

	for name, c := range map[string]struct{ zwels, want string }{
		"a method the company does not have": {"TX", `payment method "X" (LFB1-ZWELS) is not a payment method of company 2000 in the configuration`},
		"a method listed twice":              {"TST", `payment method "T" is listed twice in LFB1-ZWELS "TST"`},
	} {
		t.Run(name, func(t *testing.T) {
			file := strings.ReplaceAll(name, " ", "-") + ".json"
			write(t, e.dir, file, methodsTiny(c.zwels))
			_, got := e.act(t, e.run(t, e.connector(t, file), "documents", false, e.DE.String()), "start")
			if got["status"] != "failed" {
				t.Fatalf("run status: got %v, want failed: %v", got["status"], got["report"])
			}
			want := fmt.Sprintf("key:0000000001 message:%s severity:error table:LFB1", c.want)
			if quality := fmt.Sprint(got["report"].(map[string]any)["quality"]); !strings.Contains(quality, want) {
				t.Errorf("quality gate: got %s, want it to report %q", quality, want)
			}
		})
	}

	t.Run("the methods load in their order", func(t *testing.T) {
		write(t, e.dir, "methods.json", methodsTiny("S T"))
		if _, got := e.act(t, e.run(t, e.connector(t, "methods.json"), "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
			t.Fatalf("run status: got %v, want loaded: %v", got["status"], got["report"])
		}
		var methods []string
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT payment_methods FROM partner.supplier_company
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND partner_id IN (SELECT entity_id FROM integration.external_ref WHERE tenant_id = $1 AND object = 'LFA1')`,
			e.Tenant.ID, e.DE).Scan(&methods); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(methods, " "), "S T"; got != want {
			t.Errorf("the supplier's payment methods: got %q, want %q", got, want)
		}
	})
}
