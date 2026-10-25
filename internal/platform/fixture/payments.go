package fixture

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
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// PaymentPermissions are the permissions of the people who propose,
// release and transmit payment runs (ADR-0038): the runs, their bank
// files, the operations that build and release them, and the invoices
// the runs pay.
var PaymentPermissions = []string{"payments.run.read", "payments.run.propose", "payments.run.release", "payments.file.transmit",
	"platform.operation.read", "platform.operation.cancel", "payables.invoice.read", "payables.invoice.capture", "payables.invoice.post",
	"ledger.entry.read"}

// BankAccount gives the supplier a payments bank account (IBAN, BIC
// COBADEFFXXX) valid from 1 January 2026, as an applied partner change
// that created it would, and returns its id.
func (e Env) BankAccount(tb testing.TB, supplier uuid.UUID, iban, holder string) uuid.UUID {
	tb.Helper()
	change, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, applied_at,
		created_by, updated_by) VALUES ($1, $2, $3, 'create', $4, '{}', 'test', 'applied', $5, now(), $5, $5)`,
		e.Tenant.ID, change, change.String(), supplier, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
		bank_country_code, holder_name, valid_from, partner_change_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'iban', 'COBADEFFXXX', 'DE', $7, '2026-01-01', $8, $9)`, e.Tenant.ID, id, supplier,
		apitest.Seal(tb, e.Tenant.ID, iban), apitest.Fingerprint(tb, e.Tenant.ID, iban), iban[len(iban)-4:], holder, change, e.Tenant.Human.ID)
	return id
}

// Payable enters and posts, as the tenant's human, an invoice of the
// supplier in company 2000 dated and posted 10 March 2026 on expense
// account 520000 with tax code V1 (19%): due on 9 April 2026 under NT30,
// so a run of 15 April pays it. It returns the invoice's id.
func (e Env) Payable(tb testing.TB, supplier uuid.UUID, ref, net, tax, gross string) uuid.UUID {
	tb.Helper()
	id := e.SupplierInvoice(tb, supplier, ref, gross, tax, e.AccountItem(tb, "Services", net))
	e.Must(200, "POST", "/v1/supplier-invoices/"+id.String()+":post", `{"posting_date": "2026-03-10"}`, `"1"`)
	return id
}

// Accepting is a bank's connector that takes every file at once, for an
// API and a transmitter that send files (app.Config.Connector).
type Accepting struct{}

// Transmit takes the file.
func (Accepting) Transmit(context.Context, payments.File) error { return nil }

// paymentEndpoints are the payments context as the API wires it, with the
// object store and a connector that takes every file.
func paymentEndpoints(tb testing.TB, objects blobstore.Store) *payments.Endpoints {
	tb.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		tb.Fatal(err)
	}
	return app.Payments(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), Connector: Accepting{}, Objects: objects})
}

// RunProposals has the proposal worker build every queued payment run
// on the package database, as the API's server does, and returns how
// many it built.
func RunProposals(tb testing.TB) int {
	tb.Helper()
	w := &payments.Proposer{Endpoints: paymentEndpoints(tb, nil), Pool: pgtest.Pool(tb), AppRole: "erp_app", WorkerRole: "erp_worker"}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		tb.Fatal(fmt.Errorf("proposal worker: %w", err))
	}
	return n
}

// RunReleases has the release worker do every queued release on the
// package database, keeping the bank files in objects (nil: none kept),
// and returns how many it did.
func RunReleases(tb testing.TB, objects blobstore.Store) int {
	tb.Helper()
	pool := pgtest.Pool(tb)
	w := &workflows.Releaser{Payments: &workflows.Payments{Pool: pool, AppRole: "erp_app", Endpoints: paymentEndpoints(tb, objects)}, Pool: pool,
		WorkerRole: "erp_worker"}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		tb.Fatal(fmt.Errorf("release worker: %w", err))
	}
	return n
}

// RunTransmissions has the bank file transmitter send every file due on
// the package database through Accepting, reading the files from
// objects, and returns how many it sent.
func RunTransmissions(tb testing.TB, objects blobstore.Store) int {
	tb.Helper()
	w := &payments.Transmitter{Endpoints: paymentEndpoints(tb, objects), Pool: pgtest.Pool(tb), AppRole: "erp_app", WorkerRole: "erp_worker"}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		tb.Fatal(fmt.Errorf("bank file transmitter: %w", err))
	}
	return n
}
