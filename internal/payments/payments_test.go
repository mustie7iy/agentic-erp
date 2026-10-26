package payments_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"payments.run.read", "payments.run.propose", "payments.run.release", "payments.file.transmit",
	"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "ledger.entry.read", "ledger.entry.post", "ledger.clearing.post",
	"platform.operation.read", "platform.operation.cancel"}

// connector records the files it transmits, or fails with err; during,
// when set, runs while the bank has the file.
type connector struct {
	mu     sync.Mutex
	files  []payments.File
	err    error
	during func()
}

func (c *connector) Transmit(_ context.Context, f payments.File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.during != nil {
		c.during()
	}
	if c.err != nil {
		return c.err
	}
	c.files = append(c.files, f)
	return nil
}

type env struct {
	fixture.Env
	bank      uuid.UUID // house bank MAIN of company 2000
	expense   uuid.UUID // account 520000 of company 2000
	connector *connector
	objects   blobstore.Store // the API's object store, when a test gives it one
	configure func(*app.Config)
}

func setup(t *testing.T) env {
	t.Helper()
	return setupWith(t, nil, func(*app.Config) {})
}

// setupWith is setup with more permissions and API wiring.
func setupWith(t *testing.T, more []string, configure func(*app.Config)) env {
	t.Helper()
	return setupIn(t, fixture.New(t, append(append([]string{}, perms...), more...)...), configure)
}

// setupIn is setupWith on the tenant f.
func setupIn(t *testing.T, f fixture.Env, configure func(*app.Config)) env {
	t.Helper()
	leavesNoProposal(t, f.Tenant.ID)
	c := &connector{}
	f.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Connector = c
		configure(cfg)
	})
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), connector: c, configure: configure}
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, f.DE).Scan(&e.bank); err != nil {
		t.Fatal(err)
	}
	return e
}

// in returns e reporting to t, so a subtest's failure stops that subtest
// rather than its parent.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.T = t
	return e
}

// endpoints is the payments context as the API wires it.
func endpoints(c payments.Connector) *payments.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return app.Payments(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), Connector: c})
}

// proposer is the worker on the package database.
func proposer(t *testing.T) *payments.Proposer {
	t.Helper()
	return &payments.Proposer{Endpoints: endpoints(nil), Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
}

// releaser is the in-process release worker on the package database, wired
// like the API (its connector and object store).
func (e env) releaser(t *testing.T) *workflows.Releaser {
	t.Helper()
	h := endpoints(e.connector)
	h.Objects = e.objects
	return &workflows.Releaser{Payments: &workflows.Payments{Pool: pgtest.Pool(t), AppRole: "erp_app", Endpoints: h}, Pool: pgtest.Pool(t), WorkerRole: "erp_worker"}
}

// transmitter is the bank file worker on the package database, wired like
// the API (its connector, channels and object store).
func (e env) transmitter(t *testing.T) *payments.Transmitter {
	t.Helper()
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	cfg := app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), Connector: e.connector}
	if e.configure != nil {
		e.configure(&cfg)
	}
	h := app.Payments(cfg)
	h.Objects = e.objects
	return &payments.Transmitter{Endpoints: h, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
}

// transmit asks for the file's transmission (202, an operation) and lets
// the transmitter do it; it returns the operation as asked for.
func (e env) transmit(t *testing.T, file, ifMatch string) map[string]any {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/bank-files/"+file+":transmit", "", ifMatch)
	// The transmitter takes every tenant's due files: none of this test's
	// is left queued for a later test's, whose clock may be ahead.
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE payments.bank_file SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE id = $1 AND status IN ('to_send', 'sending')`, file)
	})
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("transmitter: %v", err)
	}
	return op
}

// failedWith checks that the transmission op asked for failed with code,
// and its file with it.
func (e env) failedWith(t *testing.T, op map[string]any, code string) {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if got["status"] != "failed" {
		t.Fatalf("transmit operation status: got %v, want failed", got["status"])
	}
	if p, _ := got["error"].(map[string]any); p["code"] != code {
		t.Errorf("transmit operation error: got %v, want %s", got["error"], code)
	}
	file, _ := e.Must(200, "GET", got["resource"].(string), "", "")
	if file["status"] != "failed" {
		t.Errorf("file status: got %v, want failed", file["status"])
	}
}

// release asks for the run's release as the approver (202, an operation),
// lets the release worker do it and returns the run as released.
func (e env) release(t *testing.T, run map[string]any, body string) map[string]any {
	t.Helper()
	op := e.decide(t, 202, run, "release", body)
	if op["kind"] != "payments.release" {
		t.Fatalf("release operation kind: got %v, want payments.release", op["kind"])
	}
	if op["status"] != "queued" {
		t.Fatalf("release operation status: got %v, want queued", op["status"])
	}
	if want := "/v1/payment-runs/" + id(run); op["resource"] != want {
		t.Fatalf("release operation resource: got %v, want %s", op["resource"], want)
	}
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if got["status"] != "released" {
		t.Fatalf("released run status: got %v, want released", got["status"])
	}
	if done["status"] != "succeeded" {
		t.Fatalf("release operation status: got %v, want succeeded: %v", done["status"], done)
	}
	if got["release_operation_id"] != id(op) {
		t.Fatalf("released run operation: got %v, want %s", got["release_operation_id"], id(op))
	}
	return got
}

// releaseFails asks for the run's release and lets the worker fail it with
// the problem code expected; it returns the operation and the run after.
func (e env) releaseFails(t *testing.T, run map[string]any, code string) (op, after map[string]any) {
	t.Helper()
	op = e.decide(t, 202, run, "release", "")
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	op, _ = e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if op["status"] != "failed" {
		t.Fatalf("release operation after the failure, status: got %v, want failed: %v", op["status"], op)
	}
	if got := op["error"].(map[string]any)["code"]; got != code {
		t.Fatalf("release operation after the failure, error: got %v, want %s", got, code)
	}
	after, _ = e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	return op, after
}

// work runs the proposal worker until no job is left.
func work(t *testing.T) int {
	t.Helper()
	n, err := proposer(t).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// workLeftovers has the proposal worker finish, when the test ends, the
// jobs a test leaves queued on purpose. The worker takes the jobs of every
// tenant in the package database, so a job left behind would be done by the
// next test's worker and counted as its own: TestProposer's failing rounds
// would fail on it, and a test that wants one job done would see two.
func workLeftovers(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { work(t) })
}

// leavesNoProposal fails the test if, when it ends, the tenant still has a
// proposal job, and has the worker do it. The worker takes the jobs of every
// tenant in the package database, so a job left behind is done by the next
// test that runs it and counted as that test's own: T54f's "worker did 2
// jobs" in TestProposeReleaseTransmit was a job TestDownPaymentDatabaseFailures
// left before it had workLeftovers. Registered before the test's own
// cleanups (the API's and workLeftovers), the check runs after them: keep
// the call ahead of apitest.NewWith in setupIn.
func leavesNoProposal(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		var left int
		pgtest.Scan(t, `SELECT count(*) FROM platform.operation o
			JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.tenant_id = $1 AND o.kind = $2 AND o.status IN ('queued', 'cancelled') AND r.status = 'proposing'`,
			[]any{tenant, payments.ProposeKind}, &left)
		if left > 0 {
			t.Errorf("proposal jobs left for the next test's worker: got %d, want 0 (finish them, or call workLeftovers)", left)
			work(t)
		}
	})
}

// bankAccount gives the supplier a payments bank account valid from the
// date to validTo ("" for open-ended), introduced by a change of the kind
// (create or bank_change).
func (e env) bankAccount(t *testing.T, supplier uuid.UUID, iban, holder, validFrom, validTo, kind string) uuid.UUID {
	t.Helper()
	change, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at,
		created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '{}', 'test', 'applied', $6, now(), $6, $6)`,
		e.Tenant.ID, change, change.String(), kind, supplier, e.Tenant.Human.ID)
	var to *string
	if validTo != "" {
		to = &validTo
	}
	pgtest.Exec(t, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
		bank_country_code, holder_name, valid_from, valid_to, partner_change_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'iban', 'COBADEFFXXX', 'DE', $7, $8, $9, $10, $11)`, e.Tenant.ID, id, supplier, apitest.Seal(t, e.Tenant.ID, iban),
		apitest.Fingerprint(t, e.Tenant.ID, iban), iban[len(iban)-4:], holder, validFrom, to, change, e.Tenant.Human.ID)
	return id
}

// invoice enters and posts an invoice of the supplier in company 2000 and
// returns it.
func (e env) invoice(t *testing.T, supplier uuid.UUID, ref, gross, date string) map[string]any {
	t.Helper()
	return e.document(t, "invoice", supplier, ref, gross, date)
}

// document enters and posts a supplier document of the kind (invoice,
// credit_memo) in company 2000, on the expense account, and returns it.
func (e env) document(t *testing.T, kind string, supplier uuid.UUID, ref, gross, date string) map[string]any {
	t.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "kind": %q, "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, supplier, ref, date, kind, gross, e.expense, gross), "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", fmt.Sprintf(`{"posting_date": %q}`, date), `"1"`)
	return posted
}

// credit posts a credit memo of the supplier in company 2000: Dr payables,
// Cr expense.
func (e env) credit(t *testing.T, supplier uuid.UUID, amount, date string) map[string]any {
	t.Helper()
	je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR",
		"reference": "CM-1", "lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}, "partner_id": %q},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}}]}`,
		e.DE, date, date, e.Account(t, "2000", "211000"), amount, supplier, e.expense, amount), "")
	return je
}

// propose asks for a run of company 2000 and returns its operation.
func (e env) propose(t *testing.T, runDate, dueBy string) map[string]any {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": %q, "due_by": %q}`,
		e.DE, e.bank, runDate, dueBy), "")
	return op
}

// run proposes and builds a run and returns it.
func (e env) run(t *testing.T, runDate, dueBy string) map[string]any {
	t.Helper()
	op := e.propose(t, runDate, dueBy)
	work(t)
	r, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	return r
}

func (e env) payments(t *testing.T, run map[string]any) []any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run)+"/payments", "", "")
	return page["items"].([]any)
}

func (e env) exclusions(t *testing.T, run map[string]any) map[string]string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run)+"/exclusions", "", "")
	out := map[string]string{}
	for _, x := range page["items"].([]any) {
		m := x.(map[string]any)
		key := m["supplier_id"].(string)
		if r, ok := m["down_payment_request_id"].(string); ok {
			key = "request " + r // a request's own exclusion (T46c)
		}
		out[key] = m["reason_code"].(string) + " " + m["amount"].(map[string]any)["amount"].(string)
	}
	return out
}

// decide releases or rejects a run as the approver.
func (e env) decide(t *testing.T, code int, run map[string]any, action, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	got, _ := e.As(e.Approver).Must(code, "POST", "/v1/payment-runs/"+id(run)+":"+action, body, tag)
	return got
}

func id(m map[string]any) string { return m["id"].(string) }

func uuidOf(s string) uuid.UUID { return uuid.MustParse(s) }

func events(t *testing.T, ten pgtest.Tenant, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, ten, outbox.Event{Context: "payments", Name: name})
}

var (
	errBank     = errors.New("bank unreachable")
	errMismatch = errors.New("version mismatch")
)

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}
