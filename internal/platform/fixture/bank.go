package fixture

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// StatementPermissions are the permissions of the people who reconcile
// bank statements, and of the reconciliation agent that proposes for
// their lines (ADR-0047): reading, importing, matching and posting
// statements, and proposing in the agent's sessions.
var StatementPermissions = []string{"payments.statement.read", "payments.statement.import", "payments.statement.match",
	"payments.statement.post", "payments.statement.propose", "agents.session.record", "agents.session.read"}

// BankRules are company 2000's bank statement rules in the tests: charges
// (NCHG) post to 520000; lines of up to 50.00 post at once, larger ones
// after the controller's approval; the four matching rules in their
// usual order.
const BankRules = `{"posting_rules": [{"transaction_code": "NCHG", "description": "Bank charges", "account": "520000"}],
	"approval_tiers": [{"up_to": "50.00"}, {"steps": [{"role": "controller", "approvals": 1}]}],
	"matching_rules": [{"kind": "end_to_end_id"}, {"kind": "batch_id"}, {"kind": "transaction_code"},
	{"kind": "amount", "amount_tolerance": "0.50", "date_window_days": 3, "counterparty": true}]}`

// HouseBank is house bank MAIN of company 2000 (DE89370400440532013000,
// EUR), as the configuration made it.
func (e Env) HouseBank(tb testing.TB) uuid.UUID {
	tb.Helper()
	var id uuid.UUID
	pgtest.Scan(tb, `SELECT id FROM payments.house_bank WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{e.Tenant.ID, e.DE}, &id)
	return id
}

// Reconciles makes house bank MAIN of company 2000 one whose statements
// post, as an activated configuration would (ADR-0047): BankRules active,
// its main bank account 101000, its clearing account open-item managed,
// its incoming payments clearing account 140000, and the tenant's agent
// in every company, to propose.
func (e Env) Reconciles(tb testing.TB) {
	tb.Helper()
	pgtest.Exec(tb, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, jsonb_set(v.document, '{companies,1,bank_statements}', $2::jsonb), v.hash, 'rollback',
			v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		e.Tenant.ID, BankRules)
	pgtest.Exec(tb, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	bank := e.HouseBank(tb)
	pgtest.Exec(tb, `UPDATE payments.house_bank SET main_gl_account_id = $2, incoming_gl_account_id = $3, version = version + 1 WHERE id = $1`,
		bank, e.Account(tb, "2000", "101000"), e.Account(tb, "2000", "140000"))
	pgtest.Exec(tb, `UPDATE ledger.company_account ca SET is_open_item_managed = true, version = ca.version + 1 FROM payments.house_bank h
		WHERE h.id = $1 AND ca.legal_entity_id = h.legal_entity_id AND ca.account_id = h.gl_account_id`, bank)
	e.Tenant.AllCompanies(tb, e.Tenant.Agent)
}

// MT940 is an MT940 statement of house bank MAIN of company 2000 with the
// balances (amounts with a decimal comma, dates YYMMDD) and the :61:
// lines (each with its :86: when given).
func MT940(ref, seq, opening, openDate, closing, closeDate string, lines ...string) string {
	return ":20:" + ref + "\n:25:DE89370400440532013000\n:28C:" + seq + "\n:60F:C" + openDate + "EUR" + opening + "\n" +
		strings.Join(lines, "") + ":62F:C" + closeDate + "EUR" + closing + "\n"
}

// ImportStatement imports the MT940 file for house bank MAIN, as the
// tenant's human, through an API that keeps statement files in objects
// (the store the test's own API reads them from), and returns the
// statements' ids.
func (e Env) ImportStatement(tb testing.TB, objects blobstore.Store, file string) []uuid.UUID {
	tb.Helper()
	api := apitest.NewWith(tb, func(cfg *app.Config) { cfg.Objects = objects })
	part := func(name, value string) string {
		return "--b\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + value + "\r\n"
	}
	r := api.Do(apitest.Req{Method: "POST", Path: "/v1/bank-statements", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}},
		Body:   part("house_bank_id", e.HouseBank(tb).String()) + part("format", "mt940") + part("file", file) + "--b--\r\n"})
	body := apitest.JSON(tb, r)
	if r.Code != 201 {
		tb.Fatalf("import: got %d %v, want 201", r.Code, body)
	}
	var out []uuid.UUID
	for _, s := range body["statements"].([]any) {
		out = append(out, uuid.MustParse(s.(map[string]any)["id"].(string)))
	}
	return out
}

// Propose has the tenant's agent propose, in a running session of its
// own, for the statement's line: body is what it proposes and why
// (`"exclude": true, "reason": …` or `"posting_rule": …, "reason": …`).
func (e Env) Propose(tb testing.TB, statement uuid.UUID, line int, body string) {
	tb.Helper()
	agent := e.As(e.Tenant.Agent)
	session, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Reconcile statement lines", "model": "erp-default"}`, "")
	agent.Must(201, "POST", fmt.Sprintf("/v1/bank-statements/%s/lines/%d:propose", statement, line),
		fmt.Sprintf(`{"session_id": %q, %s}`, session["id"], body), "*")
}

// OpeningBalance posts what house bank MAIN held before its first
// statement to its main bank account 101000 on 15 April 2026, against
// expense account 520000, as a company's opening balance would.
func (e Env) OpeningBalance(tb testing.TB, amount money.Minor) {
	tb.Helper()
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	text, day := "Opening balance", time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day,
			Currency: "EUR", HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.Must(uuid.NewV7()),
			Lines: []ledger.PostingLine{{AccountID: e.Account(tb, "2000", "101000"), Amount: amount}, {AccountID: e.Account(tb, "2000", "520000"), Amount: -amount}}})
		return err
	}); err != nil {
		tb.Fatal(err)
	}
}
