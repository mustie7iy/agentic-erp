package payments_test

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
)

// canadian is a further house bank of company 2000 at a Canadian bank,
// which has no IBAN (T45d).
func canadian(name string) config.HouseBank {
	return config.HouseBank{Name: name, BIC: "ROYCCAT2", GLAccount: "101000", InstitutionNumber: "003", TransitNumber: "00002", AccountNumber: "100-0001"}
}

// TestCanadianHouseBank: a Canadian house bank is applied with its routing
// in clear text and its account number sealed, and a document that gives
// it other routing under the same name is refused, as a new IBAN is.
func TestCanadianHouseBank(t *testing.T) {
	e := setupWith(t, []string{"payments.statement.read"}, func(*app.Config) {})
	h := houseBanks()
	doc := fixture.Document(t)
	doc.Companies[1].HouseBanks = []config.HouseBank{canadian("RBC")}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.Apply(ctx, doc) }); err != nil {
		t.Fatal(err)
	}
	var last4, institution, transit string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT iban_last4, institution_number, transit_number FROM payments.house_bank
		WHERE legal_entity_id = $1 AND code = 'RBC'`, e.DE).Scan(&last4, &institution, &transit); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{{"last4", last4, "0001"}, {"institution", institution, "003"}, {"transit", transit, "00002"}} {
		if c.got != c.want {
			t.Errorf("house bank %s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	t.Run("listed with its routing", func(t *testing.T) {
		page, _ := e.in(t).Must(200, "GET", "/v1/house-banks?legal_entity_id="+e.DE.String(), "", "")
		listed := false
		for _, it := range page["items"].([]any) {
			b := it.(map[string]any)
			want := "<nil> <nil>" // an IBAN bank has no routing
			if b["code"] == "RBC" {
				want, listed = "0001 003 00002", true
				if got := fmt.Sprint(b["account_last4"], " ", b["institution_number"], " ", b["transit_number"]); got != want {
					t.Errorf("RBC's last4, institution and transit: got %q, want %q", got, want)
				}
				continue
			}
			if got := fmt.Sprint(b["institution_number"], " ", b["transit_number"]); got != want {
				t.Errorf("%v's institution and transit: got %q, want none", b["code"], got)
			}
		}
		if !listed {
			t.Errorf("house banks of company 2000: got %v, want RBC among them", page["items"])
		}
	})
	for name, c := range map[string]struct {
		mutate   func(hb *config.HouseBank)
		findings int
	}{
		"the same account, written with spaces": {func(hb *config.HouseBank) { hb.AccountNumber = "100 0001" }, 0},
		"another transit":                       {func(hb *config.HouseBank) { hb.TransitNumber = "00003" }, 1},
		"another account":                       {func(hb *config.HouseBank) { hb.AccountNumber = "1000002" }, 1},
	} {
		t.Run(name, func(t *testing.T) {
			doc := fixture.Document(t)
			hb := canadian("RBC")
			c.mutate(&hb)
			doc.Companies[1].HouseBanks = []config.HouseBank{hb}
			var findings []config.Finding
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				findings, err = h.Check(ctx, nil, doc)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(findings) != c.findings {
				t.Errorf("findings: got %v, want %d", findings, c.findings)
			}
		})
	}
}

// TestCanadianBankFile: a run from a Canadian house bank to a Canadian
// supplier account writes pain.001 with each bank's CACPA member id and
// the account numbers in Othr (T45d).
func TestCanadianBankFile(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = $2, iban_last4 = '0001', bic = 'ROYCCAT2', institution_number = '003',
		transit_number = '00002', version = version + 1 WHERE id = $1`, e.bank, apitest.Seal(t, e.Tenant.ID, "1000001"))
	account := e.bankAccount(t, e.Supplier, "123456789012", "Maple Supply Ltd", "2026-01-01", "", "create")
	// Bank accounts are insert-only but for their end; the trigger is off
	// to turn the fixture's account into a Canadian one.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET account_scheme = 'ca', institution_number = '004', transit_number = '12345', bic = NULL,
		bank_country_code = 'CA' WHERE id = '%s'; END $$`, account))
	e.invoice(t, e.Supplier, "INV-CA-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	e.transmit(t, id(file), `"1"`)
	if len(e.connector.files) != 1 {
		t.Fatalf("files transmitted: got %d, want 1", len(e.connector.files))
	}
	xml := string(e.connector.files[0].Content)
	for _, want := range []string{
		"<Id>1000001</Id>", "<Cd>CACPA</Cd>", "<MmbId>000300002</MmbId>", "<MmbId>000412345</MmbId>", "<Id>123456789012</Id>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("file: got\n%s\nwant it to contain %s", xml, want)
		}
	}
	if strings.Contains(xml, "<IBAN>") {
		t.Errorf("file: got an IBAN in\n%s\nwant none", xml)
	}
}
