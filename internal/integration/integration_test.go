package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"integration.connector.read", "integration.connector.manage", "integration.migration.read", "integration.migration.run",
	"platform.operation.read", "ledger.entry.read", "ledger.entry.post", "payables.invoice.read", "procurement.order.read"}

type env struct {
	fixture.Env
	dir string
}

// setup is a configured tenant whose API reads extracts from a directory
// holding the mock SAP extract of 2026 (extract.json), with USD rates for
// company 1000's group currency.
func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	dir := t.TempDir()
	write(t, dir, "extract.json", sap.GenerateMock(42, "2026"))
	f.API = apitest.NewWith(t, func(c *httpapi.Config) { c.ExtractDir = dir })
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	return env{Env: f, dir: dir}
}

func write(t *testing.T, dir, name string, e *sap.Extract) {
	t.Helper()
	if err := sap.WriteExtract(filepath.Join(dir, name), e); err != nil {
		t.Fatal(err)
	}
}

// tiny is a scope of company 2000: one vendor with a bank account, an order
// received and invoiced (parked), and an invoice paid and cleared.
func tiny() *sap.Extract {
	return &sap.Extract{
		T001: []sap.T001{{BUKRS: "2000", BUTXT: "Acme GmbH", WAERS: "EUR", LAND1: "DE"}},
		SKB1: []sap.SKB1{{BUKRS: "2000", SAKNR: "0000100000", TXT50: "Bank", XBILK: "X"}, {BUKRS: "2000", SAKNR: "0000211000", TXT50: "AP", XBILK: "X", MITKZ: "K"},
			{BUKRS: "2000", SAKNR: "0000520000", TXT50: "Office"}},
		LFA1: []sap.LFA1{{LIFNR: "0000000001", NAME1: "Tiny GmbH", LAND1: "DE", STCD1: "DE123"}},
		LFB1: []sap.LFB1{{LIFNR: "0000000001", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"}},
		LFBK: []sap.LFBK{{LIFNR: "0000000001", BANKS: "DE", BANKN: "DE89370400440532013000"}},
		EKKO: []sap.EKKO{{EBELN: "4500000001", BUKRS: "2000", LIFNR: "0000000001", WAERS: "EUR", BEDAT: "20260301"}},
		EKPO: []sap.EKPO{{EBELN: "4500000001", EBELP: "00010", MATNR: "MAT-1", MENGE: "2", NETPR: "50.00", PEINH: "1", SAKTO: "0000520000"}},
		MSEG: []sap.MSEG{{MBLNR: "5000000001", MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: "00010", MENGE: "2", BUDAT: "20260305"}},
		RBKP: []sap.RBKP{{BELNR: "5100000001", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "T-1", BLDAT: "20260310", WAERS: "EUR",
			RMWWR: "100.00", WMWST1: "0.00", RBSTAT: "A"}},
		RSEG: []sap.RSEG{{BELNR: "5100000001", GJAHR: "2026", BUZEI: "000001", EBELN: "4500000001", EBELP: "00010", MENGE: "2", WRBTR: "100.00"}},
		ACDOCA: []sap.ACDOCA{
			{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000001", DOCLN: "000001", RACCT: "0000520000", HSL: "80.00", BUDAT: "20260302", BLART: "RE", XBLNR: "P-1"},
			{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000001", DOCLN: "000002", RACCT: "0000211000", HSL: "-80.00", BUDAT: "20260302", BLART: "RE",
				LIFNR: "0000000001", XBLNR: "P-1", AUGBL: "0100000002"},
			{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000001", DOCLN: "000003", RACCT: "0000520000", HSL: "0.00", BUDAT: "20260302", BLART: "RE"},
			{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000002", DOCLN: "000001", RACCT: "0000211000", HSL: "80.00", BUDAT: "20260320", BLART: "KZ",
				LIFNR: "0000000001", AUGBL: "0100000002"},
			{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000002", DOCLN: "000002", RACCT: "0000100000", HSL: "-80.00", BUDAT: "20260320", BLART: "KZ"},
		},
	}
}

func (e env) connector(t *testing.T, path string) string {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/connectors", fmt.Sprintf(`{"kind": "sap_file", "name": %q, "settings": {"path": %q}}`, "SAP "+path, path), "")
	return c["id"].(string)
}

func scope(les ...string) string {
	return `{"legal_entity_ids": ["` + strings.Join(les, `", "`) + `"], "fiscal_year": 2026}`
}

func (e env) run(t *testing.T, connector, mode string, rehearsal bool, les ...string) map[string]any {
	t.Helper()
	r, _ := e.Must(201, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": %s, "mode": %q, "is_rehearsal": %t}`,
		connector, scope(les...), mode, rehearsal), "")
	return r
}

func (e env) act(t *testing.T, run map[string]any, action string) (map[string]any, map[string]any) {
	t.Helper()
	path := "/v1/migration-runs/" + run["id"].(string)
	_, tag := e.Must(200, "GET", path, "", "")
	op, _ := e.Must(202, "POST", path+":"+action, "", tag)
	got, _ := e.Must(200, "GET", path, "", "")
	return op, got
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e env) entries(t *testing.T) int {
	t.Helper()
	return count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, e.Tenant.ID)
}

func checks(report map[string]any) string {
	var out []string
	for _, c := range report["checks"].([]any) {
		m := c.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %v", m["company_code"], m["name"], m["pass"]))
	}
	return strings.Join(out, "; ")
}

func TestMigrateAndReconcile(t *testing.T) {
	e := setup(t)
	c := e.connector(t, "extract.json")

	// A rehearsal loads, reconciles and leaves nothing behind but its report.
	rehearsal := e.run(t, c, "documents", true, e.US.String())
	op, got := e.act(t, rehearsal, "start")
	if got["status"] != "reconciled" {
		t.Fatalf("rehearsal status: got %v, want reconciled: %v", got["status"], got)
	}
	if op["status"] != "succeeded" {
		t.Errorf("rehearsal operation status: got %v, want succeeded", op["status"])
	}
	if passed := got["report"].(map[string]any)["passed"]; passed != true {
		t.Errorf("rehearsal report passed: got %v, want true", passed)
	}
	if n := e.entries(t); n != 0 {
		t.Errorf("rehearsal journal entries: got %d, want 0", n)
	}
	if got["finished_at"] == nil {
		t.Errorf("rehearsal finished_at: got nil, want a time")
	}
	if got["operation_id"] != op["id"] {
		t.Errorf("rehearsal operation_id: got %v, want %v", got["operation_id"], op["id"])
	}

	run := e.run(t, c, "documents", false, e.US.String())
	op, got = e.act(t, run, "start")
	report := got["report"].(map[string]any)
	loaded := report["loaded"].(map[string]any)
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got)
	}
	if op["kind"] != "integration.migrate" {
		t.Errorf("start operation kind: got %v, want integration.migrate", op["kind"])
	}
	for k, want := range map[string]float64{"journal_entries": 43, "suppliers": 18, "clearings": 6, "suppliers_blocked": 1} {
		if loaded[k] != want {
			t.Errorf("start loaded %s: got %v, want %v", k, loaded[k], want)
		}
	}
	if got["finished_at"] != nil {
		t.Errorf("start finished_at: got %v, want nil", got["finished_at"])
	}
	if q := report["quality"].([]any); len(q) != 4 || q[0].(map[string]any)["severity"] != "warning" {
		t.Fatalf("quality: %v", q)
	}
	op, got = e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconcile status: got %v, want reconciled: %v", got["status"], got["report"])
	}
	if op["kind"] != "integration.reconcile" {
		t.Errorf("reconcile operation kind: got %v, want integration.reconcile", op["kind"])
	}
	if c := checks(got["report"].(map[string]any)); !strings.Contains(c, "1000 Vendors true") {
		t.Errorf("reconcile checks: got %s, want 1000 Vendors true among them", c)
	}
	e.Problem(409, "integration.invalid_status", "POST", "/v1/migration-runs/"+run["id"].(string)+":reconcile", "", `"3"`)
	e.Problem(409, "integration.invalid_status", "POST", "/v1/migration-runs/"+run["id"].(string)+":start", "", `"3"`)

	// Company 2000 shares four vendors with 1000: they are set up in the second legal entity.
	second := e.run(t, c, "documents", false, e.DE.String(), e.US.String())
	_, got = e.act(t, second, "start")
	loaded = got["report"].(map[string]any)["loaded"].(map[string]any)
	if got["status"] != "loaded" {
		t.Fatalf("second scope status: got %v, want loaded: %v", got["status"], got["report"])
	}
	for k, want := range map[string]float64{"suppliers_extended": 4, "suppliers": 6} {
		if loaded[k] != want {
			t.Errorf("second scope loaded %s: got %v, want %v", k, loaded[k], want)
		}
	}
	if _, got = e.act(t, second, "reconcile"); got["status"] != "reconciled" {
		t.Fatalf("second reconcile: %v", checks(got["report"].(map[string]any)))
	}
	// A run of a scope already loaded loads nothing again.
	again := e.run(t, c, "documents", false, e.DE.String())
	if _, got = e.act(t, again, "start"); got["status"] != "loaded" || len(got["report"].(map[string]any)["loaded"].(map[string]any)) != 0 {
		t.Fatalf("again: %v", got["report"])
	}
	completed := apitest.Published(t, e.Tenant, outbox.Event{Context: "integration", Name: "migration_run_completed"})
	if len(completed) != 3 {
		t.Fatalf("completed events: got %d, want 3: %v", len(completed), completed)
	}
	if completed[0]["is_rehearsal"] != true {
		t.Errorf("first completed event is_rehearsal: got %v, want true", completed[0]["is_rehearsal"])
	}
	if completed[1]["summary"] == nil {
		t.Errorf("second completed event summary: got nil, want a summary")
	}

	page, _ := e.Must(200, "GET", "/v1/migration-runs?status=reconciled&page_size=1", "", "")
	if len(page["items"].([]any)) != 1 || page["next_page_token"] == nil {
		t.Fatalf("list: %v", page)
	}
	page, _ = e.Must(200, "GET", "/v1/migration-runs?status=reconciled&page_size=5&page_token="+page["next_page_token"].(string), "", "")
	if len(page["items"].([]any)) != 2 {
		t.Fatalf("second page: %v", page)
	}
}

func TestBalancesMode(t *testing.T) {
	e := setup(t)
	c := e.connector(t, "extract.json")
	run := e.run(t, c, "balances", false, e.US.String(), e.DE.String())
	_, got := e.act(t, run, "start")
	if got["status"] != "loaded" || got["report"].(map[string]any)["loaded"].(map[string]any)["journal_entries"] != float64(2) {
		t.Fatalf("balances: %v", got["report"])
	}
	if _, got = e.act(t, run, "reconcile"); got["status"] != "reconciled" || strings.Contains(checks(got["report"].(map[string]any)), "debits") {
		t.Fatalf("reconcile: %v", got["report"])
	}
	again := e.run(t, c, "balances", false, e.US.String(), e.DE.String())
	if _, got = e.act(t, again, "start"); got["status"] != "loaded" || len(got["report"].(map[string]any)["loaded"].(map[string]any)) != 0 {
		t.Fatalf("again: %v", got["report"])
	}
	// A company without journal lines gets no opening entry; inconsistent clearing data is refused.
	x := tiny()
	x.ACDOCA[3].HSL, x.ACDOCA[4].HSL = "70.00", "-70.00"
	write(t, e.dir, "uneven.json", x)
	fresh := fixture.New(t, perms...)
	fresh.API = e.API
	other := env{Env: fresh, dir: e.dir}
	_, got = other.act(t, other.run(t, other.connector(t, "uneven.json"), "balances", false, fresh.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("uneven clearing status: got %v, want failed: %v", got["status"], got["report"])
	}
	if load := got["report"].(map[string]any)["load"].([]any); len(load) != 1 {
		t.Fatalf("uneven clearing load messages: got %d, want 1: %v", len(load), load)
	} else if msg := load[0].(map[string]any)["message"].(string); !strings.Contains(msg, "clearing data") {
		t.Fatalf("uneven clearing message: got %q, want it to mention clearing data", msg)
	}
	empty := tiny()
	empty.ACDOCA = nil
	empty.EKPO = append(empty.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MATNR: "MAT-2", MENGE: "1", NETPR: "10.00", PEINH: "1", SAKTO: "0000520000"})
	empty.MSEG = append([]sap.MSEG{{MBLNR: "5000000001", MJAHR: "2026", ZEILE: "0002", BWART: "101", EBELN: "4500000001", EBELP: "00020", MENGE: "1",
		BUDAT: "20260305"}}, empty.MSEG...)
	write(t, e.dir, "empty.json", empty)
	if _, got = other.act(t, other.run(t, other.connector(t, "empty.json"), "balances", false, fresh.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("no journal: %v", got["report"])
	}
	// An opening entry the ledger refuses fails the load.
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND period = 3`,
		fresh.Tenant.ID, fresh.DE)
	write(t, e.dir, "tiny.json", tiny())
	_, got = other.act(t, other.run(t, other.connector(t, "tiny.json"), "balances", false, fresh.DE.String()), "start")
	if got["status"] != "failed" || !strings.Contains(fmt.Sprint(got["report"].(map[string]any)["load"]), "OB/2000/2026") {
		t.Fatalf("closed period: %v", got["report"])
	}
}

func TestQualityGateAndRefusedLoads(t *testing.T) {
	e := setup(t)
	bad := tiny()
	bad.T001[0].WAERS = "USD"
	bad.SKB1 = append(bad.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000999999"}, sap.SKB1{BUKRS: "2000", SAKNR: "0000140000", MITKZ: "K"})
	bad.LFA1 = append(bad.LFA1, sap.LFA1{LIFNR: "0000000009", LAND1: "DE"}, sap.LFA1{LIFNR: "0000000010", NAME1: "Tiny GmbH", LAND1: "DE", SPERR: "X"})
	bad.LFB1 = append(bad.LFB1, sap.LFB1{LIFNR: "0000000099", BUKRS: "2000", AKONT: "0000520000", ZTERM: "ZZ99"},
		sap.LFB1{LIFNR: "0000000009", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"}, sap.LFB1{LIFNR: "0000000010", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"})
	bad.EKKO = append(bad.EKKO, sap.EKKO{EBELN: "4500000002", BUKRS: "2000", LIFNR: "0000000077", WAERS: "EUR", BEDAT: "2026"})
	bad.EKPO = append(bad.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MENGE: "-1", NETPR: "x", PEINH: "0", SAKTO: "0000888888"})
	bad.MSEG = append(bad.MSEG, sap.MSEG{MBLNR: "5000000002", MJAHR: "2026", ZEILE: "0001", BWART: "102", EBELN: "4500000001", EBELP: "00010"})
	bad.ACDOCA = append(bad.ACDOCA,
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000003", DOCLN: "000001", RACCT: "0000777777", HSL: "1.00", BUDAT: "2026", BLART: "XX"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000003", DOCLN: "000002", RACCT: "0000211000", HSL: "x", BUDAT: "20260301", BLART: "XX"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000003", DOCLN: "000003", RACCT: "0000211000", HSL: "-0.50", BUDAT: "20260301", BLART: "XX"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000004", DOCLN: "000001", RACCT: "0000211000", HSL: "0.00", BUDAT: "20260301", BLART: "SA",
			LIFNR: "0000000066"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000005", DOCLN: "000001", RACCT: "0000211000", HSL: "-5.00", BUDAT: "20260301", BLART: "SA",
			LIFNR: "0000000010"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000005", DOCLN: "000002", RACCT: "0000100000", HSL: "5.00", BUDAT: "20260301", BLART: "SA"})
	bad.RBKP = append(bad.RBKP,
		sap.RBKP{BELNR: "5100000002", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "t 1", BLDAT: "20260310", WAERS: "EUR", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "A"},
		sap.RBKP{BELNR: "5100000003", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", BLDAT: "x", WAERS: "EUR", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "A"},
		sap.RBKP{BELNR: "5100000004", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", BLDAT: "20260310", WAERS: "EUR", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "5"},
		sap.RBKP{BELNR: "5100000005", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", BLDAT: "20260310", WAERS: "EUR", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "B"},
		sap.RBKP{BELNR: "5100000006", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "T-6", BLDAT: "20260310", WAERS: "XXX", RMWWR: "1", WMWST1: "0", RBSTAT: "A"})
	bad.RSEG = append(bad.RSEG, sap.RSEG{BELNR: "5100000006", GJAHR: "2026", BUZEI: "000001", WRBTR: "1"})
	write(t, e.dir, "bad.json", bad)
	run := e.run(t, e.connector(t, "bad.json"), "documents", false, e.DE.String())
	op, got := e.act(t, run, "start")
	quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
	for _, want := range []string{"not the legal entity's local currency", "not in the legal entity's chart", "reconciliation account on one side",
		"vendor has no name", "possible duplicate vendors", "without vendor master", "not a vendor reconciliation account of company", `payment terms "ZZ99"`,
		"vendor 0000000077 not in extract", "invalid SAP date", "G/L account 888888", "not a positive number", "price unit",
		"movement type 102", "missing from chart of accounts", "without vendor", "vendor 0000000066 not in extract", "journal_xx number range",
		"does not balance", "posting-blocked but has open payables", "repeats invoice", "unparseable amounts", "status \"B\"", "currency \"XXX\""} {
		if !strings.Contains(quality, want) {
			t.Errorf("quality gate: got %s, want it to report %q", quality, want)
		}
	}
	if got["status"] != "failed" {
		t.Fatalf("run status: got %v, want failed", got["status"])
	}
	if op["status"] != "failed" {
		t.Fatalf("operation status: got %v, want failed", op["status"])
	}
	if code := op["error"].(map[string]any)["code"]; code != "integration.migration_failed" {
		t.Fatalf("operation error: got %v, want integration.migration_failed", code)
	}
	if n := e.entries(t); n != 0 {
		t.Fatalf("journal entries: got %d, want 0", n)
	}

	// Records the services refuse fail the load as a whole; nothing stays.
	refused := tiny()
	refused.MSEG[0].BUDAT = "2026"
	refused.LFA1[0].LAND1 = "ZZ"
	write(t, e.dir, "refused.json", refused)
	_, got = e.act(t, e.run(t, e.connector(t, "refused.json"), "documents", false, e.DE.String()), "start")
	load := fmt.Sprint(got["report"].(map[string]any)["load"])
	if got["status"] != "failed" {
		t.Fatalf("refused status: got %v, want failed", got["status"])
	}
	for _, want := range []string{"invalid SAP date", "LFA1"} {
		if !strings.Contains(load, want) {
			t.Fatalf("refused load: got %s, want it to mention %q", load, want)
		}
	}
	if n := count(t, `SELECT count(*) FROM integration.external_ref WHERE tenant_id = $1`, e.Tenant.ID); n != 0 {
		t.Fatalf("refused external refs: got %d, want 0", n)
	}
	// A rehearsal whose controls fail reports the differences and missing documents.
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND period = 3`,
		e.Tenant.ID, e.DE)
	write(t, e.dir, "tiny.json", tiny())
	tinyConnector := e.connector(t, "tiny.json")
	_, got = e.act(t, e.run(t, tinyConnector, "documents", true, e.DE.String()), "start")
	if got["status"] != "failed" || !strings.Contains(fmt.Sprint(got["report"].(map[string]any)["load"]), "period_not_open") {
		t.Fatalf("closed period: %v", got["report"])
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND period = 3`,
		e.Tenant.ID, e.DE)
	run = e.run(t, tinyConnector, "documents", false, e.DE.String())
	e.act(t, run, "start")
	// Something posted outside the migration, and a document missing on the platform, fail the controls.
	pgtest.Exec(t, `DELETE FROM integration.external_ref WHERE tenant_id = $1 AND object = 'ACDOCA' AND external_key LIKE '%0100000002'`, e.Tenant.ID)
	stranger := e.NewSupplier(t, "BP90000099")
	e.As(e.Approver).Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-01", "posting_date": "2026-03-01",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "-9.00", "currency": "EUR"}, "partner_id": %q},
		{"account_id": %q, "amount": {"amount": "9.00", "currency": "EUR"}}]}`, e.DE, e.Account(t, "2000", "211000"), stranger, e.Account(t, "2000", "520000")), "")
	_, got = e.act(t, run, "reconcile")
	report := got["report"].(map[string]any)
	if got["status"] != "failed" {
		t.Errorf("failed controls status: got %v, want failed", got["status"])
	}
	if missing, _ := report["missing_documents"].([]any); len(missing) != 1 {
		t.Errorf("failed controls missing documents: got %d, want 1: %v", len(missing), missing)
	}
	if diffs := fmt.Sprint(report["diffs"]); !strings.Contains(diffs, "partner "+stranger.String()) {
		t.Errorf("failed controls diffs: got %s, want partner %s among them", diffs, stranger)
	}
}

func TestConnectorsAndRunRules(t *testing.T) {
	e := setup(t)
	c, tag := e.Must(201, "POST", "/v1/connectors", `{"kind": "sap_odata", "name": "S4", "settings": {"base_url": "https://s4.example.com", "client": "100",
		"page_size": 500, "system": "S4_PRD"}, "secret_ref": "env:ERP_TEST_SAP"}`, "")
	path := "/v1/connectors/" + c["id"].(string)
	if c["status"] != "active" {
		t.Fatalf("created status: got %v, want active", c["status"])
	}
	if c["secret_ref"] != "env:ERP_TEST_SAP" {
		t.Fatalf("created secret_ref: got %v, want env:ERP_TEST_SAP", c["secret_ref"])
	}
	if tag != `"1"` {
		t.Fatalf("created ETag: got %s, want %s", tag, `"1"`)
	}
	for name, body := range map[string]string{
		"unknown kind":        `{"kind": "fax", "name": "x", "settings": {}}`,
		"path escapes":        `{"kind": "sap_file", "name": "x", "settings": {"path": "../etc"}}`,
		"not http":            `{"kind": "sap_odata", "name": "x", "settings": {"base_url": "ftp://x"}}`,
		"client not a string": `{"kind": "sap_odata", "name": "x", "settings": {"base_url": "https://x", "client": 100}}`,
		"page size 0":         `{"kind": "sap_odata", "name": "x", "settings": {"base_url": "https://x", "page_size": 0}}`,
		"lower-case system":   `{"kind": "sap_file", "name": "x", "settings": {"path": "a.json", "system": "sap"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, _, _ := e.Do("POST", "/v1/connectors", body, ""); got != 422 {
				t.Errorf("%s: got %d, want 422", body, got)
			}
		})
	}
	e.Problem(409, "integration.duplicate_name", "POST", "/v1/connectors", `{"kind": "email", "name": "S4", "settings": {}}`, "")
	upd, tag := e.Must(200, "PATCH", path, `{"name": "S4 PRD", "settings": {"base_url": "https://s4.example.com"}, "secret_ref": null}`, tag)
	if upd["name"] != "S4 PRD" {
		t.Fatalf("updated name: got %v, want S4 PRD", upd["name"])
	}
	if upd["secret_ref"] != nil {
		t.Fatalf("updated secret_ref: got %v, want nil", upd["secret_ref"])
	}
	if client := upd["settings"].(map[string]any)["client"]; client != nil {
		t.Fatalf("updated settings.client: got %v, want nil", client)
	}
	e.Problem(422, "integration.invalid_settings", "PATCH", path, `{"settings": {}}`, tag)
	_, tag = e.Must(200, "PATCH", path, `{"secret_ref": "env:ERP_TEST_SAP"}`, tag)
	e.Problem(409, "integration.invalid_status", "POST", path+":enable", "", tag)
	disabled, tag := e.Must(200, "POST", path+":disable", `{"reason": "Cutover done"}`, tag)
	if disabled["status"] != "disabled" {
		t.Fatalf("disabled: %v", disabled)
	}
	run := e.run(t, c["id"].(string), "documents", false, e.DE.String())
	e.Problem(409, "integration.connector_disabled", "POST", "/v1/migration-runs/"+run["id"].(string)+":start", "", `"1"`)
	e.Must(200, "POST", path+":enable", "", tag)
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "integration", Name: "connector_enabled"})
	if len(evs) != 2 {
		t.Fatalf("connector_enabled events: got %d, want 2: %v", len(evs), evs)
	}
	if evs[0]["previous_status"] != nil {
		t.Errorf("first event previous_status: got %v, want nil", evs[0]["previous_status"])
	}
	if evs[1]["previous_status"] != "disabled" {
		t.Errorf("second event previous_status: got %v, want disabled", evs[1]["previous_status"])
	}
	got, _ := e.Must(200, "GET", path, "", "")
	if got["name"] != "S4 PRD" {
		t.Fatalf("get: %v", got)
	}
	page, _ := e.Must(200, "GET", "/v1/connectors?kind=sap_odata&page_size=1", "", "")
	if len(page["items"].([]any)) != 1 {
		t.Fatalf("list: %v", page)
	}
	e.Must(201, "POST", "/v1/connectors", `{"kind": "email", "name": "Mail", "settings": {}}`, "")
	page, _ = e.Must(200, "GET", "/v1/connectors?page_size=1", "", "")
	if page["next_page_token"] == nil {
		t.Fatalf("list page: %v", page)
	}

	// OData sources: credentials come from the referenced secret; extraction failures leave the run queued.
	runPath := "/v1/migration-runs/" + run["id"].(string)
	e.Problem(422, "integration.secret_unavailable", "POST", runPath+":start", "", `"1"`)
	sapServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer sapServer.Close()
	t.Setenv("ERP_TEST_SAP", "user:secret")
	e.Must(200, "PATCH", path, fmt.Sprintf(`{"settings": {"base_url": %q, "client": "100", "page_size": 10}}`, sapServer.URL), `"5"`)
	e.Problem(503, "platform.unavailable", "POST", runPath+":start", "", `"1"`)
	if got, _ := e.Must(200, "GET", runPath, "", ""); got["status"] != "queued" {
		t.Fatalf("after a failed extraction: %v", got)
	}
	// Credentials in a vault: the server's resolver reads them; a reference to nothing is 422.
	vault := vaulttest.New(t)
	vault.Secrets["secret/erp/sap"] = map[string]any{"credentials": "user:secret"}
	local := secrets.Local{}
	withVault := apitest.NewWith(t, func(cfg *httpapi.Config) {
		cfg.Secrets = secrets.Multi{secrets.SchemeEnv: local, secrets.SchemeVault: &secrets.Vault{Address: vault.URL, Token: vaulttest.Token}}
	})
	vaulted := apitest.Client{T: t, API: withVault, Tenant: e.Tenant, As: e.Tenant.Human}
	_, tag = vaulted.Must(200, "PATCH", path, `{"secret_ref": "vault:secret/erp/sap#credentials"}`, `"6"`)
	vaulted.Problem(503, "platform.unavailable", "POST", runPath+":start", "", `"1"`) // resolved; SAP answers 503
	_, tag = vaulted.Must(200, "PATCH", path, `{"secret_ref": "vault:secret/erp/sap#missing"}`, tag)
	vaulted.Problem(422, "integration.secret_unavailable", "POST", runPath+":start", "", `"1"`)
	e.Must(200, "PATCH", path, `{"secret_ref": "env:ERP_TEST_SAP"}`, tag)

	// Runs need a source connector, a known mode and active legal entities the caller may act in.
	mail := ""
	for _, it := range page["items"].([]any) {
		mail = it.(map[string]any)["id"].(string)
	}
	inactive := "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d99"
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Dormant', 'DE', 'EUR', gen_random_uuid(), 'inactive', $3, $3)`, e.Tenant.ID, inactive, e.Tenant.Human.ID)
	for _, c := range []struct {
		name, connector, mode, scope, code string
	}{
		{"unknown mode", c["id"].(string), "full", scope(e.DE.String()), "integration.unknown_mode"},
		{"unknown connector", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d98", "documents", scope(e.DE.String()), "request.reference_not_found"},
		{"not a source", mail, "documents", scope(e.DE.String()), "integration.not_a_source"},
		{"unknown legal entity", c["id"].(string), "documents", scope("01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d97"), "request.reference_not_found"},
		{"inactive legal entity", c["id"].(string), "documents", scope(inactive), "integration.legal_entity_not_active"},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := e.Client
			client.T = t
			client.Problem(422, c.code, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": %s, "mode": %q, "is_rehearsal": false}`,
				c.connector, c.scope, c.mode), "")
		})
	}
	// Without an extract directory, sap_file connectors cannot be read; a missing file is an extraction failure.
	plain := env{Env: e.Env}
	plain.API = apitest.New(t)
	fileRun := plain.run(t, plain.connector(t, "missing.json"), "documents", false, e.DE.String())
	plain.Problem(503, "platform.unavailable", "POST", "/v1/migration-runs/"+fileRun["id"].(string)+":start", "", `"1"`)
	missing := e.run(t, e.connector(t, "nowhere.json"), "documents", false, e.DE.String())
	e.Problem(503, "platform.unavailable", "POST", "/v1/migration-runs/"+missing["id"].(string)+":start", "", `"1"`)
	if _, err := os.Stat(filepath.Join(e.dir, "nowhere.json")); err == nil {
		t.Fatal("unexpected file")
	}
	e.Problem(404, "request.not_found", "GET", "/v1/migration-runs/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d96", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/connectors/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d96", "", "")
}

// TestClearingOutsideScope loads a payment cleared by a document outside
// the extract, from a connector naming its source system.
func TestClearingOutsideScope(t *testing.T) {
	e := setup(t)
	x := tiny()
	x.ACDOCA[1].AUGBL, x.ACDOCA[3].AUGBL = "0100000009", "0100000009"
	x.EKPO[0].PEINH = ""
	write(t, e.dir, "tiny.json", x)
	c, _ := e.Must(201, "POST", "/v1/connectors", `{"kind": "sap_file", "name": "S4", "settings": {"path": "tiny.json", "system": "S4_PRD"}}`, "")
	run := e.run(t, c["id"].(string), "documents", false, e.DE.String())
	_, got := e.act(t, run, "start")
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	if clearings := got["report"].(map[string]any)["loaded"].(map[string]any)["clearings"]; clearings != float64(1) {
		t.Fatalf("start loaded clearings: got %v, want 1", clearings)
	}
	if n := count(t, `SELECT count(*) FROM integration.external_ref WHERE tenant_id = $1 AND system = 'S4_PRD'`, e.Tenant.ID); n == 0 {
		t.Fatalf("external refs of S4_PRD: got 0, want some")
	}
	if _, got = e.act(t, run, "reconcile"); got["status"] != "reconciled" {
		t.Fatalf("reconcile: %v", got["report"])
	}
	// Runs are visible only to those who may act in every legal entity of their scope.
	e.As(e.Tenant.Agent).Problem(404, "request.not_found", "GET", "/v1/migration-runs/"+run["id"].(string), "", "")

	// Opening entries need a journal_ob number range in the fiscal year.
	next, _ := e.Must(201, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": {"legal_entity_ids": [%q], "fiscal_year": 2027},
		"mode": "balances", "is_rehearsal": false}`, c["id"], e.DE), "")
	if _, got = e.act(t, next, "start"); got["status"] != "failed" || !strings.Contains(fmt.Sprint(got["report"]), "no journal_ob number range") {
		t.Fatalf("no range: %v", got["report"])
	}
}

var errMismatch = fmt.Errorf("version mismatch")
