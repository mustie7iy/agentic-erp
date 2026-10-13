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

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"payments.run.read", "payments.run.propose", "payments.run.release", "payments.file.transmit",
	"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "ledger.entry.read", "ledger.entry.post",
	"platform.operation.read", "platform.operation.cancel"}

// connector records the files it transmits, or fails with err.
type connector struct {
	mu    sync.Mutex
	files []payments.File
	err   error
}

func (c *connector) Transmit(_ context.Context, f payments.File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	c := &connector{}
	f.API = apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Connector = c })
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), connector: c}
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, f.DE).Scan(&e.bank); err != nil {
		t.Fatal(err)
	}
	return e
}

// endpoints is the payments context as the API wires it.
func endpoints(c payments.Connector) *payments.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return httpapi.Payments(httpapi.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), Connector: c})
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
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, supplier, ref, date, gross, e.expense, gross), "")
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
		out[m["supplier_id"].(string)] = m["reason_code"].(string) + " " + m["amount"].(map[string]any)["amount"].(string)
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
