package payments_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestCancelledAndFailedProposals(t *testing.T) {
	e := setup(t)
	op := e.propose(t, "2026-04-15", "2026-04-15")
	e.Must(200, "POST", "/v1/operations/"+id(op)+":cancel", "", `"1"`)
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["status"] != "failed" {
		t.Fatalf("cancelled: %v", run)
	}
	failed := events(t, e.Tenant, "payment_run_failed")
	if len(failed) != 1 || failed[0]["previous_status"] != "proposing" || failed[0]["released_item_count"] != float64(0) || failed[0]["operation_id"] != id(op) {
		t.Fatalf("failed events: %v", failed)
	}

	// A problem while building fails the run and its operation.
	op = e.propose(t, "2026-04-15", "2026-04-15")
	pgtest.Exec(t, `DELETE FROM config.active_version WHERE tenant_id = $1`, e.Tenant.ID)
	work(t)
	run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if run["status"] != "failed" || done["status"] != "failed" || done["error"].(map[string]any)["code"] != "config.not_bootstrapped" {
		t.Fatalf("failed: %v %v", run, done)
	}
	e.decide(t, 409, run, "release", "")
}

func TestProposeValidation(t *testing.T) {
	e := setup(t)
	body := func(le, bank uuid.UUID, runDate string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": %q, "due_by": %q}`, le, bank, runDate, runDate)
	}
	var usBank uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, e.US).Scan(&usBank); err != nil {
		t.Fatal(err)
	}
	inactive := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Dormant', 'DE', 'EUR', gen_random_uuid(), 'inactive', $3, $3)`, e.Tenant.ID, inactive, e.Tenant.Human.ID)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown legal entity", body(uuid.New(), e.bank, "2026-04-15"), 422, "request.reference_not_found"},
		{"inactive legal entity", body(inactive, e.bank, "2026-04-15"), 422, "payments.legal_entity_not_active"},
		{"other legal entity's bank", body(e.DE, usBank, "2026-04-15"), 422, "request.reference_not_found"},
		{"unknown bank", body(e.DE, uuid.New(), "2026-04-15"), 422, "request.reference_not_found"},
		{"no fiscal year", body(e.DE, e.bank, "2031-01-15"), 422, "ledger.no_fiscal_period"},
	}
	for _, c := range cases {
		if got := e.Problem(c.status, c.code, "POST", "/v1/payment-runs:propose", c.body, ""); t.Failed() {
			t.Fatalf("%s: %v", c.name, got)
		}
	}
	pgtest.Exec(t, `UPDATE payments.house_bank SET status = 'inactive', version = version + 1 WHERE id = $1`, e.bank)
	e.Problem(422, "payments.house_bank_inactive", "POST", "/v1/payment-runs:propose", body(e.DE, e.bank, "2026-04-15"), "")
	e.Problem(404, "request.not_found", "GET", "/v1/payment-runs/"+uuid.NewString(), "", "")
}

func TestForeignExchange(t *testing.T) {
	e := setup(t)
	rate := func(from, rate string) {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', $2, $3, 'ECB', $4)`, e.Tenant.ID, from, rate, e.Tenant.Human.ID)
	}
	rate("2026-01-01", "0.90")
	rate("2026-04-01", "0.95")
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "US-1",
		"invoice_date": "2026-03-01", "gross_amount": {"amount": "100.00", "currency": "USD"}, "tax_amount": {"amount": "0.00", "currency": "USD"},
		"items": [{"account_id": %q, "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "100.00", "currency": "USD"}}]}`,
		e.US, e.Supplier, e.Account(t, "1000", "520000")), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
	var bank uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, e.US).Scan(&bank); err != nil {
		t.Fatal(err)
	}
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.US, bank), "")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	// The invoice is booked at 90.00 EUR and paid at 95.00 EUR: a loss in group currency, which needs an account.
	// The release operation fails with the ledger's problem; nothing was posted, so the run is proposed again.
	if _, after := e.releaseFails(t, run, "ledger.no_fx_account"); after["status"] != "proposed" || after["decided_by"] != nil {
		t.Fatalf("after the failed release: %v", after)
	}
	loss := e.Account(t, "1000", "510000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
		e.Tenant.ID, e.US, loss, e.Tenant.Human.ID)
	run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
	e.release(t, run, "")
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+pay["journal_entry_id"].(string), "", "")
	lines := je["lines"].([]any)
	fx := lines[2].(map[string]any)
	if len(lines) != 3 || fx["account_id"] != loss.String() || fx["amount"].(map[string]any)["amount"] != "0.00" ||
		fx["amount_gc"].(map[string]any)["amount"] != "5.00" || lines[1].(map[string]any)["amount_gc"].(map[string]any)["amount"] != "-95.00" {
		t.Fatalf("payment entry: %v", lines)
	}
}

func TestTransmitFailures(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	path := "/v1/bank-files/" + id(file) + ":transmit"

	// Without a connector nothing is transmitted.
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints(nil).TransmitFile(ctx, uuidOf(id(file)), func(int32) error { return nil })
		return err
	})
	if p := problem.From(err); p.Status != 503 {
		t.Fatalf("no connector: %v", err)
	}
	e.connector.err = errBank
	e.Problem(503, "platform.unavailable", "POST", path, "", `"1"`)
	e.connector.err = nil
	e.Problem(412, "request.version_mismatch", "POST", path, "", `"9"`)
	// A run whose data no longer renders the recorded file is not transmitted.
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = $2, version = version + 1 WHERE id = $1`, e.bank,
		apitest.Seal(t, e.Tenant.ID, "DE02120300000000202051"))
	e.Problem(409, "payments.bank_file_changed", "POST", path, "", `"1"`)
	// Bank details that cannot be opened are an internal error.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET account_cipher = '\x00' WHERE partner_id = '%s'; END $$`, e.Supplier))
	e.Problem(500, "platform.internal", "POST", path, "", `"1"`)
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = '\x00', version = version + 1 WHERE id = $1`, e.bank)
	e.Problem(500, "platform.internal", "POST", path, "", `"1"`)
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = $2, version = version + 1 WHERE id = $1`, e.bank,
		apitest.Seal(t, e.Tenant.ID, "DE89370400440532013000"))
	if got, _ := e.Must(200, "GET", "/v1/bank-files/"+id(file), "", ""); got["status"] != "created" || len(e.connector.files) != 0 {
		t.Fatalf("file after failures: %v %v", got, e.connector.files)
	}

	// A file the bank format refuses stops the release.
	e.bankAccount(t, e.Vendor, "DE89370400440532013000", " ", "2026-01-01", "", "create")
	e.invoice(t, e.Vendor, "V-1", "10.00", "2026-03-01")
	second := e.run(t, "2026-04-16", "2026-04-16")
	// The payment posts, then the file is refused: the operation fails and the run stays releasing with its posting, to be resumed once the data is fixed.
	if _, after := e.releaseFails(t, second, "payments.bank_file_invalid"); after["status"] != "releasing" {
		t.Fatalf("after the refused file: %v", after)
	}
	if pay := e.payments(t, second)[0].(map[string]any); pay["journal_entry_id"] == nil {
		t.Fatalf("posting undone: %v", pay)
	}
	// Resumed after the fix: the posted payment is left alone, the file is written.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET holder_name = 'Vendor' WHERE partner_id = '%s'; END $$`, e.Vendor)) // the data fixed behind the immutability rule, as a correction would
	second, _ = e.Must(200, "GET", "/v1/payment-runs/"+id(second), "", "")
	released := e.release(t, second, "")
	if e.payments(t, released)[0].(map[string]any)["journal_entry_id"] == nil {
		t.Fatal("payment posted twice or lost")
	}
}

func TestListsAndAccess(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.invoice(t, e.Vendor, "INV-2", "50.00", "2026-03-01")
	first := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, first, "")
	none := e.NewSupplier(t, "BP90000003")
	e.invoice(t, none, "INV-3", "5.00", "2026-03-01")
	e.invoice(t, e.NewSupplier(t, "BP90000004"), "INV-4", "6.00", "2026-03-01")
	second := e.run(t, "2026-04-20", "2026-04-20")

	list := func(path string) map[string]any {
		t.Helper()
		page, _ := e.Must(200, "GET", path, "", "")
		return page
	}
	ids := func(page map[string]any) []string {
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, id(it.(map[string]any)))
		}
		return out
	}
	page := list("/v1/payment-runs?page_size=1")
	if fmt.Sprint(ids(page)) != fmt.Sprint([]string{id(first)}) || page["next_page_token"] == nil {
		t.Fatalf("first page: %v", page)
	}
	page = list("/v1/payment-runs?page_size=1&page_token=" + url.QueryEscape(page["next_page_token"].(string)))
	if fmt.Sprint(ids(page)) != fmt.Sprint([]string{id(second)}) || page["next_page_token"] != nil {
		t.Fatalf("second page: %v", page)
	}
	if got := ids(list("/v1/payment-runs?order_by=document_number%20desc&page_size=1")); fmt.Sprint(got) != fmt.Sprint([]string{id(second)}) {
		t.Fatalf("by number: %v", got)
	}
	page = list("/v1/payment-runs?order_by=document_number&page_size=1")
	if page["next_page_token"] == nil {
		t.Fatalf("by number, first page: %v", page)
	}
	if got := ids(list(fmt.Sprintf("/v1/payment-runs?status=released&legal_entity_id=%s&document_number=%s&run_date_from=2026-04-01&run_date_to=2026-04-15",
		e.DE, first["document_number"]))); fmt.Sprint(got) != fmt.Sprint([]string{id(first)}) {
		t.Fatalf("filtered: %v", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/payment-runs?order_by=total", "", "")

	pays := list("/v1/payment-runs/" + id(first) + "/payments?page_size=1")
	if len(pays["items"].([]any)) != 1 || pays["next_page_token"] == nil {
		t.Fatalf("payments page: %v", pays)
	}
	rest := list("/v1/payment-runs/" + id(first) + "/payments?page_size=1&page_token=" + url.QueryEscape(pays["next_page_token"].(string)))
	if len(rest["items"].([]any)) != 1 || rest["next_page_token"] != nil {
		t.Fatalf("payments second page: %v", rest)
	}
	if got := list("/v1/payment-runs/" + id(first) + "/payments?supplier_id=" + e.Vendor.String()); len(got["items"].([]any)) != 1 {
		t.Fatalf("payments of the vendor: %v", got)
	}
	exs := list("/v1/payment-runs/" + id(second) + "/exclusions?page_size=1")
	if len(exs["items"].([]any)) != 1 || exs["next_page_token"] == nil {
		t.Fatalf("exclusions page: %v", exs)
	}
	if got := list("/v1/bank-files?status=created&page_size=1"); len(got["items"].([]any)) != 1 || got["next_page_token"] != nil {
		t.Fatalf("bank files: %v", got)
	}
	if got := list("/v1/bank-files?status=transmitted"); len(got["items"].([]any)) != 0 {
		t.Fatalf("transmitted files: %v", got)
	}
	file := list("/v1/bank-files")["items"].([]any)[0].(map[string]any)

	// Two bank files page by creation time.
	e.invoice(t, e.Supplier, "INV-5", "7.00", "2026-03-01")
	third := e.run(t, "2026-04-21", "2026-04-21")
	e.release(t, third, "")
	files := list("/v1/bank-files?page_size=1")
	if files["next_page_token"] == nil {
		t.Fatalf("files page: %v", files)
	}
	if more := list("/v1/bank-files?page_size=1&page_token=" + url.QueryEscape(files["next_page_token"].(string))); len(more["items"].([]any)) != 1 {
		t.Fatalf("files second page: %v", more)
	}

	// People without access to company 2000 see none of it.
	us := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	us.Subject = "human|" + us.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, 'US only', 'active', $2, $2)`, e.Tenant.ID, us.ID, us.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, e.Tenant.ID, us.ID)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.Tenant.ID, us.ID, e.US)
	other := e.As(us)
	for _, path := range []string{"/v1/payment-runs/" + id(first), "/v1/payment-runs/" + id(first) + "/payments",
		"/v1/payment-runs/" + id(first) + "/exclusions", "/v1/bank-files/" + id(file)} {
		other.Problem(404, "request.not_found", "GET", path, "", "")
	}
	for _, path := range []string{"/v1/payment-runs", "/v1/bank-files"} {
		if got, _ := other.Must(200, "GET", path, "", ""); len(got["items"].([]any)) != 0 {
			t.Errorf("%s: %v", path, got)
		}
	}
	other.Problem(422, "request.reference_not_found", "POST", "/v1/payment-runs:propose",
		fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15", "due_by": "2026-04-15"}`, e.DE, e.bank), "")
}
