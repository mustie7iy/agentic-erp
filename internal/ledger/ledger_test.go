package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"ledger.account.read", "ledger.period.read", "ledger.period.manage", "ledger.entry.read", "ledger.entry.post",
	"ledger.rate.read", "ledger.rate.maintain", "ledger.clearing.post"}

// document is the prototype's configuration (companies 1000 in USD and
// 2000 in EUR) with journal number ranges for 2026.
func document(tb testing.TB) *config.Config {
	tb.Helper()
	b, err := os.ReadFile("../config/testdata/valid.yaml")
	if err != nil {
		tb.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		tb.Fatal(err)
	}
	for _, co := range []string{"1000", "2000"} {
		for _, dt := range []string{"journal_sa", "journal_ab"} {
			c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: co, DocumentType: dt, FiscalYear: 2026, First: 100000, Last: 999999})
		}
	}
	return c
}

func deps() configuration.Dependencies {
	return configuration.Dependencies{LegalEntities: tenancy.Directory{},
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}}, partner.PaymentTerms{}}}
}

func endpoints() *ledger.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &ledger.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
}

type env struct {
	apitest.Client
	ten              pgtest.Tenant
	us, de           uuid.UUID // legal entities 1000 (USD) and 2000 (EUR)
	usCal, deCal     uuid.UUID
	supplier, vendor uuid.UUID // active suppliers set up in both legal entities
}

// setup creates a tenant (group currency EUR) with legal entities 1000 and
// 2000, bootstraps the configuration and opens the 2026 periods of both.
func setup(tb testing.TB) env {
	tb.Helper()
	ten := pgtest.NewTenant(tb, perms...)
	ten.AllCompanies(tb, ten.Human)
	e := env{Client: apitest.Client{T: tb, API: apitest.New(tb), Tenant: ten, As: ten.Human}, ten: ten,
		us: uuid.Must(uuid.NewV7()), de: uuid.Must(uuid.NewV7()), usCal: uuid.Must(uuid.NewV7()), deCal: uuid.Must(uuid.NewV7())}
	pgtest.Exec(tb, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme Corp US', 'US', 'USD', $4, 'active', $6, $6), ($1, $3, '2000', 'Acme GmbH', 'DE', 'EUR', $5, 'active', $6, $6)`,
		ten.ID, e.us, e.de, e.usCal, e.deCal, ten.Human.ID)
	doc, _ := json.Marshal(document(tb))
	if err := apitest.Committed(tb, ten, ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, deps(), doc)
		return err
	}); err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period <= 12`, ten.ID)
	e.supplier, e.vendor = e.newSupplier(tb, "BP00000001"), e.newSupplier(tb, "BP00000002")
	return e
}

// newSupplier inserts an active supplier set up in both legal entities on
// their payables reconciliation account 211000.
func (e env) newSupplier(tb testing.TB, number string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $3, 'US', 'active', $4, $4)`, e.ten.ID, id, number, e.ten.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'supplier', $3)`, e.ten.ID, id, e.ten.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		SELECT $1, $2, le.id, a.id, 'NT30', $3, $3 FROM tenancy.legal_entity le JOIN ledger.gl_account a ON a.tenant_id = le.tenant_id AND a.number = '211000'
		JOIN ledger.chart_of_accounts c ON c.id = a.chart_id AND c.code = le.code WHERE le.tenant_id = $1`, e.ten.ID, id, e.ten.Human.ID)
	return id
}

// in returns e reporting to t, so a subtest's failure stops that subtest
// rather than its parent.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.T = t
	return e
}

// account is the id of an account of a company's chart.
func (e env) account(tb testing.TB, company, number string) string {
	tb.Helper()
	page, _ := e.Must(200, "GET", "/v1/accounts?number="+number, "", "")
	admin := pgtest.Admin(tb)
	for _, it := range page["items"].([]any) {
		a := it.(map[string]any)
		var chart string
		if err := admin.QueryRow(context.Background(), `SELECT code FROM ledger.chart_of_accounts WHERE id = $1`, a["chart_id"]).Scan(&chart); err != nil {
			tb.Fatalf("read chart of account: %v", err)
		}
		if chart == company {
			return a["id"].(string)
		}
	}
	tb.Fatalf("no account %s/%s", company, number)
	return ""
}

type line struct {
	account, amount, partner string
}

// entry is a journal entry body.
func entry(le uuid.UUID, date, currency string, lines ...line) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		p := ""
		if l.partner != "" {
			p = fmt.Sprintf(`, "partner_id": %q`, l.partner)
		}
		parts[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": %q}%s}`, l.account, l.amount, currency, p)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q, "lines": [%s]}`,
		le, date, date, currency, strings.Join(parts, ", "))
}

func events(tb testing.TB, ten pgtest.Tenant, name string) []map[string]any {
	tb.Helper()
	return apitest.Published(tb, ten, outbox.Event{Context: "ledger", Name: name})
}

func TestPostAndReverse(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	je, tag := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1200.00", ""}, line{revenue, "-1200.00", ""}), "")
	for field, want := range map[string]any{"document_number": "100000", "fiscal_year": float64(2026), "fiscal_period": float64(3),
		"document_type": "SA", "local_currency": "EUR", "group_currency": "EUR", "rate_tc_lc": "1.000000000000"} {
		if je[field] != want {
			t.Errorf("entry %s: got %v, want %v", field, je[field], want)
		}
	}
	if len(tag) < 10 {
		t.Errorf("entry tag: got %q, want at least 10 characters", tag)
	}
	lines := apitest.List(t, je, "lines")
	if len(lines) < 2 {
		t.Fatalf("entry lines: got %d, want at least 2", len(lines))
	}
	l, ok := lines[1].(map[string]any)
	if !ok {
		t.Fatalf("entry line 2: got %v, want an object", lines[1])
	}
	if got := apitest.Object(t, l, "amount")["amount"]; got != "-1200.00" {
		t.Errorf("line amount: got %v, want -1200.00", got)
	}
	if got := apitest.Object(t, l, "amount_gc")["amount"]; got != "-1200.00" {
		t.Errorf("line amount_gc: got %v, want -1200.00", got)
	}
	rev, _ := e.Must(200, "POST", "/v1/journal-entries/"+je["id"].(string)+":reverse", "", tag)
	if rev["reversal_of_id"] != je["id"] {
		t.Errorf("reversal_of_id: got %v, want %v", rev["reversal_of_id"], je["id"])
	}
	if rev["document_number"] != "100001" {
		t.Errorf("reversal document_number: got %v, want 100001", rev["document_number"])
	}
	if rev["header_text"] != "Reversal of 100000" {
		t.Errorf("reversal header_text: got %v, want Reversal of 100000", rev["header_text"])
	}
	got, _ := e.Must(200, "GET", "/v1/journal-entries/"+je["id"].(string), "", "")
	if got["reversed_by_id"] != rev["id"] {
		t.Fatalf("original: %v", got)
	}
	if posted := events(t, e.ten, "journal_entry_posted"); len(posted) != 2 {
		t.Fatalf("posted events: %v", posted)
	}
	reversed := events(t, e.ten, "journal_entry_reversed")
	if len(reversed) != 1 {
		t.Fatalf("reversed events: got %d %v, want 1", len(reversed), reversed)
	}
	if reversed[0]["reversal_entry_id"] != rev["id"] {
		t.Errorf("reversed event reversal_entry_id: got %v, want %v", reversed[0]["reversal_entry_id"], rev["id"])
	}
}
