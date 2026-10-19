package payments_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func houseBanks() payments.HouseBanks {
	return payments.HouseBanks{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Channels: integration.BankChannels{}, Keys: apitest.FieldKeys()}
}

// further is a further house bank of company 2000 on its clearing account.
func further(name string) config.HouseBank {
	return config.HouseBank{Name: name, IBAN: "DE02120300000000202051", BIC: "DEUTDEFFXXX", GLAccount: "101000"}
}

type bankRow struct {
	code, last4, currency, status string
	account                       uuid.UUID
}

func banksOf(t *testing.T, le uuid.UUID) map[string]bankRow {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT code, iban_last4, currency, status, gl_account_id FROM payments.house_bank WHERE legal_entity_id = $1`, le)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bankRow{}
	for rows.Next() {
		var b bankRow
		if err := rows.Scan(&b.code, &b.last4, &b.currency, &b.status, &b.account); err != nil {
			t.Fatal(err)
		}
		out[b.code] = b
	}
	return out
}

func TestHouseBanks(t *testing.T) {
	e := setup(t)
	if got := banksOf(t, e.DE)["MAIN"]; got != (bankRow{"MAIN", "3000", "EUR", "active", e.Account(t, "2000", "100000")}) {
		t.Fatalf("house bank from the first configuration: %+v", got)
	}
	h := houseBanks()
	check := func(mutate func(c *config.Config)) []config.Finding {
		t.Helper()
		doc := fixture.Document(t)
		mutate(doc)
		var findings []config.Finding
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			findings, err = h.Check(ctx, nil, doc)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return findings
	}
	if f := check(func(*config.Config) {}); len(f) != 0 {
		t.Fatalf("unchanged: %v", f)
	}
	if f := check(func(c *config.Config) { c.Companies[1].HouseBank.Name = "main-1" }); len(f) != 1 || f[0].Path != "companies[2000].house_bank.name" {
		t.Fatalf("bad name: %v", f)
	}
	if f := check(func(c *config.Config) { c.Companies[1].HouseBank.IBAN = "DE02120300000000202051" }); len(f) != 1 || f[0].Path != "companies[2000].house_bank" {
		t.Fatalf("new IBAN under the same name: %v", f)
	}
	if f := check(func(c *config.Config) { c.Companies[1].HouseBank.BIC = "DEUTDEFFXXX" }); len(f) != 1 {
		t.Fatalf("new BIC under the same name: %v", f)
	}
	if f := check(func(c *config.Config) {
		c.Companies[1].HouseBank.Name = "NEW"
		c.Companies[0].Code = "9999" // the ledger section reports the missing legal entity
	}); len(f) != 0 {
		t.Fatalf("new name: %v", f)
	}

	if f := check(func(c *config.Config) { c.Companies[1].HouseBanks = []config.HouseBank{further("db-1")} }); len(f) != 1 || f[0].Path != "companies[2000].house_banks[db-1].name" {
		t.Fatalf("bad further name: %v", f)
	}

	apply := func(doc *config.Config) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.Apply(ctx, doc) })
	}
	doc := fixture.Document(t)
	doc.Companies[1].HouseBank = config.HouseBank{Name: "NEW", IBAN: "DE02120300000000202051", BIC: "DEUTDEFFXXX"}
	if err := apply(doc); err != nil {
		t.Fatal(err)
	}
	got := banksOf(t, e.DE)
	if got["NEW"].status != "active" {
		t.Errorf("replaced, NEW status: got %s, want active", got["NEW"].status)
	}
	if got["NEW"].last4 != "2051" {
		t.Errorf("replaced, NEW last4: got %s, want 2051", got["NEW"].last4)
	}
	if got["MAIN"].status != "inactive" {
		t.Errorf("replaced, MAIN status: got %s, want inactive", got["MAIN"].status)
	}
	if err := apply(fixture.Document(t)); err != nil {
		t.Fatal(err)
	}
	got = banksOf(t, e.DE)
	if got["NEW"].status != "inactive" {
		t.Errorf("back to MAIN, NEW status: got %s, want inactive", got["NEW"].status)
	}
	if got["MAIN"].status != "active" {
		t.Errorf("back to MAIN, MAIN status: got %s, want active", got["MAIN"].status)
	}

	// A further house bank pays from its own ledger account; dropped from
	// the document, it becomes inactive.
	doc = fixture.Document(t)
	doc.Companies[1].HouseBanks = []config.HouseBank{further("DB")}
	if err := apply(doc); err != nil {
		t.Fatal(err)
	}
	got = banksOf(t, e.DE)
	if want := (bankRow{"DB", "2051", "EUR", "active", e.Account(t, "2000", "101000")}); got["DB"] != want {
		t.Errorf("further bank: got %+v, want %+v", got["DB"], want)
	}
	if got["MAIN"].status != "active" {
		t.Errorf("with a further bank, MAIN status: got %s, want active", got["MAIN"].status)
	}
	if err := apply(fixture.Document(t)); err != nil {
		t.Fatal(err)
	}
	if got := banksOf(t, e.DE)["DB"].status; got != "inactive" {
		t.Errorf("dropped further bank status: got %s, want inactive", got)
	}
	doc = fixture.Document(t)
	doc.Companies[1].HouseBanks = []config.HouseBank{further("DB")}
	doc.Companies[1].HouseBanks[0].GLAccount = "109999"
	if err := apply(doc); err == nil {
		t.Fatal("applied a house bank on an account the legal entity does not have")
	}

	// A company without a legal entity, or without a bank account, cannot be applied.
	doc = fixture.Document(t)
	doc.Companies[0].Code = "9999"
	if err := apply(doc); err == nil {
		t.Fatal("applied a company without a legal entity")
	}
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), '3000', 'New', 'DE', 'EUR', gen_random_uuid(), 'active', $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	doc = fixture.Document(t)
	doc.Companies[0].Code = "3000"
	if err := apply(doc); err == nil {
		t.Fatal("applied a company without a bank account")
	}
	// IBANs that cannot be opened stop the check.
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = '\x00', version = version + 1 WHERE legal_entity_id = $1`, e.DE)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.Check(ctx, nil, fixture.Document(t))
		return err
	})
	if err == nil {
		t.Fatal("checked with a corrupt IBAN")
	}
}

func TestDirConnector(t *testing.T) {
	dir := t.TempDir()
	c := payments.DirConnector{Dir: dir}
	if err := c.Transmit(context.Background(), payments.File{Name: "a.xml", Content: []byte("<x/>")}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.xml")); err != nil || string(b) != "<x/>" {
		t.Fatalf("file: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if err := (payments.DirConnector{Dir: filepath.Join(dir, "missing")}).Transmit(context.Background(), payments.File{Name: "b.xml"}); err == nil {
		t.Fatal("wrote into a missing directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "c.xml", "taken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Transmit(context.Background(), payments.File{Name: "c.xml"}); err == nil {
		t.Fatal("renamed onto a directory")
	}
}
