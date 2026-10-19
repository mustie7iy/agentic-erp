package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// methodSetup is downPaymentSetup with object storage, a second house bank
// of company 2000, DB, on its bank clearing account 101000, and payment
// methods (ADR-0038): T ranks DB before MAIN in EUR, S ranks MAIN in EUR,
// C ranks DB only in USD, and X, inactive, ranks MAIN in EUR.
func methodSetup(t *testing.T) (env, uuid.UUID) {
	t.Helper()
	store := blobstore.Dir{Root: t.TempDir()}
	e := downPaymentSetupWith(t, func(cfg *app.Config) { cfg.Objects = store })
	e.objects = store
	db := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id,
		active_gl_account_id, status, created_by, updated_by) VALUES ($1, $2, $3, 'DB', 'DB', $4, '6300', 'DEUTDEFFXXX', 'EUR', $5, $5, 'active', $6, $6)`,
		e.Tenant.ID, db, e.DE, apitest.Seal(t, e.Tenant.ID, "DE72500700100175526300"), e.Account(t, "2000", "101000"), e.Tenant.Human.ID)
	for _, m := range []struct {
		code, currency, banks string
		active                bool
	}{{"T", "EUR", "{DB,MAIN}", true}, {"S", "EUR", "{MAIN}", true}, {"C", "USD", "{DB}", true}, {"X", "EUR", "{MAIN}", false}} {
		pgtest.Exec(t, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
			VALUES ($1, $2, $3, $3, $4, $5, $5)`, e.Tenant.ID, e.DE, m.code, m.active, e.Tenant.Human.ID)
		pgtest.Exec(t, `INSERT INTO payments.bank_ranking (tenant_id, legal_entity_id, method, currency, house_banks, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $6)`, e.Tenant.ID, e.DE, m.code, m.currency, m.banks, e.Tenant.Human.ID)
	}
	return e, db
}

// methods sets the supplier's payment methods in company 2000.
func (e env) methods(t *testing.T, supplier uuid.UUID, methods string) {
	t.Helper()
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_methods = $3, version = version + 1 WHERE partner_id = $1 AND legal_entity_id = $2`,
		supplier, e.DE, methods)
}

// TestProposalPerBankAndMethod: a run proposed without a house bank pays
// each supplier, its items and its down payment requests, through the first
// of its payment methods that an active house bank ranked for the method
// and the run currency pays with (ADR-0038); a supplier none can pay is
// excluded with no_house_bank. The release posts each payment to its
// bank's account and writes one bank file per house bank and method, from
// that bank's account.
func TestProposalPerBankAndMethod(t *testing.T) {
	e, db := methodSetup(t)
	unpaid := e.NewSupplier(t, "BP90000007")
	for _, s := range []struct {
		id         uuid.UUID
		iban, name string
	}{{e.Supplier, "DE89370400440532013000", "Supplier"}, {e.Vendor, "DE02120300000000202051", "Vendor"}, {unpaid, "DE75512108001245126199", "Unpaid"}} {
		e.bankAccount(t, s.id, s.iban, s.name, "2026-01-01", "", "create")
		e.invoice(t, s.id, "INV-"+s.name, "100.00", "2026-03-01")
	}
	e.methods(t, e.Supplier, "{C,X,T,S}") // C ranks no bank in EUR and X is inactive: T, through DB
	e.methods(t, e.Vendor, "{S}")
	e.methods(t, unpaid, "{C}")
	request := e.request(t, e.Supplier, "50.00", "2026-03-15", "")
	refused := e.request(t, unpaid, "40.00", "2026-03-15", "")

	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.DE), "")
	job := payments.Job{RunID: uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):]), ProposedBy: e.Tenant.Human.ID, ProposerKind: "human"}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "build", func(ctx context.Context) error { return endpoints(nil).Process(ctx, job) })
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if _, ok := run["house_bank_id"]; ok {
		t.Errorf("run house_bank_id: got %v, want none", run["house_bank_id"])
	}
	main := e.bank.String()
	paid := map[string]string{}
	for _, p := range e.payments(t, run) {
		m := p.(map[string]any)
		paid[m["supplier_id"].(string)+" "+m["kind"].(string)] = fmt.Sprint(m["house_bank_id"], " ", m["payment_method"])
	}
	want := map[string]string{e.Supplier.String() + " items": db.String() + " T", e.Supplier.String() + " down_payment": db.String() + " T",
		e.Vendor.String() + " items": main + " S"}
	for k, w := range want {
		if paid[k] != w {
			t.Errorf("payment %s: got %q, want %q", k, paid[k], w)
		}
	}
	if len(paid) != len(want) {
		t.Errorf("payments: got %v, want %v", paid, want)
	}
	excluded := e.exclusions(t, run)
	for k, w := range map[string]string{unpaid.String(): "no_house_bank 100.00", "request " + id(refused): "no_house_bank 40.00"} {
		if excluded[k] != w {
			t.Errorf("exclusion %s: got %q, want %q", k, excluded[k], w)
		}
	}

	e.release(t, run, "")
	e.checkFiles(t, run, map[string]int{db.String() + " T": 2, main + " S": 1})
	for _, p := range e.payments(t, run) {
		m := p.(map[string]any)
		je, _ := e.Must(200, "GET", "/v1/journal-entries/"+m["journal_entry_id"].(string), "", "")
		bank := e.Account(t, "2000", "100000")
		if m["house_bank_id"] == db.String() {
			bank = e.Account(t, "2000", "101000")
		}
		if !strings.Contains(fmt.Sprint(je["lines"]), bank.String()) {
			t.Errorf("payment %s posting: got %v, want a line on %s", m["id"], je["lines"], bank)
		}
	}
	paidRequest, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(request), "", "")
	if paidRequest["status"] != "paid" {
		t.Errorf("request status: got %v, want paid", paidRequest["status"])
	}
}

// checkFiles checks the run's bank files: per "bank method", the number of
// payments the file pays, each from that bank's account, under a message
// id of its own, with a bank_file_created event each.
func (e env) checkFiles(t *testing.T, run map[string]any, want map[string]int) {
	t.Helper()
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	items := files["items"].([]any)
	if len(items) != len(want) {
		t.Fatalf("bank files: got %d, want %d", len(items), len(want))
	}
	ids := map[string]bool{}
	for _, f := range items {
		m := f.(map[string]any)
		key := fmt.Sprint(m["house_bank_id"], " ", m["payment_method"])
		r := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/bank-files/" + id(m) + "/content", Tenant: e.Tenant.ID, As: e.Tenant.Human})
		if r.Code != 200 {
			t.Fatalf("file %s content status: got %d, want 200: %s", key, r.Code, r.Body)
		}
		content := r.Body.String()
		if got := strings.Count(content, "<CdtTrfTxInf>"); got != want[key] {
			t.Errorf("file %s payments: got %d, want %d", key, got, want[key])
		}
		if suffix := "-" + m["payment_method"].(string) + "</MsgId>"; !strings.Contains(content, suffix) {
			t.Errorf("file %s message id: got none ending %s in %s", key, suffix, content)
		}
		ids[id(m)] = true
	}
	created := 0
	for _, ev := range events(t, e.Tenant, "bank_file_created") {
		if ids[ev["bank_file_id"].(string)] {
			created++
		}
	}
	if created != len(want) {
		t.Errorf("bank_file_created events: got %d, want %d", created, len(want))
	}
}

// TestProposalCurrency: a run without a house bank names a currency that
// exists; with one, it may only repeat the bank's.
func TestProposalCurrency(t *testing.T) {
	e := setup(t)
	for _, c := range []struct{ name, body, code string }{
		{"no bank, no currency", `"legal_entity_id": %q`, "payments.currency_required"},
		{"unknown currency", `"legal_entity_id": %q, "currency": "XXX"`, "request.reference_not_found"},
		{"another currency than the bank's", `"legal_entity_id": %q, "house_bank_id": %q, "currency": "USD"`, "payments.currency_mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []any{e.DE}
			if strings.Count(c.body, "%q") == 2 {
				args = append(args, e.bank)
			}
			got, _ := e.Must(422, "POST", "/v1/payment-runs:propose", "{"+fmt.Sprintf(c.body, args...)+`, "run_date": "2026-04-15", "due_by": "2026-04-15"}`, "")
			if got["code"] != c.code {
				t.Errorf("code: got %v, want %s", got["code"], c.code)
			}
		})
	}
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "currency": "EUR",
		"run_date": "2026-04-15", "due_by": "2026-04-15"}`, e.DE, e.bank), "")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["house_bank_id"] != e.bank.String() {
		t.Errorf("run house_bank_id: got %v, want %s", run["house_bank_id"], e.bank)
	}
}

// TestFileWithoutPayments: a bank file rendered again from its run (no
// object storage) whose house bank and method no longer match any of the
// run's payments is broken data, refused rather than rendered empty.
func TestFileWithoutPayments(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	pgtest.Exec(t, `UPDATE payments.bank_file SET house_bank_id = $2, payment_method = 'X', version = version + 1 WHERE payment_run_id = $1`,
		uuidOf(id(run)), e.bank)
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	path := "/v1/bank-files/" + id(files["items"].([]any)[0].(map[string]any)) + "/content"
	if r := e.API.Do(apitest.Req{Method: "GET", Path: path, Tenant: e.Tenant.ID, As: e.Tenant.Human}); r.Code != 500 {
		t.Errorf("content status: got %d, want 500: %s", r.Code, r.Body)
	}
}
